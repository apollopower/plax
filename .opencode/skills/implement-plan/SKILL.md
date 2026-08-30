---
name: implement-plan
description: Use when a scoped implementation plan in docs/plans/ needs to be carried out — "implement plan 21", "tackle docs/plans/22-work-records.md", "do #75" (resolve the issue to its linked plan first). Reads the plan's 9 sections as a contract, implements bottom-up with its named tests, runs make check, updates guide/manual/index/triage checkboxes, and commits one commit per plan. Not for writing or editing plans.
---

# Implement a plax plan

A plan in `docs/plans/NN-slug.md` is the contract for one unit of work.
Your job is to deliver exactly what it specifies — nothing more — and to
verify it honestly.

## 0. Orient — read everything before touching code

1. Read the target plan fully. If the user referenced an issue number
   ("#75") or a triage doc instead of a plan, follow the link in
   `docs/plans/triage/` to the plan doc first.
2. Read the triage snapshot the plan cites for intent, priority, and scope
   boundaries.
3. Read `docs/plans/index.md` — the phase table entry and the "Plan file
   conventions" section (section order, `⚠`/`→` markers, test naming).
4. Recon the live code: every file in the plan's Package layout, the Kong
   structs in `cmd/plax/main.go`, and the `cmd/plax/guide.md` /
   `docs/manual.md` sections the plan touches.
5. If the plan contradicts the current code (stale, or reality diverged),
   STOP: update the plan first, or surface the conflict to the user. Do not
   silently re-interpret the plan.

## 1. Plan the work

Build a todo list (todowrite) in bottom-up order: leaf packages first, then
wiring, then CLI, then docs. Use the plan's Package layout as the file list
and the Tests section for named test todos. Group each package's tests with
the package.

## 2. Implement

- **Type specifications**: replicate structs, fields, JSON tags, and
  validation rules exactly. No extra exported surface.
- **Algorithms**: follow the steps; every `⚠` edge case must be handled —
  those are the bugs the plan already foresaw.
- **CLI specification**: exact flags, args, exit codes, stdout records /
  stderr chatter, `--json` where specified.
- **Error handling**: each table row is a requirement — wire the failure
  mode to the specified behavior (message, exit code, rollback), following
  the `→` cause→effect convention. Use `%w` wrapping and `errors.Is` where
  the codebase does.
- **Comments**: explain *why*, never *what*. Add a comment only for a
  non-obvious edge case, trade-off, or external constraint; do not restate
  the code.
- **Dependencies**: add a module only if the plan's Dependencies section
  lists it; use the pinned version; run `go mod tidy`.
- **Tests**: implement the named tests. Repo conventions: `Test<Package>_<Behavior>`
  names, `t.Helper()` in setup helpers, go-cmp golden files under
  `testdata/<fixture>/`, hand-rolled mutex-guarded fakes, e2e in
  `cmd/plax/e2e_test.go` self-skipping without `PLAX_TEST_POSTGRES_URL`.
- **Concurrency**: CSP — goroutines, channels, select. A `sync.Mutex` is
  acceptable only inside test fakes.
- **Scope**: respect boundaries the plan states ("out of scope", "later
  plan"). No gold-plating.

## 3. Verify

1. `make check` (fmt + vet + lint + test) must be clean. It does *not* run
   `go mod tidy` or `gofmt -s`; run `go mod tidy` and `go test -race
   -count=1 ./...` yourself when the plan adds or changes dependencies.
2. If the plan adds CLI surface, build the binary and smoke-test new flags
   against the acceptance criteria where cheap (exit codes, stdout/stderr
   separation, `--json` shape).
3. E2E: if `PLAX_TEST_POSTGRES_URL` is set, run the e2e tests and require
   them to pass. If not, say so explicitly in the report — never claim e2e
   passed when it self-skipped.

## 4. Update docs and status

The docs files in the plan's Package layout are deliverables, not afterthoughts:

- `cmd/plax/guide.md` — the agent-facing operational reference
  (new/changed commands and flags).
- `docs/manual.md` — the human-facing guide.
- `docs/plans/index.md` — the phase table entry for this plan.
- The triage snapshot's acceptance criteria — tick `[x]` the rows that are
  now true ("resolved", follow-ups filed).
- The plan's own Acceptance criteria — tick `[x]` only what you actually
  verified: the `go vet` / `go test` / `golangci-lint` rows by a clean
  `make check`; e2e rows only if e2e ran and passed.

## 5. Commit

One commit per plan. (Committing is part of this workflow's contract;
it overrides the default "do not commit unless asked" rule — but stay
within it: do not push.) Inspect `git status` and `git diff` first; stage only
intended files; never commit secrets. Follow the repo's conventional-commit
style, e.g.:

```
feat: apply instance migrations during up (fixes #75)
```

or `feat: <plan objective summary> (implements plan NN)`. Include the docs
updates and checkbox ticks in the same commit. Do not push — report the
commit SHA.

## 6. Report

Final message: one line per deliverable implemented, verification status
(`make check`; e2e ran or skipped), acceptance criteria ticked, scope
boundaries respected, commit SHA.