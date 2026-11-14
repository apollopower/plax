import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
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
        meta = {"headRefOid": SHA, "title": "Fixture PR", "body": "Untrusted body"}
        with (
            patch.object(
                review, "gh", side_effect=[json.dumps(meta), "fixture diff", SHA]
            ),
            patch.object(review.subprocess, "check_output", return_value=SHA + "\n"),
        ):
            review.prepare()
        context = (self.scratch / "pr-context.md").read_text()
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
            (OTHER_SHA, SHA, SHA),
            (SHA, OTHER_SHA, SHA),
            (SHA, SHA, OTHER_SHA),
        ):
            with self.subTest(heads=(metadata_sha, checkout_sha, final_sha)):
                with (
                    patch.object(
                        review,
                        "gh",
                        side_effect=[
                            json.dumps({"headRefOid": metadata_sha}),
                            "diff",
                            final_sha,
                        ],
                    ),
                    patch.object(
                        review.subprocess, "check_output", return_value=checkout_sha
                    ),
                ):
                    with self.assertRaises(ValueError):
                        review.prepare()
                self.assertFalse((self.scratch / "pr-context.md").exists())

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

    def test_config_denies_symlinks_after_other_read_rules(self):
        root = self.scratch
        (root / "ordinary.txt").write_text("fixture")
        (root / "dir").mkdir()
        (root / "dir" / "nested-link").symlink_to(root / "ordinary.txt")
        links = {
            "alias": root / "ordinary.txt",
            "directory": root / "dir",
            ".env.example": root / "ordinary.txt",
            "dangling": root / "missing",
            "cycle": root / "cycle",
            "{env:OPENCODE_API_KEY}": root / "ordinary.txt",
            "~": root / "ordinary.txt",
            "$HOME": root / "ordinary.txt",
        }
        for name, target in links.items():
            (root / name).symlink_to(target)
        cfg = json.loads((ROOT / ".github/opencode-review.json").read_text())
        before = copy.deepcopy(cfg)
        result = review.review_config(root, cfg)
        initial = before["agents"]["review"]["permissions"]
        perms = result["agents"]["review"]["permissions"]
        self.assertEqual(perms[: len(initial)], initial)
        expected = [
            "alias",
            "directory",
            ".env.example",
            "dangling",
            "cycle",
            "?env:OPENCODE_API_KEY}",
            "?",
            "?HOME",
            "dir/nested-link",
        ]
        for name in expected:
            for resource in (name, name + "/*"):
                self.assertIn(
                    {"action": "read", "resource": resource, "effect": "deny"},
                    perms[len(initial) :],
                )
        self.assertEqual(len(perms), len(initial) + 2 * len(expected))
        self.assertEqual(result["agents"]["title"], before["agents"]["title"])
        self.assertEqual((root / "ordinary.txt").read_text(), "fixture")
        self.assertTrue((root / "directory").is_symlink())

    def test_config_fails_if_symlink_inventory_cannot_be_read(self):
        cfg = json.loads((ROOT / ".github/opencode-review.json").read_text())

        def broken_walk(root, **kwargs):
            kwargs["onerror"](OSError("fixture scan error"))

        with patch.object(review.os, "walk", side_effect=broken_walk):
            with self.assertRaises(OSError):
                review.review_config(self.scratch, cfg)
        (self.scratch / "fixture").write_text("fixture")
        with patch.object(Path, "lstat", side_effect=OSError("fixture stat error")):
            with self.assertRaises(OSError):
                review.review_config(self.scratch, cfg)

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
            (scratch / "allowed.txt").write_text("SCRATCH_READ_OK")
            (root / "outside.txt").write_text("OUTSIDE_READ_MUST_NOT_LEAK")
            (repo / ".env").write_text("ENV_READ_MUST_NOT_LEAK")
            (repo / ".env.local").write_text("ENV_LOCAL_MUST_NOT_LEAK")
            (repo / ".env.example").write_text("EXAMPLE_READ_OK")
            (repo / "env-alias.txt").symlink_to(repo / ".env")
            (repo / "outside-alias.txt").symlink_to(root / "outside.txt")
            (repo / "environ-alias.txt").symlink_to("/proc/self/environ")
            (repo / "alias-chain.txt").symlink_to(repo / "env-alias.txt")
            (repo / "outside-dir").symlink_to(root)
            (repo / "nested").mkdir()
            (repo / "nested" / ".env.example").symlink_to(repo / ".env")
            (repo / "{env:OPENCODE_API_KEY}").symlink_to(repo / ".env")
            (repo / "~").symlink_to(repo / ".env")
            (repo / "$HOME").symlink_to(repo / ".env")
            (repo / "dangling-link").symlink_to(repo / "missing")
            (repo / "cycle").symlink_to(repo / "cycle")
            calls = [
                ("read", {"path": str(scratch / "allowed.txt")}),
                ("read", {"path": str(root / "outside.txt")}),
                ("read", {"path": str(repo / ".env")}),
                ("read", {"path": str(repo / ".env.local")}),
                ("read", {"path": str(repo / ".env.example")}),
                (
                    "grep",
                    {"pattern": "MUST_NOT_LEAK", "path": str(root / "outside.txt")},
                ),
                ("grep", {"pattern": "MUST_NOT_LEAK", "path": str(repo / ".env")}),
                (
                    "edit",
                    {
                        "path": str(scratch / "allowed.txt"),
                        "oldString": "SCRATCH_READ_OK",
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
                ("read", {"path": str(repo / "env-alias.txt")}),
                ("read", {"path": str(repo / "outside-alias.txt")}),
                ("read", {"path": str(repo / "environ-alias.txt")}),
                ("read", {"path": str(repo / "alias-chain.txt")}),
                ("read", {"path": str(repo / "outside-dir" / "outside.txt")}),
                ("read", {"path": str(repo / "nested" / ".env.example")}),
                ("read", {"path": str(repo / "{env:OPENCODE_API_KEY}")}),
                ("read", {"path": str(repo / "~")}),
                ("read", {"path": str(repo / "$HOME")}),
                ("read", {"path": str(repo / "dangling-link")}),
                ("read", {"path": str(repo / "cycle")}),
                ("read", {"path": "env-alias.txt"}),
                ("read", {"path": "nested/../env-alias.txt"}),
                ("read", {"path": "outside-dir"}),
                ("read", {"path": "outside-dir/./outside.txt"}),
                ("read", {"path": "outside-dir/checkout with spaces/../outside.txt"}),
            ]
            # Only this executable can handle gh; no local authentication or network publishing.
            stub = root / "bin"
            stub.mkdir()
            (stub / "gh").write_text(
                "#!/usr/bin/env python3\n"
                "import json, os, pathlib, sys\n"
                "args = sys.argv[1:]\n"
                "with open(os.environ['GH_CALLS'], 'a') as f: f.write(json.dumps(args) + '\\n')\n"
                "if args[:2] == ['pr', 'view']:\n"
                " print(os.environ['PR_HEAD_SHA'] if '--jq' in args else json.dumps({'headRefOid': os.environ['PR_HEAD_SHA'], 'title': 'Fixture PR context'}))\n"
                "elif args[:2] == ['pr', 'diff']: print('FIXTURE_PATCH_CONTEXT')\n"
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
                "PR_HEAD_SHA": sha,
                "GITHUB_SERVER_URL": "https://github.com",
                "GITHUB_RUN_ID": "123",
                "GH_CALLS": str(root / "gh-calls.jsonl"),
                "POSTED_BODY": str(root / "posted.md"),
                "OPENCODE_API_KEY": "fixture-provider-credential",
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
                ["python3", helper, "prepare"], cwd=repo, env=env, check=True
            )
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
                review.render_review(events, sha, RUN_URL),
            )
            requests = json.dumps(provider.requests)
            self.assertIn("FIXTURE_PATCH_CONTEXT", requests)
            self.assertIn("Fixture PR context", requests)
            self.assertIn("SCRATCH_READ_OK", requests)
            self.assertIn("EXAMPLE_READ_OK", requests)
            for secret in (
                "OUTSIDE_READ_MUST_NOT_LEAK",
                "ENV_READ_MUST_NOT_LEAK",
                "ENV_LOCAL_MUST_NOT_LEAK",
                "fixture-provider-credential",
            ):
                self.assertFalse(
                    secret in requests + output, "Leaked fixture " + secret
                )
            self.assertEqual((scratch / "allowed.txt").read_text(), "SCRATCH_READ_OK")
            tools = [
                event["part"] for event in events if event.get("type") == "tool_use"
            ]
            self.assertEqual(len(tools), len(calls), output)
            for index, part in enumerate(tools):
                state = part["state"]
                self.assertEqual(
                    state["status"], "completed" if index in (0, 4) else "error", part
                )
                if index >= 11:
                    self.assertIn("Permission denied", state["error"])
            env.pop("OPENCODE_API_KEY")
            subprocess.run(["python3", helper, "post"], cwd=repo, env=env, check=True)
            posted = (root / "posted.md").read_text()
            self.assertTrue(posted.startswith("OpenCode on behalf of apollopower\n\n"))
            self.assertIn(sha, posted)
            self.assertIn("Fixture review: no findings.", posted)
            self.assertIn(review.VALIDATION_NOTE, posted)
            self.assertIn("INTERMEDIATE_PREAMBLE_MUST_NOT_POST", output)
            self.assertNotIn("INTERMEDIATE_PREAMBLE_MUST_NOT_POST", posted)
            self.assertEqual(len((root / "gh-calls.jsonl").read_text().splitlines()), 5)
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
