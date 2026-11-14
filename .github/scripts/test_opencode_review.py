import copy
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import textwrap
import threading
import unittest
from unittest.mock import patch

import opencode_review as review


ROOT = Path(__file__).resolve().parents[2]
SHA = "a" * 40
OTHER_SHA = "b" * 40
RUN_URL = "https://github.com/example/repo/actions/runs/123"


def archive_bytes(files, links=None):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as archive:
        root = tarfile.TarInfo("fixture")
        root.type = tarfile.DIRTYPE
        archive.addfile(root)
        for name, content in files.items():
            data = content.encode() if isinstance(content, str) else content
            member = tarfile.TarInfo(
                name if name.startswith("/") else "fixture/" + name
            )
            member.size = len(data)
            member.mode = 0o755
            archive.addfile(member, io.BytesIO(data))
        for name, target in (links or {}).items():
            member = tarfile.TarInfo("fixture/" + name)
            member.type = tarfile.SYMTYPE
            if isinstance(target, tuple):
                target, member.type = target
            member.linkname = target
            archive.addfile(member)
    return stream.getvalue()


def generation_script():
    workflow = (ROOT / ".github/workflows/opencode.yml").read_text()
    step = workflow.split("      - name: Generate review with OpenCode V2\n", 1)[1]
    step = step.split("      - name:", 1)[0]
    return textwrap.dedent(step.split("        run: |\n", 1)[1])


def completed(text="No correctness findings.", message="final"):
    return [
        {"type": "text", "part": {"messageID": message, "text": text}},
        {"type": "step_finish", "part": {"messageID": message, "reason": "stop"}},
    ]


class ReviewTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(
            prefix="review-tests-", dir=os.getenv("TMPDIR")
        )
        self.addCleanup(self.temp.cleanup)
        self.scratch = Path(self.temp.name)
        env = {
            "GITHUB_REPOSITORY": "example/repo",
            "PR_NUMBER": "91",
            "PR_HEAD_SHA": SHA,
            "PR_BASE_SHA": OTHER_SHA,
            "REVIEW_CODE_SHA": OTHER_SHA,
            "GITHUB_SERVER_URL": "https://github.com",
            "GITHUB_RUN_ID": "123",
            "TMPDIR": str(self.scratch),
        }
        self.env = patch.dict(os.environ, env, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_inputs_reject_invalid_identifiers(self):
        for key, value in (
            ("GITHUB_REPOSITORY", "--repo"),
            ("PR_NUMBER", "-1"),
            ("PR_HEAD_SHA", "main"),
        ):
            with self.subTest(key=key), patch.dict(os.environ, {key: value}):
                with self.assertRaises(ValueError):
                    review.inputs()

    def test_prepare_collects_metadata_and_patch(self):
        meta = {
            "headRefOid": SHA,
            "baseRefOid": OTHER_SHA,
            "title": "Fixture PR",
            "body": "Untrusted body",
        }

        def download(*args, **kwargs):
            kwargs["stdout"].write(archive_bytes({"main.go": "package main"}))

        with (
            patch.object(
                review,
                "gh",
                side_effect=[json.dumps(meta), "fixture diff", json.dumps(meta)],
            ),
            patch.object(
                review.subprocess, "check_output", return_value=OTHER_SHA + "\n"
            ),
            patch.object(review.subprocess, "run", side_effect=download) as fetch,
        ):
            review.prepare()
        self.assertEqual(
            fetch.call_args.args[0], ["gh", "api", f"repos/example/repo/tarball/{SHA}"]
        )
        context = (self.scratch / "model" / "pr-context.md").read_text()
        for expected in (
            SHA,
            "Fixture PR",
            "Untrusted body",
            "fixture diff",
            "untrusted",
        ):
            self.assertIn(expected, context)

    def test_prepare_rejects_mismatched_or_changing_head(self):
        for metadata_sha, checkout_sha, final_sha in (
            (OTHER_SHA, OTHER_SHA, SHA),
            (SHA, SHA, SHA),
            (SHA, OTHER_SHA, OTHER_SHA),
        ):
            with self.subTest(heads=(metadata_sha, checkout_sha, final_sha)):
                with (
                    patch.object(
                        review,
                        "gh",
                        side_effect=[
                            json.dumps(
                                {"headRefOid": metadata_sha, "baseRefOid": OTHER_SHA}
                            ),
                            "diff",
                            json.dumps(
                                {"headRefOid": final_sha, "baseRefOid": OTHER_SHA}
                            ),
                        ],
                    ),
                    patch.object(
                        review.subprocess, "check_output", return_value=checkout_sha
                    ),
                    patch.object(
                        review.subprocess,
                        "run",
                        side_effect=lambda *args, **kwargs: kwargs["stdout"].write(
                            archive_bytes({})
                        ),
                    ),
                ):
                    with self.assertRaises(ValueError):
                        review.prepare()
                self.assertFalse((self.scratch / "model" / "pr-context.md").exists())

    def test_render_uses_only_final_assistant_text(self):
        intermediate = completed("Not a review: investigating.", "intermediate")
        intermediate[-1]["part"]["reason"] = "tool-calls"
        body = review.render_review(intermediate + completed(), SHA, RUN_URL)
        self.assertTrue(body.startswith("OpenCode on behalf of apollopower\n\n"))
        self.assertIn("No correctness findings.", body)
        self.assertIn(SHA, body)
        self.assertIn(RUN_URL, body)
        self.assertNotIn("investigating", body)

    def test_render_always_includes_fixed_validation_note(self):
        for text in ("No findings.", "All tests passed."):
            with self.subTest(text=text):
                body = review.render_review(completed(text), SHA, RUN_URL)
                self.assertTrue(
                    body.startswith(
                        "OpenCode on behalf of apollopower\n\n"
                        + review.VALIDATION_NOTE
                        + "\n\n"
                    )
                )
                self.assertLess(body.index(review.VALIDATION_NOTE), body.index(text))

    def test_source_export_is_inert_and_never_follows_links(self):
        files = {
            "main.go": "package main",
            "opencode.json": '{"plugins":["evil"]}',
            ".opencode/plugins/evil.ts": "throw new Error('not executable');",
            "nested/opencode.jsonc": '{"permissions":[]}',
            "AGENTS.md": "not ambient instructions",
            ".github/scripts/opencode_review.py": "raise RuntimeError('untrusted')",
            ".env": "secret",
            "nested/.env.local": "secret",
            ".env.example": "example",
            "binary": b"\0binary",
            "large": b"x" * (review.MAX_FILE_BYTES + 1),
            "a/{env:OPENCODE_API_KEY}": "literal data",
        }
        links = {
            name: "/proc/self/environ"
            for name in (
                "*",
                "**",
                "?",
                "!",
                "[",
                ".env.example-link",
                "outside-dir",
                "env-alias",
                "cycle",
            )
        }
        links["hard-link"] = ("fixture/main.go", tarfile.LNKTYPE)
        with tarfile.open(
            fileobj=io.BytesIO(archive_bytes(files, links)), mode="r:gz"
        ) as source:
            review.export_source(source, self.scratch)
        entries = {
            entry["path"]: entry
            for entry in json.loads((self.scratch / "index.json").read_text())
        }
        for name, content in files.items():
            if name in (".env", "nested/.env.local", "binary", "large"):
                self.assertIn("omitted", entries[name])
                continue
            view = entries[name]["view"]
            self.assertRegex(view, r"^files/[0-9]+\.txt$")
            self.assertEqual((self.scratch / view).read_text(), content)
        for name in links:
            self.assertIn("omitted", entries[name])
        self.assertEqual(
            sorted(path.name for path in self.scratch.iterdir()),
            ["files", "index.json"],
        )
        self.assertFalse(any(path.is_symlink() for path in self.scratch.rglob("*")))
        self.assertFalse(
            any(
                path.stat().st_mode & 0o111
                for path in (self.scratch / "files").iterdir()
            )
        )

    def test_source_export_rejects_unsafe_paths_and_size_limits(self):
        for name in ("../escape", "/absolute", "fixture/../../escape"):
            with (
                self.subTest(name=name),
                tempfile.TemporaryDirectory(dir=self.scratch) as tmp,
            ):
                data = archive_bytes({name: "invalid"})
                with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as source:
                    with self.assertRaises(ValueError):
                        review.export_source(source, Path(tmp))
        for key, value in (("MAX_ENTRIES", 1), ("MAX_SOURCE_BYTES", 1)):
            with (
                self.subTest(limit=key),
                tempfile.TemporaryDirectory(dir=self.scratch) as tmp,
            ):
                with tarfile.open(
                    fileobj=io.BytesIO(archive_bytes({"a": "abc", "b": "def"})),
                    mode="r:gz",
                ) as source:
                    with (
                        patch.object(review, key, value),
                        self.assertRaises(ValueError),
                    ):
                        review.export_source(source, Path(tmp))

    def test_config_comes_only_from_trusted_script_directory(self):
        (self.scratch / ".github").mkdir()
        config = self.scratch / ".github/opencode-review.json"
        valid = json.loads((ROOT / ".github/opencode-review.json").read_text())
        config.write_text(json.dumps(valid))
        with (
            patch.object(review, "TRUSTED_ROOT", self.scratch),
            redirect_stdout(io.StringIO()) as output,
        ):
            review.configure()
        self.assertEqual(json.loads(output.getvalue()), valid)
        for malformed in (
            None,
            [],
            {},
            {"agents": None},
            {"agents": {"review": {"permissions": None}}},
            {"agents": {"review": {"permissions": [None]}}},
            {
                "agents": {
                    "review": {
                        "permissions": [
                            {"action": "read", "resource": "*", "effect": "unknown"}
                        ]
                    }
                }
            },
        ):
            config.write_text(json.dumps(malformed))
            with (
                patch.object(review, "TRUSTED_ROOT", self.scratch),
                self.assertRaises(ValueError),
            ):
                review.configure()

    def test_workflow_separates_untrusted_preflight_from_privileged_review(self):
        workflow = (ROOT / ".github/workflows/opencode.yml").read_text()
        preflight = workflow.split("  review-preflight:\n")[1].split("  review:\n")[0]
        trusted = workflow.split("  review:\n")[1].split("  opencode:\n")[0]
        self.assertIn("github.event_name == 'pull_request'", preflight)
        self.assertIn("contents: read", preflight)
        self.assertNotIn("secrets.", preflight)
        self.assertNotIn(": write", preflight)
        self.assertIn("github.event_name == 'pull_request_target'", trusted)
        self.assertIn("ref: ${{ github.workflow_sha }}", trusted)
        self.assertIn(
            "github.event.pull_request.head.repo.full_name == github.repository",
            trusted,
        )
        self.assertNotIn("ref: ${{ github.event.pull_request.head.sha }}", trusted)
        self.assertNotIn("unittest", trusted)
        self.assertIn("contents: read", trusted)
        self.assertNotIn("contents: write", trusted)
        self.assertNotIn("fetch-depth: 0", trusted)
        for job in (preflight, trusted):
            self.assertIn("working-directory: ${{ runner.temp }}", job)
            self.assertIn("--registry=https://registry.npmjs.org", job)
            self.assertIn('--userconfig="$prefix/user.npmrc"', job)
            self.assertIn('--globalconfig="$prefix/global.npmrc"', job)
            self.assertNotIn("actions/cache", job)
        cfg = json.loads((ROOT / ".github/opencode-review.json").read_text())
        allowed = {
            rule["resource"]
            for rule in cfg["agents"]["review"]["permissions"]
            if rule["action"] == "read" and rule["effect"] == "allow"
        }
        self.assertEqual(allowed, {"pr-context.md", "index.json", "files/*.txt"})

    def test_render_rejects_invalid_or_incomplete_results(self):
        cases = [
            None,
            {},
            [],
            [None],
            [{}],
            [{"type": 7}],
            [{"type": "step_finish", "part": None}],
            [{"type": "step_finish", "part": []}],
            [{"type": "step_finish", "part": {"messageID": 7, "reason": "stop"}}],
            [{"type": "step_finish", "part": {"messageID": "", "reason": "stop"}}],
            [{"type": "step_finish", "part": {"messageID": "final", "reason": None}}],
            [{"type": "text", "part": {"messageID": "final", "text": {}}}],
            completed(" "),
            completed("é" * 30000),
            completed() + [{"type": "error", "error": "fixture failure"}],
            completed() + [{"type": "step_start", "part": {"messageID": "next"}}],
            [completed()[0]],
            [{"type": "step_finish", "part": {"messageID": "other", "reason": "stop"}}],
        ]
        for reason in ("tool-calls", "length", "error"):
            events = completed()
            events[-1]["part"]["reason"] = reason
            cases.append(events)
        for events in cases:
            with self.subTest(events=events):
                with self.assertRaises(ValueError):
                    review.render_review(events, SHA, RUN_URL)

    def write_events(self, events):
        (self.scratch / "review.jsonl").write_text(
            "\n".join(json.dumps(event) for event in events) + "\n"
        )

    def test_render_writes_comment_and_rejects_credential(self):
        self.write_events(completed())
        review.render()
        self.assertIn(
            "No correctness findings.", (self.scratch / "review.md").read_text()
        )
        (self.scratch / "review.md").unlink()
        self.write_events(completed("fixture-provider-secret"))
        with patch.dict(os.environ, {"OPENCODE_API_KEY": "fixture-provider-secret"}):
            with self.assertRaisesRegex(ValueError, "credential"):
                review.render()
        self.assertFalse((self.scratch / "review.md").exists())

    def test_render_rejects_malformed_json(self):
        (self.scratch / "review.jsonl").write_text("not JSON\n")
        with self.assertRaises(ValueError):
            review.render()

    def test_malformed_events_fail_cleanly_at_cli_boundary(self):
        for event in (
            {"type": "step_finish", "part": None},
            {"type": "text", "part": {"messageID": "final", "text": 7}},
        ):
            with self.subTest(event=event):
                self.write_events([event])
                result = subprocess.run(
                    [
                        sys.executable,
                        str(ROOT / ".github/scripts/opencode_review.py"),
                        "render",
                    ],
                    capture_output=True,
                    text=True,
                    timeout=10,
                )
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout, "")
                self.assertIn("Review pipeline failed:", result.stderr)
                self.assertNotIn("Traceback", result.stderr)
                self.assertFalse((self.scratch / "review.md").exists())

    def test_post_checks_head_before_publishing(self):
        body = self.scratch / "review.md"
        body.write_text("OpenCode on behalf of apollopower\n\nFixture review.")
        with patch.object(review, "gh", side_effect=[SHA, "fixture-comment-url"]) as gh:
            review.post()
        self.assertEqual(
            gh.call_args_list[-1].args,
            ("pr", "comment", "91", "--repo", "example/repo", "--body-file", str(body)),
        )

    def test_post_rejects_stale_or_missing_review(self):
        for head, text in ((OTHER_SHA, "review"), (SHA, None), (SHA, " ")):
            with self.subTest(head=head, text=text):
                if text is not None:
                    (self.scratch / "review.md").write_text(text)
                with patch.object(review, "gh", return_value=head) as gh:
                    with self.assertRaises(ValueError):
                        review.post()
                    self.assertEqual(gh.call_count, 1)
                (self.scratch / "review.md").unlink(missing_ok=True)


