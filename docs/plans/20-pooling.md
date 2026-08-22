# Plan 20 — Instance pooling via `plax pool`

## Objective

Add a pooling layer on top of `up`/`down`/`suspend`/`resume` so agents can
acquire a pre-warmed, suspended instance instantly instead of waiting for a
full `up` (issue #67).

## Decisions recorded from triage 2026-08-21 and the design review

**Phase 4 ships as prescribed, with the plan number corrected to 20**
(`19-pooling.md` collides with `19-upgrade.md`). The issue's design stands:
pool state lives in the registry as a `pooled` flag on ordinary suspended
instances, `acquire` is the only admission point, no daemon, drift is
resolved at `acquire`. Three decisions were finalized with the maintainer:

**Drift is classified, and acquire never does more work than a fresh `up`.**
The registry stamp has three hashes (compose, env template, toolchain). A
pooled instance is stale in exactly two ways:

- **Shallow** (env template hash changed only): rederive `.env` in place —
  derivation is milliseconds — then resume. Acquire stays fast.
- **Deep** (compose or toolchain hash changed): the suspended instance's
  containers and DB clone are structurally stale, and `Rederive` only
  rewrites `.env`; it cannot repair them. Tear the instance down and run a
  fresh `up` instead.

That gives the failsafe bound the triage asked for: **acquire's worst case
equals the no-pool path** (a fresh `up`), never more. Deep-drift rebuilds
branch from the instance's recorded `SourceRef` when it still resolves,
else current HEAD.

**Pool membership is one flag and four observable states, no ledger.**
`InstanceRecord.Pooled` is set by `seed` and never cleared by `acquire` or
`release`. `Pooled && suspended` = available (in the pool); `Pooled &&
running` = in-use (acquired). The registry already records everything
`status` needs; no new state store and no registry version bump (`pooled`
is `omitempty`, so pre-existing registries load unchanged).

**Guardrails (finalized in this plan).** `resume` is the only refusal:
resuming a pooled instance directly would bypass `acquire`, the single
admission point. Everything else composes: `down` and `suspend` work on
pooled instances as-is (suspend is equivalent to `release`), `ls` marks
pooled members, the remaining commands are untouched. Full matrix in the
CLI section.

## Package layout

```
pkg/pool/pool.go        # Seed/Acquire/Release/Drain/Status, NextName, drift classification
pkg/pool/pool_test.go   # unit tests with per-package fakes (AGENTS.md convention)
pkg/instance/rederive.go# refactor: Rederive loops over new RederiveInstance(deps, name)
cmd/plax/main.go        # PoolCmd subtree + runPoolSeed/Acquire/Release/Drain/Status
cmd/plax/main_test.go   # CLI-level pool tests (guardrails, dispatch)
cmd/plax/guide.md       # "Command reference" gains the pool section
docs/manual.md          # Pooling section (seed/acquire/release/drain/status walkthrough)
README.md               # command list gains plax pool
docs/plans/index.md     # add plan 20 to the phase table
docs/plans/triage/2026-08-21.md # check the #67 box; fix the 19-pooling reference
AGENTS.md               # package table gains a `pool` row
```

`pkg/pool` is a new package rather than more `pkg/instance` surface:
`instance` stays the lifecycle primitive (up/down/suspend/resume/rederive);
the pool is the policy layer that composes it (blocking wait, drift
classification, name selection). Its per-package fakes follow the
"fakes are per-package" convention; the existing `instance` test helpers are
not exported for reuse.

## Type specifications

### `pkg/registry/registry.go` — one new field

```go
type InstanceRecord struct {
    // existing fields unchanged
    //
    // Pooled marks an instance created by `plax pool seed`. It is never
    // cleared by acquire or release; combined with State it yields the four
    // pool observables: Pooled+suspended = available, Pooled+running = in
    // use, !Pooled+suspended = ordinary suspended, !Pooled+running = ordinary.
    Pooled bool `json:"pooled,omitempty"`
}
```

Backward compatible: `omitempty` means old registries and records written by
older binaries simply lack the field. Registry `Version` stays 1.

### `pkg/pool/pool.go`

```go
// DriftKind classifies how a pooled instance's inputs diverged from the
// registry stamp.
type DriftKind int

const (
    DriftNone    DriftKind = iota
    DriftShallow           // env template changed; Rederive is sufficient
    DriftDeep              // compose or toolchain changed; rebuild required
)

// Drift compares the current stamp against the stored registry stamp.
func Drift(current, stored registry.BlueprintStamp) DriftKind

// NextName returns the lowest free poolN name ("pool1", "pool2", ...).
// Digits only — instance names forbid hyphens (validateName), so the issue's
// "pool-1" spelling is not representable.
func NextName(reg *registry.Registry) (string, error)

// Available returns the names of pooled-and-suspended instances, sorted.
func Available(reg *registry.Registry) []string

// Seed creates max(0, n - len(Available)) new instances, each derived and
// suspended with Pooled=true, and returns the names created (in order).
// The ref is resolved once at start. Stale pooled members are NOT rebuilt
// here: acquire is the decision point and rebuilds them on demand.
func Seed(ctx context.Context, bp *blueprint.Blueprint, bm instance.BaseManager,
    docker instance.DockerDriver, root, ref string, n int) ([]string, error)

// Acquire blocks until a pooled instance is available (bounded by timeout,
// 0 = wait forever), then makes it ready: no drift -> resume; shallow drift
// -> rederive env then resume; deep drift -> down + fresh up, re-pooled.
// Returns the acquired name.
func Acquire(ctx context.Context, bp *blueprint.Blueprint, bm instance.BaseManager,
    docker instance.DockerDriver, root string, timeout time.Duration) (string, error)

// Release suspends a pooled instance back into the pool. No-op when already
// suspended; error when the instance is not pool-managed.
func Release(ctx context.Context, deps *instance.Deps, name string) error

// Drain destroys every pooled instance. Refuses when any pooled instance is
// running (in use) unless force is set.
func Drain(ctx context.Context, deps *instance.Deps, force bool) (int, error)

// Counts returns the pool's available/in-use/stale tallies. stale is -1
// when current is zero-valued (blueprint unavailable — the caller prints
// unknown).
func Counts(reg *registry.Registry, current registry.BlueprintStamp) (available, inUse, stale int)
```

`Seed`/`Acquire` take the shared backends (`bm`, `docker`) directly instead
of an `*instance.Deps` because they open a fresh registry session per step
(see Algorithms); the CLI builds `bm`/`docker` once like `buildDeps` does.

### `pkg/instance/rederive.go` — refactor

```go
// RederiveInstance regenerates the .env of one instance. Returns whether
// the file changed. Extracted from Rederive so the pool can rederive a
// single pooled member at acquire.
func RederiveInstance(deps *Deps, name string) (changed bool, err error)

// Rederive keeps its signature and semantics: loops all instances in sorted
// order, warning on per-instance failure, calling RederiveInstance.
func Rederive(deps *Deps) error
```

## Algorithms

### Drift classification

```
Drift(current, stored):
    if !stamp.HasChanged(current, stored):         return DriftNone
    if current.ComposeHash != stored.ComposeHash ||
       current.ToolchainHash != stored.ToolchainHash:
        return DriftDeep       ⚠ compose/toolchain changes alter containers,
                                 ports, or the base — Rederive cannot repair
    return DriftShallow        ⚠ env-template-only: .env regeneration suffices
```

### NextName

```
for n := 1; ; n++:
    name := "pool" + n
    if _, exists := reg.Instances[name]; !exists:  return name
        ⚠ a user instance named poolN (non-pooled) is skipped, never stolen;
          the registry lock is held while the caller mutates, so nothing can
          take the name between selection and Up
```

### Seed

```
1. resolved := ResolveRef(root, ref) once      ⚠ all members share the ref
2. session 0: reg := Open; available := len(Available(reg)); reg.Close
3. toCreate := max(0, n - available)
4. for i in 1..toCreate:
     reg := Open(root)                          ⚠ one session per instance: the
                                                lock is released between
                                                instances, so an agent's
                                                acquire can take pool1 the
                                                moment it is suspended while
                                                pool2..N are still warming
     name := NextName(reg)
     pool := portpool.New(bp.PortPool.Start, bp.PortPool.End, reg)
     deps := {Blueprint: bp, Registry: reg, Pool: pool, BM: bm,
              Docker: docker, RepoRoot: root, SourceRef: ref, ResolvedRef: resolved}
     if err := instance.Up(ctx, deps, name):    ⚠ Up rolls back fully on
         warn; continue                          failure; the slot is skipped
     if err := instance.Suspend(ctx, deps, name):  ⚠ Up verifies health before
         warn; continue                          it returns, so pool members
                                                 are pre-verified
     rec := reg.Get(name); rec.Pooled = true
     reg.UpdateInstance(name, rec); reg.Save
     created = append(created, name); reg.Close
5. reg := Open; reg.BlueprintStamp = stamp.Compute(root, bp); reg.Save; reg.Close
     ⚠ one stamp covers the whole pool; all members go stale together
6. stdout: created names, one per line
   stderr: "seeded X of Y (pool has Z available)"
   return error when any member failed, nil otherwise
```

Partial failure is deliberate: a transient base failure should not waste the
other slots. `seed 0` (or fewer than available) reports and exits 0 without
creating anything. Stale members are left in place; `acquire` rebuilds them.

### Acquire (the single admission point)

```
1. ctx := signal-cancellable (SIGINT/SIGTERM, like runUp)
2. deadline := now + timeout; timeout = 0 → no deadline
3. waited := false
4. loop:
     reg := Open(root)
     for name in Available(reg):                ⚠ sorted; lowest first
         rec := reg.Get(name)
         kind := Drift(stamp.Compute(root, bp), reg.BlueprintStamp)
         switch kind:
         case DriftNone:
             err := instance.Resume(ctx, deps{Blueprint: bp, Registry: reg,
                 BM: bm, Docker: docker, RepoRoot: root}, name)
         case DriftShallow:
             _, err := instance.RederiveInstance(deps, name)
             if err == nil: err = instance.Resume(ctx, deps, name)
         case DriftDeep:
             stderr: "rebuilding stale pooled instance %s (blueprint inputs
                      changed)..."
             rref := ""
             if rec.SourceRef != "" && ResolveRef(rec.SourceRef) ok:
                 rref = rec.SourceRef            ⚠ preserve the member's ref
                                                 when it still exists; else HEAD
             instance.Down(ctx, deps, name)
             err := instance.Up(ctx, upDeps{..., SourceRef: rref, ResolvedRef: resolved}, name)
             if err == nil:
                 rec = reg.Get(name); rec.Pooled = true
                 reg.UpdateInstance(name, rec); reg.Save
         if err == nil:
             stdout: name; reg.Close; return name
         stderr: warning; continue              ⚠ a broken member (worktree
                                                deleted, port taken) is skipped;
                                                the next available is tried
     reg.Close
     if deadline passed: return error "no pooled instance available within %s
         — run 'plax pool seed N' or 'plax up <name>'"
     if !waited: stderr "waiting for a pooled instance... (Ctrl-C to abort,
         pass --timeout=SECONDS to bound the wait)"; waited = true
     select: ctx.Done → return error; ticker (1s) → loop
```

The wait loop opens and closes the registry on every poll, so **the lock is
never held across the wait** — a concurrent `seed` or `release` can refill
the pool while acquirers block. Inside one poll, the lock is held across the
mutating step (resume/rebuild), exactly like every other command holds it
across its work. Two racing acquirers serialize on the flock: the loser's
next poll sees the winner's record as running and picks the next member.

The acquired instance stays `Pooled: true` with `State: running` — that is
what `status` counts as in-use.

### Release

```
1. rec := reg.Get(name); !found → error
2. !rec.Pooled → error "instance %s is not a pooled instance (created with
   'plax pool seed')"
3. rec.State == suspended → "instance %s is already in the pool"; exit 0
4. instance.Suspend(ctx, deps, name)   ⚠ keeps Pooled=true; stops containers,
                                         kills processes, clears PIDs
5. stdout: "released %s to the pool"
```

### Drain

```
1. pooled := [rec for rec in reg.Instances if rec.Pooled]
2. inUse := [rec for rec in pooled if rec.State == running]
3. len(inUse) > 0 && !force → error "N pooled instance(s) in use (%s) — pass
   --force to destroy them anyway"
4. for rec in pooled: instance.Down(ctx, deps, rec.name)
       ⚠ Down is tolerant and idempotent per step; failures warn and continue
5. stdout: "destroyed %d pooled instance(s)"
```

`down` on a pooled member works today and implicitly removes it from the
pool (the record is deleted) — `drain` is the batch form, not a new
permission.

### Status

```
reg := Open
current := stamp.Compute(root, bp)   ⚠ blueprint missing → stale reported
                                     as "-" (Counts gets a zero stamp and
                                     the caller prints unknown)
available, inUse, stale := Counts(reg, current)
stdout (table):
  pool:
    available: 3
    in-use:    2
    stale:     0
  stale > 0 → note: "blueprint inputs changed — pooled instances are rebuilt
      on demand at 'plax pool acquire'"
JSON: {"available": 3, "in_use": 2, "stale": 0}
exit 0 always
```

## CLI specification

Kong subtree under `Pool`, mirroring the `Base` subtree:

```
plax pool seed <n> [--ref <ref>] [--root <dir>]
plax pool acquire [--timeout <seconds>] [--root <dir>]
plax pool release <name> [--root <dir>]
plax pool drain [--force] [--root <dir>]
plax pool status [--json] [--root <dir>]
```

| Command | stdout (records) | stderr (chatter) | Exit |
|---|---|---|---|
| `seed <n>` | created names, one per line | per-instance progress, "seeded X of Y", ref notes | 0 all created; 1 any failed |
| `acquire` | the acquired name | "waiting for a pooled instance...", rebuild notices, warnings | 0 acquired; 1 timeout/error |
| `release <name>` | "released %s to the pool" / "already in the pool" | suspend progress | 0; 1 not pooled/not found |
| `drain` | "destroyed N pooled instance(s)" | teardown progress | 0; 1 in-use without --force / registry failure |
| `status` | table or JSON | staleness note | 0 always |

`--timeout` accepts an integer number of seconds; 0 (default) waits
indefinitely. `--ref` defaults to the repo's current HEAD, resolved once for
the whole seed.

### Guardrail matrix (finalized)

| Command on a pooled member | Behavior |
|---|---|
| `pool acquire` | the only admission point; takes Pooled+suspended, makes it ready |
| `pool release <name>` | suspend a running member back into the pool |
| `pool drain` | batch destroy; refuses in-use members unless `--force` |
| `down <name>` | allowed; destroys the member and implicitly removes it from the pool (record deleted) |
| `suspend <name>` | allowed; equivalent to `release` — the member stays pooled |
| `resume <name>` | **refused** when Pooled+suspended: "instance %s is in the pool — run 'plax pool acquire'" (acquire stays the single admission point); a running pooled member gets the ordinary already-running error |
| `up <name>` | ordinary name collision handling; a user-created `poolN` is skipped by `NextName`, never stolen |
| `ls` | STATE column renders `pooled` for Pooled+suspended (display-only; registry State stays `suspended`), `running` for in-use members; JSON carries the raw `pooled` field |
| `status <name>` | unchanged — six-dimension report for a suspended instance |
| `attach` / `exec <name>` | unchanged — suspended members get the existing "suspended" note; work once acquired |
| `rederive` | unchanged — regenerates pooled members' .env like any other instance |
| `verify <name>` | unchanged — runtime checks skipped while suspended |
| `doctor` | unchanged |
| `send` / `recv <name>` | unchanged — mailbox is name-based |

### Examples

```
$ plax pool seed 3
pool1
pool2
pool3

$ plax pool acquire
pool1

$ plax pool status
pool:
  available: 2
  in-use:    1
  stale:     0

$ plax pool release pool1
released pool1 to the pool

$ plax pool drain
destroyed 3 pooled instance(s)
```

## Error handling

| Failure | Behavior |
|---|---|
| `seed`: `Up` fails for one name | warn; continue remaining slots; exit 1 at the end; successful names still printed |
| `seed`: base missing or unlocked | `Up` errors per instance; exit 1 |
| `seed`: name `poolN` collides with a non-pooled user instance | slot skipped with a warning; next free name used |
| `acquire`: pool empty, timeout hit | "no pooled instance available within %s — run 'plax pool seed N' or 'plax up \<name\>'"; exit 1 |
| `acquire`: `Resume` fails for one candidate (port taken, Docker down, container gone) | warn; try the next available; if none succeeds, return the last error; exit 1 |
| `acquire`: shallow rederive fails (template missing, derivation error) | warn; try the next available |
| `acquire`: deep-drift rebuild's `Up` fails | the stale instance is already torn down; warn; try the next; if none, return the error |
| `acquire`: interrupted while waiting (SIGINT) | context cancellation error; exit 1; nothing mutated |
| `release`: instance not pooled | error "instance %s is not a pooled instance (created with 'plax pool seed')"; exit 1 |
| `release`: instance already suspended | "instance %s is already in the pool"; exit 0 |
| `drain`: in-use members and no `--force` | error naming them; exit 1; nothing destroyed |
| `drain`: per-member `Down` failures | `Down` is tolerant; warnings; exit 1 only if registry removal fails |
| `status`: blueprint unavailable | stale printed as "-"; available/in-use still shown |
| registry lock contended | poll blocks on the flock, then re-reads — the wait never holds the lock |

## Tests

### `pkg/pool/pool_test.go`

Fakes: per-package hand-rolled fakes for `instance.BaseManager` and
`instance.DockerDriver` (call-recording, `sync.Mutex`-guarded, following
AGENTS.md). A real `registry.Registry` on a `t.TempDir()` and a real
`portpool` over it. Test names follow `TestPool_<Behavior>`.

- `TestPool_NextName_FirstFree` — empty registry → `pool1`.
- `TestPool_NextName_SkipsTaken` — `pool1`..`pool3` exist → `pool4`.
- `TestPool_NextName_SkipsUserInstance` — a non-pooled `pool2` exists → `pool1`.
- `TestPool_Available_Sorted` — mixed records → only Pooled+suspended, sorted.
- `TestPool_Drift_None` / `TestPool_Drift_Shallow` / `TestPool_Drift_Deep` —
  stamp comparisons for the three classes (deep wins when compose and env
  both changed).
- `TestPool_Seed_CreatesN` — `Seed(3)` → `pool1..pool3` created, suspended,
  `Pooled=true`, registry stamp updated, names returned in order.
- `TestPool_Seed_EnsuresAvailable` — 2 available, `Seed(3)` → creates only
  `pool3`.
- `TestPool_Seed_NoopWhenSatisfied` — 3 available, `Seed(2)` → empty result,
  nil error.
- `TestPool_Seed_PartialFailure` — fake BM fails on the 2nd `Up` → 1 name
  returned, non-nil error.
- `TestPool_Seed_ReleasesLockBetweenInstances` — a goroutine acquires `pool1`
  between seed iterations (observable via the fake's call order / a registry
  check mid-seed) → interleaving works.
- `TestPool_Acquire_PicksLowest` — only `pool2` available → returns `pool2`,
  State running, `Pooled` stays true.
- `TestPool_Acquire_EmptyTimeout` — empty pool, `timeout=50ms` → error naming
  seed/up.
- `TestPool_Acquire_BlocksUntilRelease` — goroutine calls `Release` after
  ~200ms; `Acquire` with a 2s timeout → returns the released name.
- `TestPool_Acquire_BlocksUntilSeed` — goroutine seeds 1 after ~200ms;
  `Acquire` returns it.
- `TestPool_Acquire_ShallowDrift_Rederives` — env template rewritten after
  seed; acquire re-derives the member's `.env` (content asserted) before
  resume, resume succeeds.
- `TestPool_Acquire_DeepDrift_Rebuilds` — compose hash changed; acquire
  downs the stale member and ups a fresh one; result is `Pooled=true` and
  running.
- `TestPool_Acquire_DeepDrift_RefPreserved` — stale member's `SourceRef`
  still resolves → the rebuild passes that ref to `Up` (fake records it).
- `TestPool_Acquire_ResumeFail_TriesNext` — fake Docker fails on `pool1`'s
  containers, `pool2` succeeds → returns `pool2`, warning on stderr.
- `TestPool_Release_Running` — running pooled member → suspended, `Pooled`
  stays true.
- `TestPool_Release_AlreadyInPool` — suspended member → no-op, nil error.
- `TestPool_Release_NotPooled` — ordinary instance → error.
- `TestPool_Release_NotFound` — missing instance → error.
- `TestPool_Drain_RefusesInUse` — one running pooled member → error naming
  it; nothing destroyed.
- `TestPool_Drain_Force_DestroysInUse` — `force=true` → all destroyed.
- `TestPool_Drain_AvailableOnly` — only suspended members → destroyed, count
  returned.
- `TestPool_Status_Counts` — mix of pooled/running/pooled+suspended/ordinary
  + stale stamp → (available, inUse, stale) correct.
- `TestPool_Status_UnknownStale` — zero stamp (no blueprint) → stale -1.

### `cmd/plax/main_test.go`

- `TestPoolCLI_ResumeGuardrail` — pooled+suspended record in a temp
  registry; `plax resume <name>` refuses with the acquire hint.
- `TestPoolCLI_SuspendKeepsPooled` — `suspend` on a running pooled member
  keeps `Pooled` true (release-equivalence).
- `TestPoolCLI_Acquire_StdoutName` — acquire prints only the name to stdout.

### E2E — `cmd/plax/e2e_test.go` (skips without `PLAX_TEST_POSTGRES_URL`)

`TestE2E_Pool_FullCycle` — `pool seed 2` → `pool status --json` shows
`available: 2` → `pool acquire` returns a name → `exec` runs in it →
`pool release` → second `acquire` (after `plax.json` env-template change)
rederives → `pool drain` → `ls` shows no pooled members.

### Fixtures

No repo fixtures: env templates and stamps are written into `t.TempDir()`
repos with a minimal `plax.json` (patterned on `instance_test.go`'s
`testBlueprint()`).

## Acceptance criteria

- [ ] `plax pool seed 3` creates `pool1`..`pool3` suspended with `pooled:
      true`; names on stdout, one per line; exit 0
- [ ] `plax pool seed 3` when 2 are already available creates only 1 more
- [ ] `plax pool acquire` resumes the lowest available member and prints its
      name on stdout; the record keeps `pooled: true`
- [ ] `plax pool acquire --timeout 10` on an empty pool exits 1 with a
      message pointing at `plax pool seed` / `plax up`
- [ ] `plax pool acquire` on an empty pool with no timeout blocks, prints
      one "waiting for a pooled instance..." notice, and returns when a
      concurrent `release`/`seed` refills the pool
- [ ] After an env-template-only `plax.json` change, `acquire` rederives the
      member's `.env` before resuming (verified by file content)
- [ ] After a `docker-compose.yml` change, `acquire` tears down the stale
      member and rebuilds it (from the recorded ref when it resolves, else
      current HEAD), keeping it pooled
- [ ] `plax pool release <name>` suspends a running member back into the
      pool; release on a non-pooled instance exits 1
- [ ] `plax pool drain` refuses with a message naming in-use members; with
      `--force` it destroys them
- [ ] `plax pool status` and `plax pool status --json` report available,
      in-use, and stale
- [ ] `plax resume <name>` on a pooled member refuses with guidance to
      `plax pool acquire`
- [ ] `plax down <name>` destroys a pooled member and removes it from the
      pool
- [ ] `plax ls` shows pooled available members with state `pooled` (raw
      registry state stays `suspended`)
- [ ] Registries written before this change load unchanged; `pooled` is
      absent from their records (registry version stays 1)
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (with and without
      `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

## Dependencies

No new external dependencies. Stdlib only:

| Package | Import path | Purpose |
|---|---|---|
| `context` | stdlib | cancellation and timeout for blocking acquire |
| `time` | stdlib | wait ticker, deadline |
| `sort` | stdlib | deterministic member selection |
| `fmt`, `os`, `strconv` | stdlib | output and naming |
| `regexp` | stdlib | `^pool(\d+)$` member-name recognition |

