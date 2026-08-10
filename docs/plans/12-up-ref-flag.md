# Plan 12 — `plax up` start point: `--ref` flag

## Objective

Add a `--ref` flag to `plax up` so an instance can be built from a
branch, tag, PR, or commit SHA instead of always branching from the
repo root's current HEAD. This enables the most common reason to create
a parallel environment — reviewing someone else's change — without
requiring manual `git fetch` and `git checkout --detach` in the
worktree.

---

## Package layout

```
cmd/plax/main.go              # Add --ref to UpCmd; thread into runUp → instance.Up
pkg/instance/
  up.go                       # Pass sourceRef to worktree.Create
  instance.go                 # Deps gets SourceRef field
  instance_test.go            # Add --ref test cases
pkg/worktree/
  worktree.go                 # Create gains sourceRef param; resolve PR refs
  git.go                      # No changes (RefExists, HeadRef already present)
  worktree_test.go            # Tests for branch-from-ref, PR resolution
pkg/registry/
  registry.go                 # InstanceRecord gets SourceRef field
cmd/plax/
  e2e_test.go                 # E2E with --ref flag
docs/plans/
  index.md                    # Add plan 12 to phase table
```

---

## Type specifications

### `cmd/plax/main.go`

```go
type UpCmd struct {
    Name  string `arg:"" help:"Instance name (e.g. i1)"`
    Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
    PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
    Ref   string `name:"ref" short:"R" optional:"" help:"Branch, PR number, tag, or commit SHA to branch from (default: current HEAD)"`
}
```

`--ref` accepts:
- A branch name: `main`, `feature/foo`, `origin/bar` — branch from the tip
  of that ref. Plax resolves it via `git rev-parse --verify <ref>` before
  creating the branch.
- A PR number: `3861` or `pr/3861` — resolves to `refs/pull/<N>/head`.
  Prefix `pr/` is optional; bare integers are treated as PR numbers.
- A tag: `v1.2.3` — resolves via `git rev-parse --verify`.
- A commit SHA: `abc1234` — resolves via `git rev-parse --verify`.
- An explicit ref: `refs/pull/3861/head`, `refs/tags/v1.2.3` — passes
  through to git unmodified.

### `pkg/instance/instance.go`

```go
type Deps struct {
    Blueprint *blueprint.Blueprint
    Registry  *registry.Registry
    Pool      *portpool.PortPool
    BM        BaseManager
    Docker    DockerDriver
    RepoRoot  string
    SourceRef string   // NEW: ref to branch from (empty = repo-root HEAD)
}
```

### `pkg/registry/registry.go`

```go
type InstanceRecord struct {
    // ... existing fields unchanged ...
    SourceRef  string `json:"source_ref,omitempty"`   // NEW: the --ref value used at creation
}
```

Stored for provenance. When the user runs `plax status`, the source ref
appears in the drift report — an instance created from a PR can report
"ahead 3 of main, behind 0 (from PR #3861)" instead of silently claiming
to be up to date with main.

### `pkg/worktree/worktree.go`

```go
// Create creates a git branch (plax/<name>) and adds a worktree at
// .plax/worktrees/<name>.
//
// If sourceRef is empty, branches from the repo root's current HEAD
// (existing behaviour). Otherwise, branches from the resolved commit
// of sourceRef.
//
// sourceRef is resolved by ResolveRef before branch creation. The
// resolution step runs git fetch for remote refs that are not already
// present.
func Create(repoRoot, name, sourceRef string) (string, error)

// ResolveRef resolves a user-supplied ref string to a git commit SHA
// (or a fully qualified ref that git branch can accept).
//
//   - Empty string → "" (caller uses repo-root HEAD).
//   - Bare integer → "refs/pull/<N>/head" (fetched if needed).
//   - Prefixed "pr/<N>" → "refs/pull/<N>/head" (fetched if needed).
//   - Branch/tag/SHA → verified via `git rev-parse --verify`.
//   - Explicit refs/ prefix → verified as-is.
func ResolveRef(repoRoot, ref string) (string, error)
```

---

## Algorithms

### Ref resolution in `ResolveRef`

```
ResolveRef(repoRoot, ref):
  1. If ref is "" → return "", nil.
  2. If ref is all digits, or starts with "pr/" followed by digits:
     a. Strip "pr/" prefix if present. Call the result N.
     b. Construct fetchRef = "refs/pull/" + N + "/head".
     c. If `git rev-parse --verify refs/pull/N/head` fails:
        - Run `git fetch origin refs/pull/N/head:refs/pull/N/head`.
        - ⚠ Fetch failure: return error "cannot fetch PR #N — does the remote
          have a PR with that number? Try 'gh pr view N' to confirm."
     d. Return fetchRef.
  3. If ref starts with "refs/" → verify with `git rev-parse --verify <ref>`.
     ⚠ Not found: error "ref <ref> not found".
     Return ref.
  4. Otherwise (branch, tag, or short SHA):
     a. Try `git rev-parse --verify <ref>`.
     b. If that fails, try `git rev-parse --verify origin/<ref>`.
     c. ⚠ Both fail: error "ref <ref> not found — check the branch name or
        run 'git fetch'".
     d. Return the first one that worked.
```

