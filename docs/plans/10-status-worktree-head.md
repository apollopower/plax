# Fix 9 — status drift should measure worktree HEAD, not recorded branch

## Problem

`plax status` code drift checks `git rev-list <base>...<rec.Branch>` from the
repo root, using the branch plax created at `up` time. If the user checks out
a different branch (e.g. a PR to review) inside the worktree, status still
reports "up to date with main" regardless of actual divergence.

`schemaDrift` has the same blind spot: it hashes migration filenames at the
`base` ref, not at the worktree's actual HEAD. If the checked-out branch adds
a migration, both sides are "main", so it reports "migrations match main" while
the instance database is missing that migration.

## Design

### 1. New helper: `worktree.WorktreeHead(worktreePath string) (ref, commit string, err error)`

Runs `git -C <worktreePath> rev-parse --abbrev-ref HEAD` and
`git -C <worktreePath> rev-parse HEAD`. Runs from the **worktree's directory**,
not the repo root. Returns `ref=""` for detached HEAD (same convention as the
existing `HeadRef`).

- [ ] Add to `pkg/worktree/git.go`
- [ ] Add test in `pkg/worktree/worktree_test.go`

### 2. Resolve the worktree's HEAD in `codeDrift`

In `pkg/status/status.go`, `codeDrift` currently takes `(repoRoot, base string,
rec *registry.InstanceRecord)`. The right-hand side of the ahead/behind range
must become the worktree's actual HEAD commit instead of `rec.Branch`.

- [ ] Call `worktree.WorktreeHead(rec.WorktreePath)`. If the worktree path is
  gone (instance was torn down, worktree file was deleted), fall back to
  `rec.Branch` and note "worktree path missing" in detail.
- [ ] If the resolved commit matches the commit at `rec.Branch` (fast path:
  `git rev-parse <rec.Branch>` gives the same SHA), use `rec.Branch` directly
  so the ahead/behind detail string uses a human-readable branch name. This
  is the common case — the user hasn't changed HEAD — and avoids showing a
  bare SHA when the named branch is fine.
- [ ] Pass the resolved commit SHA (or branch name for the fast path) to
  `worktree.AheadBehind(repoRoot, base, rhs)`. Since git worktrees share
  object storage, `--left-right --count` from the repo root against a
  worktree commit SHA works correctly.
- [ ] Update the detail string:
  - Fast path (abbreviated ref matches recorded branch): unchanged, e.g.
    `"up to date with main"` / `"ahead 1, behind 0"`
  - Detached HEAD: `"ahead 1, behind 0 (detached at <short SHA>)"`
  - Named branch differs from `rec.Branch`: `"ahead 2, behind 0 (on pr-branch)"`
  - Worktree missing: fallback to recorded branch, note in detail

### 3. Fix `schemaDrift` to use worktree HEAD migration files

`schemaDrift` calls `worktree.SchemaFilesAtRef(repoRoot, base, migrationsDir)`
to hash the migration filenames at the `base` ref. This should instead use the
**worktree's current revision**, so migrations added by a checked-out branch
are detected.

- [ ] Resolve worktree HEAD commit (same helper from step 2) and pass it to
  `SchemaFilesAtRef` instead of `base`.
- [ ] Update detail string to reflect the ref being compared:
  `"database was built from a different migration set than worktree HEAD declares"`.
- [ ] Edge case: if worktree HEAD cannot be resolved (worktree gone), fall
  back to the current `base` behavior with a note.

### 4. Tests

- [ ] Unit test `worktree.WorktreeHead` for normal branch and detached HEAD.
- [ ] Unit test `codeDrift` with worktree on a branch that diverges from
  `rec.Branch`.
- [ ] Unit test `codeDrift` with worktree missing.
- [ ] Unit test `schemaDrift` with a migration added in the worktree HEAD
  that doesn't exist at `base`.

## Edge cases

| Scenario | Behavior |
|---|---|
| Worktree on `rec.Branch` (normal) | Use `rec.Branch` as RHS (fast path); no behavior change |
| Worktree on a different named branch | Use branch name; ahead/behind vs base from that branch |
| Worktree on detached HEAD | Use commit SHA; `(detached at abc1234)` in detail |
| Worktree path missing | Fall back to `rec.Branch`; note "worktree path missing" |
| `rec.Branch` (the git branch) has been deleted | `AheadBehind` will fail; surface the error in detail |
| `base` ref is a commit SHA (no branch name) | Same behavior as today; only RHS resolution changes |

## Files

- `pkg/worktree/git.go` — new `WorktreeHead` helper
- `pkg/worktree/worktree_test.go` — tests for the helper
- `pkg/status/status.go` — changes in `codeDrift`, `schemaDrift`, and
  likely `Build` (to pass the resolved ref to both dimensions)
