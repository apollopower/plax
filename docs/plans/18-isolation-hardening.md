# Plan 18 — Isolation hardening: scratch directory + dependency-share detection

## Objective

Close the two remaining isolation gaps from triage Phase 4: give every instance
a gitignored `scratch/` directory (issue #53), and make the shared-parent-tree
dependency hazard visible via a verification check that fails loudly when a
branch's manifests diverge from the shared tree (issue #48, detection-only).

## Decision recorded from triage 2026-08-14

**#53 ships as prescribed by the triage:** `scratch/` created in the worktree
at `up`, ignored via that worktree's local exclude file (`.git/info/exclude`,
never the repo's tracked `.gitignore`), removed on `down`, reported in the `up`
summary. No cross-cutting concerns; no registry or blueprint changes.

**#48 is scoped to the detection check only (#48a).** The triage's four options
are ordered by cost and it recommends starting with the check; the hardlink-copy
fix (#48b) is deferred to a dedicated plan, to be evaluated on its own. The
check "converts a silent wrong answer into a visible one — which is the
property that matters most for correctness."

Three refinements to the triage's sketch, decided here:

**The comparison is worktree-working-tree vs repo-root-working-tree, not ref
vs ref.** The shared tree is built from whatever the parent checkout has
installed — disk state, not a commit. Comparing the two working trees on disk
names disagreements the running instance actually faces, including uncommitted
changes on either side. No git plumbing beyond plain file reads.

**The check fires only when the worktree lacks its own `node_modules` and
the parent has a tree to share.** The walk-up hazard (issue #48: Node module
resolution climbing from `.plax/worktrees/<name>` into the parent's
`node_modules`) exists precisely when the worktree has no dependency tree of
its own. If `node_modules` exists in the worktree, someone ran an install
inside it — the deliberate escape hatch — and a manifest difference no longer
implies a wrong tree. Conversely, if the parent checkout has no `node_modules`
at all, there is no shared tree and no mismatch to report — the instance runs
with whatever the branch installs (or fails to), which is a different problem
the process-liveness check already surfaces.

**The manifest list is Node-ecosystem only.** The walk-up sharing hazard is a
property of Node-style resolution; Go, Rust, and Python resolvers do not
search parent directories the same way. The list covers the root
`package.json` plus every lockfile that actually pins a tree. Workspaces
declare their packages in the root manifest, which is on the list; per-package
manifests below the root are out of scope and noted as a limitation.

## Package layout

```
pkg/worktree/worktree.go      # AddExclude: append a pattern to a worktree's local exclude file
pkg/worktree/worktree_test.go # AddExclude unit tests
pkg/verify/verify.go          # depManifests list, CheckDependencyIsolation, RunVerify wiring
pkg/verify/verify_test.go     # CheckDependencyIsolation unit tests
pkg/instance/up.go            # Step 1.5: scratch/ creation + AddExclude + summary line
pkg/instance/down.go          # best-effort scratch removal before worktree removal
pkg/instance/instance_test.go # scratch up/down tests
cmd/plax/e2e_test.go          # scratch presence + clean git status (optional, e2e)
docs/plans/index.md           # Add plan 18 to the phase table
```

## Type specifications

No new structs, no blueprint schema changes, no registry changes, no new
`Deps` fields.

### `pkg/worktree/worktree.go` — new function

```go
// AddExclude appends a pattern to a linked worktree's local exclude file
// (.git/worktrees/<name>/info/exclude). Best-effort: it never touches the
// repo's tracked .gitignore.
func AddExclude(worktreePath, pattern string) error
```

`worktreePath` is the absolute worktree path (as stored in `rec.WorktreePath`);
`pattern` is a single gitignore pattern line (e.g. `scratch/`). Returns an
error when `git rev-parse` or the file write fails; callers decide how to
surface it.

### `pkg/verify/verify.go` — new function and constant

```go
// depManifests are the root dependency-declaration files whose content
// determines a Node-ecosystem dependency tree. The lockfile entries make
// the common dependency-bump branch (lockfile-only change) detectable.
var depManifests = []string{
    "package.json",
    "package-lock.json",
    "npm-shrinkwrap.json",
    "bun.lockb",
    "bun.lock",
    "yarn.lock",
    "pnpm-lock.yaml",
}

func CheckDependencyIsolation(repoRoot, worktreePath string) []CheckResult
```

Both paths are absolute. The function is pure and static: file reads only, no
context, no git commands — it compares the two working trees as they are on
disk. All results carry `Layer: 1` like the other static checks.

## Algorithms

### `worktree.AddExclude` — append to the local exclude file

```
excludePath = run("git rev-parse --git-path info/exclude", Dir=worktreePath)
             # yields .git/worktrees/<name>/info/exclude for linked worktrees
             # in every gitdir layout (incl. --separate-git-dir)
data, err = os.ReadFile(excludePath); if IsNotExist: data = nil, err = nil
if err: return err
if pattern (with trailing newline) already in data: return nil   # idempotent
os.WriteFile(excludePath, data + pattern + "\n", 0644)
```

`⚠` `--git-path` resolves the worktree-local gitdir even when `.git` at the
repo root is a `gitdir:` file (separate-git-dir layout) — a hand-joined
`.git/worktrees/<name>` path would silently miss. `git worktree add` writes a
header comment to this file in modern git; appending is safe regardless.

`⚠` The write is non-atomic and this is accepted: the file is a local ignore
list, a torn write is recoverable by the user, and the pattern is short. The
registry file lock discipline does not apply to git-internal state.

### `instance.Up` — Step 1.5: scratch directory

Inserted after Step 1 (worktree create, up.go:89-97), before Step 2 (mailbox,
up.go:99-107):

```
scratchDir = filepath.Join(worktreePath, "scratch")
os.MkdirAll(scratchDir, 0755)
if err = worktree.AddExclude(worktreePath, "scratch/"); err != nil:
    warn on stderr "warning: cannot ignore scratch/ in worktree: <err>"
```

No rollback entry: the worktree-removal cleanup already removes the whole
worktree directory, scratch included.

`⚠` An `AddExclude` failure does not fail `up` — a warning is enough for a
best-effort ignore file. The consequence (unignored `scratch/` appearing in
`git status`) is visible and self-explanatory; failing the whole `up` for it
would block instance creation on a cosmetic git-file problem.

The summary block (up.go:406-432) gains one always-present line, printed
unconditionally after the conditional `logs:` line:

```
  scratch:   .plax/worktrees/<name>/scratch/
```

### `instance.Down` — best-effort scratch removal

Inserted at the start of Step 6 (worktree removal, down.go:113-118), before
`worktree.Remove`:

```
os.RemoveAll(filepath.Join(rec.WorktreePath, "scratch"))
# on error: warning to stderr, continue
```

`⚠` `git worktree remove --force` already deletes the entire worktree
directory, so this is normally a no-op on a path that no longer exists. It
covers the residual case where worktree removal fails (leftover directory):
without it, a failed `down` could strand a scratch tree full of agent debris.

### `verify.CheckDependencyIsolation` — detect the shared-tree mismatch

```
if os.Stat(worktreePath/node_modules) exists:
    return nil                            # own tree installed — nothing to flag
if os.Stat(repoRoot/node_modules) missing:
    return nil                            # no shared tree — nothing to compare

differing = []
present   = 0
for rel in depManifests:
    wtData, err = os.ReadFile(worktreePath/rel)
    if IsNotExist(err): continue          # branch does not declare this file
    if err: return failure "cannot read <rel> in worktree: <err>"
    present++
    parentData, perr = os.ReadFile(repoRoot/rel)
    if perr != nil and !IsNotExist(perr):
        return failure "cannot read <rel> in the parent working tree: <err>"
    if IsNotExist(perr) or !bytes.Equal(wtData, parentData):
        differing.append(rel)

if present == 0: return nil               # nothing declared — nothing to compare
if len(differing) == 0:
    return pass {Check: "dependency-isolation", Layer: 1,
                 Detail: "<present> manifest(s) match the parent tree the instance shares"}
return one failure per differing file:
    {Check: "dependency-isolation", Layer: 1, Passed: false,
     Detail: "instance shares the parent node_modules tree but <rel> differs — the instance runs the parent's dependencies, not this branch's. Install in the worktree or rebuild the instance",
     Artifact: rel}
```

`⚠` The gate is `node_modules` existing in the worktree, checked via
`os.Stat` (which follows symlinks). A deliberately symlinked `node_modules`
(pnpm-store style) counts as present and silences the check — acceptable: the
walk-up share case is exactly "no `node_modules` in the worktree". The
parent-side gate (no shared tree → no results) prevents a misleading failure
when the parent checkout never installed its dependencies.

`⚠` A worktree `node_modules` that is stale relative to a changed lockfile is
not detected. Plax cannot know when an in-tree install is stale; the check's
contract is "shared tree must match the branch", not "in-tree install must be
fresh".

`⚠` Binary lockfiles (`bun.lockb`) compare byte-for-byte; no parsing is
attempted.

`⚠` Sub-package manifests in a workspace (beneath the repo root) are not
compared. Workspace membership is declared in the root `package.json`, which
is compared; per-package manifests changing without a root-level change is a
scenario not worth second-guessing a workspace tool over.

### `verify.RunVerify` — wiring

```
results = append(results, CheckEnv(...)...)                    # as today
results = append(results, CheckDependencyIsolation(deps.RepoRoot, rec.WorktreePath)...)  # new, unconditional
```

The check runs for running **and** suspended instances — it is static and the
worktree files exist in both states. It is not gated on `RuntimeChecks` (that
flag governs the TCP probe only). Failures flow through the existing
`VerificationError` machinery: `up` stays up with `unhealthy`, `plax verify`
exits 1, `ls` shows `unhealthy` via the persisted record.

## CLI specification

No new commands, flags, or exit codes.

| Command | Change |
|---|---|
| `plax up <name>` | Creates `scratch/` in the worktree and adds it to the worktree's local exclude file. Summary block gains a `scratch:` line. A dependency-bump branch whose manifests differ from the parent tree reports `dependency-isolation` in the verification section; instance stays up, marked `unhealthy`, exit 1. |
| `plax down <name>` | Best-effort removal of `scratch/` before worktree removal. |
| `plax verify <name>` | Output may include `[pass]`/`[fail] dependency-isolation`. `--json` output includes the new `CheckResult`s. |

Commands with no change: `init`, `ls`, `status`, `resume`, `suspend`,
`rederive`, `attach`, `exec`, `send`, `recv`, `doctor`, `base *`.

### Example — `plax up i1` summary block

```
instance i1 up
  worktree:  .plax/worktrees/i1
  branch:    plax/i1
  database:  plax_i1
  ports:     PORT=3301 REDIS_PORT=26380
  logs:      .plax/logs/i1/
  scratch:   .plax/worktrees/i1/scratch/
```

### Example — `plax verify i1` on a dependency-bump branch

```
i1:
  [pass] env-completeness
  [pass] env-unresolved-holes
  [pass] env-scrubbed-leaks
  [fail] dependency-isolation: instance shares the parent node_modules tree but package.json differs — the instance runs the parent's dependencies, not this branch's. Install in the worktree or rebuild the instance
  [pass] process-liveness
  [pass] db-existence
  [pass] db-provenance
```

## Error handling

| Failure | Behavior |
|---|---|
| `git rev-parse --git-path` fails in `AddExclude` | warning on stderr; `scratch/` exists but unignored; `up` continues |
| exclude-file write fails in `AddExclude` | warning on stderr; `up` continues |
| exclude file already contains the pattern | no-op, success (idempotence) |
| `scratch/` creation fails | `up` returns the error → rollback runs (worktree removed) |
| `os.RemoveAll` on `scratch/` fails during `down` | warning to stderr; `down` continues |
| worktree `node_modules` exists | check returns no results (own tree installed) |
| repo root lacks `node_modules` | check returns no results (no shared tree to diverge from) |
| no manifests present in the worktree | check returns no results (nothing declared) |
| worktree manifest read error | failure, detail names path and error (cannot verify → fail loudly) |
| parent manifest read error | failure, detail names path and error — an unreadable file is never reported as "differs" |
| parent manifest missing or differs | failure per differing file, detail names the relative path |
| all present manifests identical | pass, detail states the shared tree matches |
| binary lockfile (`bun.lockb`) | byte comparison — no special handling |
| workspace sub-package manifest differs without root change | not detected — documented limitation |

## Tests

### `pkg/worktree/worktree_test.go`

- `TestWorktree_AddExclude_AppendsPattern` — real temp repo + `git worktree
  add`; after `AddExclude(wtPath, "scratch/")` the worktree's exclude file
  contains `scratch/` and the repo's tracked `.gitignore` is untouched.
- `TestWorktree_AddExclude_Idempotent` — second call with the same pattern
  leaves the file unchanged.
- `TestWorktree_AddExclude_MissingGitDir` — path that is not a git worktree
  returns an error.
- `TestWorktree_AddExclude_PreservesExistingEntries` — pre-existing pattern in
  the exclude file survives the append.

### `pkg/verify/verify_test.go`

- `TestVerify_DependencyIsolation_SharedTree_ManifestsMatch` — no
  `node_modules` in worktree, identical `package.json` + `package-lock.json`
  on both sides → single pass result.
- `TestVerify_DependencyIsolation_SharedTree_ManifestDiffers` — `package.json`
  differs → one failure naming `package.json`; the identical lockfile is not
  reported.
- `TestVerify_DependencyIsolation_SharedTree_LockfileDiffers` — only the
  lockfile differs (the dependency-bump case) → failure naming it.
- `TestVerify_DependencyIsolation_LocalNodeModules_Silent` — `node_modules`
  present in the worktree with differing manifests → no results.
- `TestVerify_DependencyIsolation_NoManifests_NoResults` — empty worktree →
  no results.
- `TestVerify_DependencyIsolation_ParentManifestMissing_Fails` — branch adds
  `package.json` the parent lacks → failure naming it.
- `TestVerify_DependencyIsolation_UnreadableManifest_Fails` — `chmod 000` on
  the worktree manifest → failure naming the path.
- `TestVerify_DependencyIsolation_UnreadableParentManifest_Fails` — `chmod
  000` on the parent manifest → failure naming the path, not a "differs"
  report.
- `TestVerify_DependencyIsolation_NoSharedTree_NoResults` — repo root without
  `node_modules` (and differing manifests) → no results.
- `TestVerify_RunVerify_IncludesDependencyCheck` — RunVerify on a record whose
  worktree shares a differing manifest → health `unhealthy`, error is a
  `*VerificationError` containing the `dependency-isolation` result.

### Fixture changes

- `pkg/instance/instance_test.go` — `initRepo` commits a `.gitignore`
  containing `.env`, so the derived `.env` does not dirty `git status` and the
  clean-status assertions hold.
- `cmd/plax/e2e_test.go` — `initFixtureRepo` keeps its committed `.env`
  (the quoting round-trip test needs it on disk), so the scratch e2e test
  asserts that `git status --porcelain` shows no `scratch` entry rather than
  being completely empty — the derived `.env` legitimately shows as modified.

### `pkg/instance/instance_test.go`

- `TestInstance_Up_ScratchCreatedAndExcluded` — after `Up`, `scratch/` exists
  in the worktree, the worktree's exclude file contains `scratch/`, and
  `git status` inside the worktree is clean.
- `TestInstance_Down_RemovesScratch` — after `Up` then `Down`, no `scratch/`
  residue exists and `assertNoResidue` passes.

### `cmd/plax/e2e_test.go`

- `TestEndToEnd_ScratchDirectory` — `plax up i1` on the sample repo;
  `scratch/` exists, `git -C .plax/worktrees/i1 status --porcelain` contains no
  `scratch` entry (the fixture's tracked `.env` still shows as modified);
  `plax down i1` removes it. Self-skips with the existing
  `PLAX_TEST_POSTGRES_URL` gate.

## Acceptance criteria

- [ ] `plax up i1` creates `.plax/worktrees/i1/scratch/` and prints a
      `scratch:` line in the summary block
- [ ] The worktree's local exclude file contains `scratch/`; the repo's
      tracked `.gitignore` is byte-identical before and after `up`
- [ ] `git status` inside the worktree is clean immediately after `up`
- [ ] `plax down i1` leaves no `scratch/` residue even when worktree removal
      fails
- [ ] `plax verify i1` on an instance whose branch manifests differ from the
      parent tree (and which has no own `node_modules`) reports
      `dependency-isolation` failing and names each differing file; the
      instance is marked `unhealthy`
- [ ] `plax verify i1` passes `dependency-isolation` when the shared tree's
      manifests match the branch's
- [ ] An instance that installed its own `node_modules` is not flagged by
      `dependency-isolation` regardless of manifest differences
- [ ] A branch that only changes a lockfile (no `package.json` change) is
      detected
- [ ] `plax up` on a dependency-bump branch stays up but exits 1 with the
      `dependency-isolation` failure visible in the verification section
- [ ] The check runs for suspended instances too (static, no runtime gate)
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (with and without
      `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

## Dependencies

No new external dependencies.

| Package | Import path | Purpose |
|---|---|---|
| `os/exec` | stdlib | `git rev-parse` in `worktree.AddExclude` — already imported in `pkg/worktree` |
| `bytes` | stdlib | manifest byte comparison in `CheckDependencyIsolation` |
| `os`, `path/filepath` | stdlib | file reads and path joins — already imported in `pkg/verify` |