### Create changes

The `Create` signature changes from `Create(repoRoot, name string)` to
`Create(repoRoot, name, sourceRef string)`. When `sourceRef` is non-empty
and already resolved by the caller:

```go
func Create(repoRoot, name, sourceRef string) (string, error) {
    // ... same preamble ...

    if sourceRef == "" {
        cmd = exec.Command("git", "branch", branch)
    } else {
        cmd = exec.Command("git", "branch", branch, sourceRef)
    }
    cmd.Dir = repoRoot
    // ... same error handling ...
}
```

⚠ If `sourceRef` is a commit SHA (not a named ref), `git branch <name>
   <sha>` creates the branch at that commit. The branch is still named
   `plax/<name>`. This is fine — the worktree gets checked out at that
   commit, and the user can later check out branches inside it.

⚠ `worktree.HeadRef(deps.RepoRoot)` (line 169 of `up.go`) still records
   the repo root's HEAD as `BaseRef`/`BaseCommit` — those are provenance
   ("what was the repo root when `up` was run?"), not a description of the
   worktree's contents. The `SourceRef` field records what the user
   intended to build from. Together, the drift report can say "instance
   built from PR #3861 (when repo root was at main@abc1234)".

### Up changes

In `runUp` (`cmd/plax/main.go`, line 385):

```go
func runUp(cmd UpCmd) error {
    // ... existing setup ...

    resolvedRef, err := worktree.ResolveRef(cmd.Root, cmd.Ref)
    if err != nil {
        return err
    }
    deps.Deps.SourceRef = cmd.Ref  // original user input, for the record
    deps.Deps.ResolvedRef = resolvedRef  // the git ref to pass to Create

    return instance.Up(ctx, deps.Deps, cmd.Name)
}
```

⚠ `ResolveRef` may run `git fetch`. This is the only step in `up` that
   talks to a remote. A network failure here should fail `up` before any
   side effects are created (the call happens in `runUp`, before
   `instance.Up` which creates the branch/worktree).

In `instance.Up` (`pkg/instance/up.go`, line 82):

```go
worktreePath, err := worktree.Create(deps.RepoRoot, name, deps.SourceRef)
```

In the registry record (line 293):

```go
SourceRef:  deps.SourceRef,  // NEW
BaseRef:    baseRef,
BaseCommit: baseCommit,
```

`BaseRef` and `BaseCommit` remain the repo root's HEAD at creation time
— they are provenance, not the instance's content identity.

---

## CLI specification

### `plax up <name> --ref <ref>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional), `--pg-url <dsn>` (optional), `--ref <ref>` (optional) |
| Default | No `--ref` → branch from repo-root HEAD (unchanged from today) |
| Exit 0 | Instance created from specified ref |
| Exit 1 | Ref resolution fails (ref not found, fetch failed, invalid PR number) |
| Stderr | "creating branch and worktree from <ref>…" when `--ref` is used |
| Summary | No change — the summary already prints the branch name (`plax/<name>`). The worktree sits at the source ref's commit. |

### `--ref` argument format

```
plax up r3861 --ref 3861
plax up r3861 --ref pr/3861
plax up r3861 --ref mg/fix-foo
plax up r3861 --ref abc1234
plax up r3861 --ref v2.0.0
plax up r3861 --ref refs/pull/3861/head
plax up r3861                  # same as today
```

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| Missing `--ref` value | `--ref ""` | Branch from repo-root HEAD (unchanged). |
| PR number not an integer | `strconv.Atoi` fails | `"--ref 38a6: expecting a PR number, got '38a6'"`. Exit 1. |
| PR fetch fails | `git fetch origin refs/pull/N/head` exits non-zero | `"cannot fetch PR #N — does the remote have a PR with that number?"`. Exit 1. |
| Named ref not found | Both `git rev-parse <ref>` and `git rev-parse origin/<ref>` fail | `"ref <ref> not found — check the branch name or run 'git fetch'"`. Exit 1. |
| Explicit ref not found | `git rev-parse --verify refs/...` fails | `"ref <ref> not found"`. Exit 1. |
| Branch already exists | `git branch <name>` fails (same as today) | Same error as today — the instance name collision is caught before ref resolution. |
| Instance already exists in registry | `GetInstance` returns true | Same error as today. |
| Worktree in detached HEAD state | `git branch <sha>` succeeds, worktree HEAD is a bare commit | Normal operation. `SourceRef` in the registry records the original ref. `WorktreeHead` returns `ref=""` for detached HEAD (existing behaviour per plan 10). |
| --ref combined with Ctrl-C | SIGINT during `git fetch` or `git branch` | Rollback. `git fetch` to a local ref under `refs/pull/` is safe to leave behind — it only adds a ref, and a subsequent `up` call skips the fetch. |