class FixtureProvider:
    def __init__(self, calls):
        self.calls = calls
        self.requests = []
        self.turn = 0
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                fixture.requests.append(body)
                self.send_response(200)
                if not body.get("stream"):
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(
                        json.dumps(
                            {
                                "id": "fixture-title",
                                "object": "chat.completion",
                                "choices": [
                                    {
                                        "index": 0,
                                        "message": {
                                            "role": "assistant",
                                            "content": "Fixture review",
                                        },
                                        "finish_reason": "stop",
                                    }
                                ],
                            }
                        ).encode()
                    )
                    return
                if not body.get("tools"):
                    delta = {"role": "assistant", "content": "Fixture title"}
                    reason = "stop"
                elif fixture.turn < len(fixture.calls):
                    name, args = fixture.calls[fixture.turn]
                    delta = {
                        "role": "assistant",
                        "content": "INTERMEDIATE_PREAMBLE_MUST_NOT_POST",
                        "tool_calls": [
                            {
                                "index": 0,
                                "id": f"fixture-{fixture.turn}",
                                "type": "function",
                                "function": {
                                    "name": name,
                                    "arguments": json.dumps(args),
                                },
                            }
                        ],
                    }
                    reason = "tool_calls"
                else:
                    delta = {
                        "role": "assistant",
                        "content": "Fixture review: no findings. Tests not run.",
                    }
                    reason = "stop"
                if body.get("tools"):
                    fixture.turn += 1
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for value, finish in ((delta, None), ({}, reason)):
                    chunk = {
                        "id": "fixture",
                        "object": "chat.completion.chunk",
                        "model": "fixture",
                        "choices": [
                            {"index": 0, "delta": value, "finish_reason": finish}
                        ],
                    }
                    self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *args):
        self.server.shutdown()
        self.thread.join()
        self.server.server_close()