---

## Tests

### Unit tests

**`pkg/worktree/worktree_test.go`:**

- `TestResolveRef_Empty` — `""` → `""`.
- `TestResolveRef_PRNumber` — `"3861"` → `"refs/pull/3861/head"` (with fetch mock or real fetch on an open source repo).
- `TestResolveRef_PRNumberPrefixed` — `"pr/3861"` → same result.
- `TestResolveRef_BranchName` — `"main"` → verified via `git rev-parse --verify main` (works in any git repo).
- `TestResolveRef_OriginBranch` — `"origin/main"` → verified.
- `TestResolveRef_Tag` — tag present → verified.
- `TestResolveRef_CommitSHA` — short SHA → resolved.
- `TestResolveRef_ExplicitRef` — `"refs/heads/main"` → verified.
- `TestResolveRef_NotFound` — nonexistent ref → error.
- `TestResolveRef_BareIntegerFetched` — PR reference not in local repo → `git fetch` runs (skip if no remote; use a temp repo with a pre-seeded `refs/pull/N/head`).
- `TestCreate_WithRef` — `Create(repoRoot, name, "some-ref")` → branch at that ref, worktree checks it out.
- `TestCreate_WithDetachedRef` — `Create(repoRoot, name, "<sha>")` → branch at that SHA, worktree in detached state.

**`pkg/instance/instance_test.go`:**

- `TestUp_WithRef` — `SourceRef` set, branch created from that ref, record has `SourceRef`.
- `TestUp_WithoutRef` — `SourceRef` empty, behaviour unchanged, record has empty `SourceRef`.

### End-to-end test

`TestEndToEnd_UpWithRef` (`cmd/plax/e2e_test.go`) — uses a fixture repo
with a second branch:

- `plax up i1 --ref other-branch` succeeds.
- The worktree is at `other-branch`'s HEAD.
- Run the fixture command inside the worktree — it produces output matching
  `other-branch`'s version.
- `plax up i2` (no `--ref`) branches from the fixture repo's main HEAD.
- Both instances run concurrently on different ports.
- `plax down i1 && plax down i2` clean up both.

---

## Acceptance criteria

- [ ] `plax up i1` (no `--ref`) behaves identically to before this plan
- [ ] `plax up i1 --ref main` creates an instance branched from `main` (explicitly, same result as the default when on `main`)
- [ ] `plax up i1 --ref other-branch` creates an instance branched from `other-branch`, and commands run inside the worktree see that branch's file contents
- [ ] `plax up i1 --ref <sha>` creates a worktree at that commit; the worktree is in detached HEAD state and the instance is usable
- [ ] `plax up i1 --ref pr/3861` and `plax up i1 --ref 3861` both resolve to `refs/pull/3861/head` via `git fetch` when the ref is not already present
- [ ] `plax up i1 --ref nonexistent` fails with a clear error message before any side effects
- [ ] `plax status i1` reports the source ref when one was used
- [ ] `InstanceRecord.SourceRef` is populated with the user-supplied `--ref` value (not the resolved SHA)
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (both with and without `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| No new external dependencies. | | | |

All work uses existing project packages:

- `github.com/alecthomas/kong` — `--ref` flag on `UpCmd`
- `github.com/apollopower/plax/pkg/instance`
- `github.com/apollopower/plax/pkg/worktree`
- `github.com/apollopower/plax/pkg/registry`
- `github.com/apollopower/plax/pkg/blueprint`

Standard library additions:

- `strconv` — PR number parsing in `ResolveRef`
- `os/exec` — `git fetch` in `ResolveRef`

No new external dependencies. `gh` CLI (GitHub CLI) is deliberately **not**
required — `git fetch origin refs/pull/<N>/head` works without it on any
Git remote that exposes pull request refs (GitHub, GitLab, Bitbucket, and
Gitea all do). The `gh` fallback from the original issue proposal is
unnecessary.

---

## Deferred items

| Item | Deferred to | Reason |
|---|---|---|
| `--from <sha>` separate flag | — | `--ref <sha>` covers this. A separate flag adds complexity without new capability. |
| `gh pr view` integration for PR title/description in registry | Later plan | Nice-to-have for `ls --json` output but not blocking. |
| `plax up` recording the PR description in the registry for display in `status` | When `status` grows a detail view | The `SourceRef` field is sufficient for provenance; richer metadata belongs in a future plan. |
| Automatic PR branch checkout in the worktree (instead of detached) | Later plan | Detached HEAD is the safest default — it prevents accidental pushes. A `--branch` mode that creates a tracking branch inside the worktree can be added later. |
| Interaction with plan 10 (status worktree HEAD) | Plan 10 | If plan 10 lands first, `codeDrift` already measures from the worktree's actual HEAD. `--ref` instances then naturally measure against the source ref's base, not the repo-root's base. If plan 10 is not merged, `--ref` instances will still report incorrect drift — but no worse than before, and the `SourceRef` field gives plan 10 the information it needs to fix it. |