@unittest.skipUnless(
    os.getenv("OPENCODE_TEST_BINARY"), "Set OPENCODE_TEST_BINARY for real V2 validation"
)
class RuntimeTests(unittest.TestCase):
    def test_pinned_runtime_permissions_and_local_pipeline(self):
        binary = str(Path(os.environ["OPENCODE_TEST_BINARY"]).resolve())
        self.assertEqual(
            subprocess.check_output([binary, "--version"], text=True).strip(),
            "opencode v2.3.0",
        )
        with tempfile.TemporaryDirectory(
            prefix="review-runtime-", dir=os.getenv("TMPDIR")
        ) as tmp:
            root = Path(tmp)
            repo = root / "checkout with spaces"
            repo.mkdir()
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(
                [
                    "git",
                    "-c",
                    "user.name=Fixture",
                    "-c",
                    "user.email=fixture@example.invalid",
                    "-c",
                    "commit.gpgsign=false",
                    "commit",
                    "-q",
                    "--allow-empty",
                    "-m",
                    "Fixture",
                ],
                cwd=repo,
                check=True,
            )
            sha = subprocess.check_output(
                ["git", "rev-parse", "HEAD"], cwd=repo, text=True
            ).strip()
            scratch = repo / ".git" / "opencode-review-tmp"
            scratch.mkdir()
            (scratch / "private.txt").write_text("RUNNER_PRIVATE_MUST_NOT_LEAK")
            (repo / "opencode.json").write_text((ROOT / "opencode.json").read_text())
            pr = root / "untrusted PR"
            pr.mkdir()
            (root / "outside.txt").write_text("OUTSIDE_READ_MUST_NOT_LEAK")
            marker = root / "PR_CODE_EXECUTED"
            malicious_python = f"from pathlib import Path\nPath({str(marker)!r}).write_text('executed')\nraise RuntimeError('untrusted PR code ran')\n"
            plugin = f"import fs from 'node:fs'; fs.writeFileSync({json.dumps(str(marker))}, 'executed'); export default {{}};"
            source_files = {
                "main.go": "SOURCE_READ_OK",
                ".env": "ENV_READ_MUST_NOT_LEAK",
                ".env.local": "ENV_LOCAL_MUST_NOT_LEAK",
                ".env.example": "EXAMPLE_READ_OK",
                "AGENTS.md": "PR_AGENTS_MUST_NOT_AUTOLOAD",
                "opencode.json": json.dumps(
                    {
                        "agents": {
                            "review": {
                                "permissions": [
                                    {"action": "*", "resource": "*", "effect": "allow"}
                                ]
                            }
                        },
                        "plugins": [str(pr / "evil")],
                    }
                ),
                ".opencode/opencode.json": '{"permissions":[{"action":"*","resource":"*","effect":"allow"}]}',
                ".opencode/plugins/evil.ts": plugin,
                "evil/index.ts": plugin,
                ".github/scripts/opencode_review.py": malicious_python,
                ".github/scripts/test_opencode_review.py": malicious_python,
                ".github/opencode-review.json": '{"agents":{"review":{"permissions":[{"action":"*","resource":"*","effect":"allow"}]}}}',
                ".npmrc": "registry=https://invalid.example.invalid",
                "outside-dir/sub/file": "NESTED_SOURCE_READ_OK",
            }
            for name, content in source_files.items():
                target = pr / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(content)
            links = {
                name: "/proc/self/environ"
                for name in ("*", "**", "?", "[", "!", "env-alias.txt", "outside-dir")
            }
            fixture_archive = root / "fixture.tar.gz"
            fixture_archive.write_bytes(archive_bytes(source_files, links))
            calls = [
                ("read", {"path": "PLACEHOLDER_MAIN"}),
                ("read", {"path": str(root / "outside.txt")}),
                ("read", {"path": "files/../../private.txt"}),
                ("read", {"path": str(scratch / "private.txt")}),
                ("read", {"path": str(pr / ".env")}),
                ("read", {"path": str(pr / ".env.local")}),
                ("read", {"path": "PLACEHOLDER_EXAMPLE"}),
                (
                    "grep",
                    {"pattern": "MUST_NOT_LEAK", "path": str(root / "outside.txt")},
                ),
                ("grep", {"pattern": "MUST_NOT_LEAK", "path": str(pr / ".env")}),
                (
                    "edit",
                    {
                        "path": str(scratch / "model" / "files" / "00001.txt"),
                        "oldString": "SOURCE_READ_OK",
                        "newString": "changed",
                    },
                ),
                (
                    "question",
                    {
                        "questions": [
                            {
                                "question": "Approve?",
                                "header": "Fixture",
                                "options": [{"label": "Yes", "description": "Fixture"}],
                            }
                        ]
                    },
                ),
                ("shell", {"command": "echo SHELL_MUST_NOT_RUN"}),
                (
                    "subagent",
                    {
                        "agent": "general",
                        "description": "Fixture delegation",
                        "prompt": "Say delegated.",
                    },
                ),
                ("read", {"path": str(pr / "opencode.json")}),
                ("read", {"path": str(repo / ".github/scripts/opencode_review.py")}),
                ("read", {"path": "PLACEHOLDER_CONFIG"}),
                ("read", {"path": "PLACEHOLDER_NESTED"}),
                ("read", {"path": "index.json"}),
                ("glob", {"pattern": "files/*.txt"}),
                ("glob", {"pattern": "*"}),
            ]
            # Only this executable can handle gh; no local authentication or network publishing.
            stub = root / "bin"
            stub.mkdir()
            (stub / "gh").write_text(
                "#!/usr/bin/env python3\n"
                "import json, os, pathlib, sys\n"
                "args = sys.argv[1:]\n"
                "if os.environ.get('GH_TOKEN') != 'fixture-gh-credential': sys.exit('GitHub step has no scoped token')\n"
                "if os.environ.get('OPENCODE_API_KEY'): sys.exit('Provider key leaked into GitHub step')\n"
                "with open(os.environ['GH_CALLS'], 'a') as f: f.write(json.dumps(args) + '\\n')\n"
                "if args[:2] == ['pr', 'view']:\n"
                " print(os.environ['PR_HEAD_SHA'] if '--jq' in args else json.dumps({'headRefOid': os.environ['PR_HEAD_SHA'], 'baseRefOid': os.environ['PR_BASE_SHA'], 'title': 'Fixture PR context'}))\n"
                "elif args[:2] == ['pr', 'diff']: print('FIXTURE_PATCH_CONTEXT')\n"
                "elif args == ['api', 'repos/example/repo/tarball/' + os.environ['PR_HEAD_SHA']]:\n"
                " sys.stdout.buffer.write(pathlib.Path(os.environ['FIXTURE_ARCHIVE']).read_bytes())\n"
                "elif args[:2] == ['pr', 'comment']:\n"
                " pathlib.Path(os.environ['POSTED_BODY']).write_text(pathlib.Path(args[-1]).read_text())\n"
                "else: sys.exit('Unexpected gh command')\n"
            )
            (stub / "gh").chmod(0o700)
            (stub / "opencode").symlink_to(binary)
            env = {
                "PATH": str(stub) + os.pathsep + os.environ["PATH"],
                "LANG": "C.UTF-8",
                "TMPDIR": str(scratch),
                "GITHUB_REPOSITORY": "example/repo",
                "PR_NUMBER": "91",
                "PR_HEAD_SHA": SHA,
                "PR_BASE_SHA": sha,
                "REVIEW_CODE_SHA": sha,
                "GITHUB_WORKSPACE": str(repo),
                "FIXTURE_ARCHIVE": str(fixture_archive),
                "GITHUB_SERVER_URL": "https://github.com",
                "GITHUB_RUN_ID": "123",
                "GH_CALLS": str(root / "gh-calls.jsonl"),
                "POSTED_BODY": str(root / "posted.md"),
                "OPENCODE_REVIEW_MODEL": "fixture/fixture",
                "REVIEW_PROMPT": "Review the fixture.",
                "PYTHONDONTWRITEBYTECODE": "1",
            }
            for key in (
                "HOME",
                "XDG_CONFIG_HOME",
                "XDG_DATA_HOME",
                "XDG_CACHE_HOME",
                "XDG_STATE_HOME",
            ):
                folder = root / key.lower()
                folder.mkdir()
                env[key] = str(folder)
            helper = str(ROOT / ".github/scripts/opencode_review.py")
            subprocess.run(
                ["python3", helper, "prepare"],
                cwd=repo,
                env=dict(env, GH_TOKEN="fixture-gh-credential"),
                check=True,
            )
            model = scratch / "model"
            index = {
                entry["path"]: entry
                for entry in json.loads((model / "index.json").read_text())
            }
            replacements = {
                "PLACEHOLDER_MAIN": index["main.go"]["view"],
                "PLACEHOLDER_EXAMPLE": index[".env.example"]["view"],
                "PLACEHOLDER_CONFIG": index["opencode.json"]["view"],
                "PLACEHOLDER_NESTED": index["outside-dir/sub/file"]["view"],
            }
            for tool, args in calls:
                if tool == "read" and args["path"] in replacements:
                    args["path"] = replacements[args["path"]]
            allowed_reads = set(replacements.values()) | {"index.json"}
            with FixtureProvider(calls) as provider:
                config = copy.deepcopy(
                    json.loads((ROOT / ".github/opencode-review.json").read_text())
                )
                config["agents"]["title"]["model"] = "fixture/fixture"
                config["providers"] = {
                    "fixture": {
                        "package": "@opencode/ai/providers/openai-compatible",
                        "settings": {
                            "baseURL": f"http://127.0.0.1:{provider.server.server_port}/v1",
                            "apiKey": "fixture-key",
                        },
                        "models": {
                            "fixture": {
                                "limit": {"context": 64000, "output": 4096},
                                "capabilities": {"tools": True},
                            }
                        },
                    }
                }
                scripts = repo / ".github" / "scripts"
                scripts.mkdir(parents=True)
                (scripts / "opencode_review.py").write_text(Path(helper).read_text())
                (repo / ".github" / "opencode-review.json").write_text(
                    json.dumps(config)
                )
                env["OPENCODE_API_KEY"] = "fixture-provider-credential"
                result = subprocess.run(
                    [
                        "bash",
                        "--noprofile",
                        "--norc",
                        "-euo",
                        "pipefail",
                        "-c",
                        generation_script(),
                    ],
                    cwd=repo,
                    env=env,
                    stdin=subprocess.DEVNULL,
                    capture_output=True,
                    text=True,
                    timeout=90,
                )
            self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
            output = (scratch / "review.jsonl").read_text()
            events = [json.loads(line) for line in output.splitlines()]
            self.assertIn(
                "Fixture review: no findings.",
                review.render_review(events, SHA, RUN_URL),
            )
            requests = json.dumps(provider.requests)
            self.assertIn("FIXTURE_PATCH_CONTEXT", requests)
            self.assertIn("Fixture PR context", requests)
            self.assertIn("SOURCE_READ_OK", requests)
            self.assertIn("EXAMPLE_READ_OK", requests)
            for secret in (
                "OUTSIDE_READ_MUST_NOT_LEAK",
                "ENV_READ_MUST_NOT_LEAK",
                "ENV_LOCAL_MUST_NOT_LEAK",
                "fixture-provider-credential",
                "fixture-gh-credential",
                "RUNNER_PRIVATE_MUST_NOT_LEAK",
            ):
                self.assertFalse(
                    secret in requests + output, "Leaked fixture " + secret
                )
            self.assertFalse(marker.exists())
            self.assertEqual(
                (model / replacements["PLACEHOLDER_MAIN"]).read_text(), "SOURCE_READ_OK"
            )
            system_messages = json.dumps(
                [
                    message
                    for request in provider.requests
                    for message in request.get("messages", [])
                    if message["role"] == "system"
                ]
            )
            self.assertNotIn("PR_AGENTS_MUST_NOT_AUTOLOAD", system_messages)
            self.assertFalse(any(path.is_symlink() for path in model.rglob("*")))
            self.assertEqual(
                sorted(path.name for path in model.iterdir()),
                ["files", "index.json", "pr-context.md"],
            )
            tools = [
                event["part"] for event in events if event.get("type") == "tool_use"
            ]
            self.assertEqual(len(tools), len(calls), output)
            for part in tools:
                state = part["state"]
                self.assertEqual(
                    state["status"],
                    "completed"
                    if (
                        part["tool"] == "read"
                        and state["input"]["path"] in allowed_reads
                    )
                    or (
                        part["tool"] == "glob"
                        and state["input"]["pattern"] == "files/*.txt"
                    )
                    else "error",
                    part,
                )
                if part["tool"] == "read" and state["input"]["path"] in {
                    str(root / "outside.txt"),
                    str(pr / ".env"),
                    str(pr / ".env.local"),
                    str(pr / "opencode.json"),
                    str(repo / ".github/scripts/opencode_review.py"),
                    "files/../../private.txt",
                    str(scratch / "private.txt"),
                }:
                    self.assertIn("Permission denied", state["error"])
            env.pop("OPENCODE_API_KEY")
            subprocess.run(
                ["python3", helper, "post"],
                cwd=repo,
                env=dict(env, GH_TOKEN="fixture-gh-credential"),
                check=True,
            )
            posted = (root / "posted.md").read_text()
            self.assertTrue(posted.startswith("OpenCode on behalf of apollopower\n\n"))
            self.assertIn(SHA, posted)
            self.assertIn("Fixture review: no findings.", posted)
            self.assertIn(review.VALIDATION_NOTE, posted)
            self.assertIn("INTERMEDIATE_PREAMBLE_MUST_NOT_POST", output)
            self.assertNotIn("INTERMEDIATE_PREAMBLE_MUST_NOT_POST", posted)
            self.assertEqual(len((root / "gh-calls.jsonl").read_text().splitlines()), 6)
            (scratch / "review.md").unlink()
            (stub / "opencode").unlink()
            (stub / "opencode").write_text("#!/bin/sh\nexit 13\n")
            (stub / "opencode").chmod(0o700)
            env["OPENCODE_API_KEY"] = "fixture-provider-credential"
            failed = subprocess.run(
                [
                    "bash",
                    "--noprofile",
                    "--norc",
                    "-euo",
                    "pipefail",
                    "-c",
                    generation_script(),
                ],
                cwd=repo,
                env=env,
                capture_output=True,
                text=True,
                timeout=10,
            )
            self.assertEqual(failed.returncode, 13)
            self.assertFalse((scratch / "review.md").exists())
            (repo / ".github" / "opencode-review.json").write_text("invalid JSON")
            failed_config = subprocess.run(
                [
                    "bash",
                    "--noprofile",
                    "--norc",
                    "-euo",
                    "pipefail",
                    "-c",
                    generation_script(),
                ],
                cwd=repo,
                env=env,
                capture_output=True,
                text=True,
                timeout=10,
            )
            self.assertEqual(failed_config.returncode, 1)
            self.assertIn("Review pipeline failed:", failed_config.stderr)
            self.assertFalse((scratch / "review.md").exists())


if __name__ == "__main__":
    unittest.main()
