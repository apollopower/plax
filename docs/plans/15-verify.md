# Plan 15 — Instance verification: unconditional self-checks on up + `plax verify`

## Objective

Add a `pkg/verify` package that runs static and runtime checks against an
instance after creation (and on `resume`, `rederive`, and the new
`plax verify <name>` command), catching silent correctness bugs before the
instance is trusted. The checks require no blueprint changes and no repo
cooperation. Introduce a `Health` field on instance records so unhealthy
state is visible in `ls` and `status`.

---

## Package layout

```
pkg/verify/
  verify.go             # CheckResult, VerificationError, CheckEnv, CheckServices, CheckProcesses, CheckDatabases, RunVerify
  verify_test.go        # Unit tests for each check function (package verify)
pkg/registry/
  registry.go           # InstanceRecord gains Health, VerifiedAt; new Health type + constants
pkg/derive/postgres/
  basemanager.go        # assertSeedNonEmpty helper; called from SeedBase and RefreshBase
pkg/instance/
  instance.go           # BaseManager interface gains InstanceProvenance, InstanceDBExists
  up.go                 # Step 4.5 env checks (fail fast); registry-record rollback cleanup; Step 8.5 RunVerify
  resume.go             # Env checks after .env parse; RunVerify after settle
  rederive.go           # CheckEnv per rederived .env; persist health via Registry.Save
pkg/status/
  status.go             # Report gains Health dimension (five → six)
cmd/plax/
  main.go               # New VerifyCmd + runVerify; runResume opens BM before Resume; ls HEALTH column; six-dimension status output and help text
AGENTS.md               # status package row: five-dimension → six-dimension
docs/plans/
  index.md              # Add plan 15 to phase table
```

---

## Type specifications

### `pkg/registry/registry.go` — `InstanceRecord`

```go
type InstanceRecord struct {
    // ... existing fields unchanged ...

    Health     Health     `json:"health,omitempty"`
    VerifiedAt *time.Time `json:"verified_at,omitempty"`
}
```

```go
type Health string

const (
    HealthHealthy   Health = "healthy"
    HealthUnhealthy Health = "unhealthy"
)
```

`Health` — the result of the last verification (`"healthy"` or
`"unhealthy"`). Default (empty string) means the instance predates the
verification feature and has never been checked. `VerifiedAt` is a pointer
because `encoding/json`'s `omitempty` does not omit a zero `time.Time`
struct — a plain `time.Time` field would write
`"verified_at": "0001-01-01T00:00:00Z"` into every never-verified record.

### `pkg/instance/instance.go` — `BaseManager` interface

```go
type BaseManager interface {
    BaseStatus(ctx context.Context) (postgres.BaseInfo, error)
    CloneBase(ctx context.Context, targetDB string) error
    DropInstanceDB(ctx context.Context, dbName string) error
    InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error) // new
    InstanceDBExists(ctx context.Context, dbName string) (bool, error)                      // new
}
```

Both methods already exist on `*postgres.BaseManager`; the interface needs
them because `Up` builds `verify.Deps{BM: deps.BM}` and a value typed as
the narrower interface would not satisfy `verify.BMInterface`. Hand-rolled
fakes in `pkg/instance` tests gain the two methods.

Also update the `Deps` doc comment: `Resume` gains "BM optional — nil
skips DB checks".

### `pkg/verify/verify.go` — `CheckResult`

```go
type CheckResult struct {
    Check    string `json:"check"`               // e.g. "env-completeness", "tcp-reachability"
    Layer    int    `json:"layer"`               // 1 (unconditional) or 2 (repo hook, future)
    Passed   bool   `json:"passed"`
    Detail   string `json:"detail"`              // human-readable message
    Artifact string `json:"artifact,omitempty"`  // path/port/pid that failed
}
```

### `pkg/verify/verify.go` — `VerificationError`

```go
type VerificationError struct {
    Results []CheckResult
    Layer   int // highest failing layer: 1 or 2
}

func (e *VerificationError) Error() string {
    // One line, failing checks only:
    // "verification failed: env-completeness (SENDGRID_API_KEY), tcp-reachability (127.0.0.1:26380)"
}
```

The one-line format matters: `main` routes errors through
`ctx.FatalIfErrorf`, which prints `plax: error: <Error()>` on the way to
exit 1. The multi-line per-check detail is printed to stderr separately by
the caller (see flow sections); `Error()` must not repeat it.

### `pkg/verify/verify.go` — `RunVerify`

```go
func RunVerify(ctx context.Context, deps *Deps, name string) ([]CheckResult, error)
```

Returns results and either nil (all executed checks passed) or
`*VerificationError` (at least one check failed). Persists `Health` and
`VerifiedAt` to the registry in both cases.

### `pkg/verify/verify.go` — `Deps`

```go
type Deps struct {
    Blueprint *blueprint.Blueprint
    Registry  *registry.Registry
    BM        BMInterface // nil → DB checks skipped
    RepoRoot  string
}

type BMInterface interface {
    InstanceDBExists(ctx context.Context, dbName string) (bool, error)
    InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error)
}
```

### `pkg/verify/verify.go` — check functions

Plain functions with seams injected as parameters — no func-type
indirection:

```go
func CheckEnv(templatePath, userEnvPath, derivedEnvPath string, holes map[string]string, scrub map[string]bool) []CheckResult
func CheckServices(ctx context.Context, services map[string]blueprint.ServiceDef, allocated map[string]int) []CheckResult
func CheckProcesses(pids map[string]int, isAlive func(int) bool) []CheckResult
func CheckDatabases(ctx context.Context, dbNames []string, bm BMInterface) []CheckResult
```

- `CheckProcesses` takes `isAlive` so unit tests inject a fake; production
  callers pass `process.IsAlive`.
- `CheckDatabases` takes `dbNames` as `[]string` — exactly what
  `registry.DBNamesFromRecord` returns.
- `CheckServices` dials real TCP; unit tests bind real loopback listeners
  (`net.Listen("tcp", "127.0.0.1:0")`), so no dial seam is needed.

---

## Algorithms

### Env derivation checks — `CheckEnv`

Called after `.env` derivation in `Up`, after `.env` parse in `Resume`,
after each rewrite in `Rederive`, and inside `RunVerify`. All checks read
files only; no network access. `CheckEnv` fans out to three unexported
helpers (`checkEnvCompleteness`, `checkEnvNoUnresolved`,
`checkEnvNoScrubbedLeaks`) and concatenates their results.

**1. Completeness check** — `checkEnvCompleteness`:

- Parse the template file. Parse the user's `.env` file.
- Build the expected key set:
  `(template-keys ∪ user-env-keys ∪ hole-keys) − scrub-set`.
  Hole keys belong in the set regardless of template presence — derivation
  appends holes missing from the template.
- Parse the derived `.env`. For each expected key, verify it appears in
  the derived output.
- ⚠ A key in both `scrub` and `holes` is excluded — the scrub-set
  subtraction applies last, and wins.
- Return one result per missing key:
  ```
  {Check: "env-completeness", Passed: false,
   Detail: "key NAME is missing from derived .env",
   Artifact: "NAME"}
  ```

**2. Unresolved hole check** — `checkEnvNoUnresolved`:

- Read the entire derived `.env` text. Search for `{{` substrings.
- ⚠ A `{{` in a comment line (`#`) is not an unresolved hole — it's a
  literal curly brace in a comment. Skip comment lines.
- Return one result per occurrence:
  ```
  {Check: "env-unresolved-holes", Passed: false,
   Detail: "unresolved template hole {{VAR}} survives in derived .env",
   Artifact: "derived .env"}
  ```

**3. Scrubbed value leak check** — `checkEnvNoScrubbedLeaks`:

- For each key in the `scrub` set, read its value from the user's `.env`.
- If the user's value is non-empty, search the entire derived `.env` text
  for that value (as a substring).
- ⚠ A scrubbed value that equals the template placeholder (e.g., both are
  `"placeholder"`) is considered a pass — the developer already has only
  the placeholder.
- Return one result per leak:
  ```
  {Check: "env-scrubbed-leaks", Passed: false,
   Detail: "scrubbed key NAME's real value appears in derived .env",
   Artifact: "derived .env"}
  ```

### TCP reachability check — `CheckServices`

Called after the settle delay in `Up` and `Resume`, and from `RunVerify`
for running instances:

1. For each service with `isolation == "dedicated"` (the only isolation we
   can TCP-connect to — logical Postgres is always on localhost:5432, and
   native processes get a process check instead):
   - Get the allocated port from the registered ports map.
   - Dial `127.0.0.1:<port>` with a 2-second timeout. Close immediately
     on success.
   - ⚠ Services behind a slow startup (Redis booting, Gotenberg) may not
     be ready at settle+300ms. A TCP dial that gets `connection refused` IS
     the check for a skipped/not-started service. The settle delay already
     covers the "crashed immediately" case. For slow startup, the check
     retries once after 1 second.
   - Return one result per failure:
     ```
     {Check: "tcp-reachability", Passed: false,
      Detail: "service SERVICE on port PORT is not reachable",
      Artifact: "127.0.0.1:PORT"}
     ```

### Process liveness check — `CheckProcesses`

The existing settle check already verifies processes are alive at
settle+300ms. The verification layer formalizes this as a check rather
than a fatal error:

1. For each registered PID (from the registry or freshly spawned):
   - Call `isAlive(pgid)` — `process.IsAlive` in production, a fake in
     tests.
   - Return one result per failure:
     ```
     {Check: "process-liveness", Passed: false,
      Detail: "process PROC (PGID=pgid) is not alive",
      Artifact: "PROC"}
     ```

### Database checks — `CheckDatabases`

Called after the settle delay, and from `RunVerify` when `deps.BM != nil`:

**1. Existence check**: For each database name from
`registry.DBNamesFromRecord(rec)`:

- Call `bm.InstanceDBExists(ctx, dbName)`.
- Return per-missing result:
  ```
  {Check: "db-existence", Passed: false,
   Detail: "database NAME does not exist",
   Artifact: "NAME"}
  ```

**2. Provenance check**: For each existing database:

- Call `bm.InstanceProvenance(ctx, dbName)`.
- If `prov == nil`, the provenance table is missing — the database was
  dropped and recreated externally.
- ⚠ This deliberately does NOT compare provenance versions against the
  base. That's the `status` package's job (the "Data" dimension). The
  verify check only asserts that provenance *exists at all*, proving the
  database is a valid plax clone.
- Return per-missing result:
  ```
  {Check: "db-provenance", Passed: false,
   Detail: "database NAME has no provenance table — it may have been dropped and recreated externally",
   Artifact: "NAME"}
  ```

### `RunVerify` — unified entry point

```
function RunVerify(ctx, deps, name) -> ([]CheckResult, error):
    rec, found = deps.Registry.GetInstance(name)
    if !found: return nil, "instance %q not found"

    results = []

    // Env checks (static; valid for running and suspended instances)
    templatePath = join(deps.RepoRoot, deps.Blueprint.Env.Template)
    userEnvPath  = join(deps.RepoRoot, ".env")
    derivedPath  = join(rec.WorktreePath, ".env")
    scrub        = buildScrubSet(deps.Blueprint)
    results += CheckEnv(templatePath, userEnvPath, derivedPath,
                        deps.Blueprint.Env.Holes, scrub)

    // Runtime checks only make sense on a running instance. A suspended
    // instance has stopped containers and processes BY DESIGN — TCP and
    // liveness checks would report false failures.
    if rec.State == registry.StateRunning:
        results += CheckServices(ctx, deps.Blueprint.Services, rec.Ports)
        results += CheckProcesses(rec.PIDs, process.IsAlive)

    // DB checks need Postgres. BM is nil when the caller could not
    // connect (tolerated degradation on resume/verify) — skip, don't fail.
    if deps.BM != nil:
        results += CheckDatabases(ctx, registry.DBNamesFromRecord(rec), deps.BM)

    allPassed = all(r.Passed for r in results)

    now = time.Now()
    rec.VerifiedAt = &now
    rec.Health = allPassed ? HealthHealthy : HealthUnhealthy
    deps.Registry.UpdateInstance(name, rec)
    if err := deps.Registry.Save(); err != nil:
        return results, fmt.Errorf("saving verification results: %w", err)

    if !allPassed:
        return results, &VerificationError{Results: results, Layer: 1}
    return results, nil
```

⚠ Skipped checks produce no results at all — a suspended instance's
runtime checks and a nil-BM run's DB checks are simply absent from the
slice. Callers announce skips on stderr (see CLI spec). Health is computed
only from checks that ran, so a suspended instance is not falsely marked
unhealthy for being suspended.

⚠ Health is persisted even when checks fail — durably recording the
failure is the point of the feature.

### Flow integration: `Up`

**Step 4.5: static env checks — fail fast, WITH rollback.**

```
// Step 4.5: verify the derived .env before spending boot cost on clones
// and containers.
results := verify.CheckEnv(templatePath, overridesPath, envPath,
                           deps.Blueprint.Env.Holes, scrub)
if anyFailed(results) {
    printVerificationErrors(results)   // stderr, one line per failure
    return &verify.VerificationError{Results: results, Layer: 1}
}
```

Returning the error lets the deferred rollback run: worktree (and with it
the derived `.env`), mailbox, network, and ports are all removed, and no
registry record is ever written. This is deliberate — nothing expensive
has started, and every failing check names the offending key, so the
printed output carries the debugging information the deleted `.env` would
have held. The "stays up for debugging" guarantee applies to runtime
failures only.

**Step 8: registry write (as today) + registry rollback cleanup.**

Today nothing fallible follows `AddInstance`, so no cleanup removes the
record. Verification adds such a path (`RunVerify`'s internal `Save` can
fail), so the write gains a cleanup:

```
cleanups = append(cleanups, func() {
    _ = deps.Registry.RemoveInstance(name)   // also drops the instance's port allocations
    if err := deps.Registry.Save(); err != nil {
        fmt.Fprintf(os.Stderr, "rollback: remove registry record: %v\n", err)
    }
})
```

Appended last, it runs first in the reverse-order rollback, so a failure
after the write can never leave a registry record pointing at torn-down
worktrees, databases, and containers. (The port-pool `Release` cleanups
run after it; `RemoveInstance` already deleted the instance's allocations,
so those releases are no-ops on the registry map while still correct for
the pool's own bookkeeping.)

**Step 8.5: runtime verification — failure keeps the instance up.**

Print the normal summary first (unchanged block), then verify:

```
results, err := verify.RunVerify(ctx, &verify.Deps{
    Blueprint: deps.Blueprint, Registry: deps.Registry,
    BM: deps.BM, RepoRoot: deps.RepoRoot,
}, name)
var verr *verify.VerificationError
if errors.As(err, &verr) {
    printVerificationErrors(verr.Results)     // stderr, one line per failure
    fmt.Fprintf(os.Stderr, "instance %s is up but unhealthy — investigate, "+
        "then 'plax verify %s' to re-check or 'plax down %s' to tear down\n",
        name, name, name)
    success = true    // bypass the rollback deferral...
    return verr       // ...but still exit 1 via the CLI layer
}
if err != nil {
    return err   // e.g. RunVerify's registry Save failed — rollback runs
}
success = true
return nil
```

⚠ `success = true` AND `return verr` together are the mechanism: the
deferred cleanup is skipped (the instance stays up; RunVerify already
persisted `unhealthy`) while kong still exits 1 via `FatalIfErrorf`.
Returning the error without setting `success` would tear the instance
down; setting `success` and returning nil would exit 0. The plan requires
both.

⚠ `RunVerify` re-runs the env checks Step 4.5 already ran. The
duplication is cheap (file reads only) and lets `up` and `plax verify`
share one code path. The registry is written before `RunVerify` runs so
`RunVerify` can find the record; `RunVerify` then writes it again with the
health outcome.

### Flow integration: `Resume`

**Wiring (`cmd/plax/main.go`).** `runResume` currently opens the
BaseManager *after* `instance.Resume` returns, tolerantly, for the drift
report. It now opens BM *before* calling Resume — still tolerantly — and
sets `deps.BM`; the post-resume drift report reuses the same handle. When
the open fails, `deps.BM` stays nil: DB checks are skipped with a stderr
note and resume is otherwise unaffected. This preserves today's behavior
of resume succeeding while Postgres is down (the drift report already
degrades to a note in that case).

**Env checks** run after the existing `.env` parse, before processes
spawn. On failure:

```
rec.Health = registry.HealthUnhealthy
now := time.Now(); rec.VerifiedAt = &now
deps.Registry.UpdateInstance(name, rec)
deps.Registry.Save()            // best-effort; a save error joins the returned error
printVerificationErrors(results)
return &verify.VerificationError{Results: results, Layer: 1}
```

The instance was never unsuspended (the deferred rollback stops the
just-started containers), but the record is flagged unhealthy — the broken
`.env` is a real, diagnosed defect that `ls` should surface. Resume's
rollback only stops workloads started by this resume; it never touches the
registry, so the health update persists.

**Runtime verification** runs after the settle check, same shape as Up's
Step 8.5: call `RunVerify`; on `*VerificationError` set `success = true`
and return the error — workloads stay running, health is `unhealthy`,
exit 1. On any other error, return it and let the rollback stop the
started workloads.

### Flow integration: `Rederive`

After `os.Rename(tmpPath, instEnvPath)` succeeds for an instance:

```
results := verify.CheckEnv(templatePath, userEnvPath, instEnvPath,
                           deps.Blueprint.Env.Holes, scrub)
rec := deps.Registry.Instances[name]
rec.Health = allPassed(results) ? registry.HealthHealthy : registry.HealthUnhealthy
now := time.Now(); rec.VerifiedAt = &now
deps.Registry.Instances[name] = rec
healthDirty = true
if !allPassed(results) {
    fmt.Fprintf(os.Stderr, "warning: %s: .env verification failed "+
                "— instance marked unhealthy\n", name)
}
```

After the loop: `if healthDirty { deps.Registry.Save() }` (a save error is
returned, wrapped). Neither `Rederive` nor `runRederive` saves the
registry today, so without this the health updates would be silently
discarded.

The outcome is recorded in both directions — a rederive that repairs a
previously broken `.env` flips the instance back to `healthy`. Instances
whose derived file is byte-identical (no rewrite) are not re-checked and
keep their existing health. A `DeriveMerged` failure means no new file
exists to verify; health is left untouched in that case (the existing
warning path is unchanged).

### Base seed non-emptiness — `derive/postgres/basemanager.go`

New unexported helper:

```go
// assertSeedNonEmpty fails when the freshly seeded database has zero rows
// across all user tables — the seed command may have failed silently.
func (bm *BaseManager) assertSeedNonEmpty(ctx context.Context, pool *pgxpool.Pool) error
```

1. Run `SELECT COALESCE(SUM(n_live_tup), 0) FROM pg_stat_user_tables`.
2. ⚠ `pg_stat_user_tables` is maintained asynchronously: the stats
   collector applies the seed's tabstat messages with a small lag, so an
   immediate read can racily report 0 right after a fast seed. When the
   sum is 0, retry up to 3 times, 500ms apart, before failing.
   (`pg_stat_force_next_flush()` would close the race but requires
   Postgres 15; the retry is version-agnostic.)
3. Still 0 after retries → return
   `"seeded base has zero rows across all user tables — the seed command may have failed silently"`.
4. ⚠ `pg_stat_user_tables` excludes system catalogs (tables in `pg_catalog`
   and `information_schema`), so empty migrations followed by a no-op seed
   correctly fails.
5. ⚠ A repo with only extension-defined data would always read 0 rows.
   This is an acceptable false positive — the error message is clear and
   the repo can insert at least one user-table row.

Call sites — both compose with the existing cleanup patterns:

- `SeedBase`: after `runSeed`, before `ReadProvenance`. On error:
  `basePool.Close()`, then `return errors.Join(err, bm.relock(ctx))` — the
  base must be re-locked exactly like every other failure path in
  `SeedBase`.
- `RefreshBase`: after `runSeed` on `base_next`. On error:
  `nextPool.Close()`, then
  `return errors.Join(err, bm.cleanupDB(ctx, bm.nextName))`. Refresh
  shares the check — a silent no-op seed must not stage an empty
  base_next and swap it into production.

---

## CLI specification

### New command: `plax verify <name>`

```
Usage: plax verify <name> [--root <path>] [--pg-url <dsn>] [--json]

Run verification checks against an existing instance and update its health
state in the registry. Exits 1 if any check fails.

Flags:
  --root, -r   Repo root directory (default: .)
  --pg-url     Postgres connection DSN (overrides blueprint env) — same as
               up/down/resume/status/base/doctor
  --json       Output results as JSON array
```

- The blueprint is required (it carries the env template, holes, scrub
  set, and services); a missing `plax.json` is an error.
- BM is opened tolerantly (same pattern as `runStatus`). When Postgres is
  unreachable, DB checks are skipped with a stderr note; exit code and
  health reflect the checks that ran.
- Suspended instance: runtime checks (TCP, process) are skipped with a
  stderr note; env and DB checks still run.
- No Docker dependency: the service check dials TCP directly and never
  inspects containers.

| Command | Change |
|---|---|
| `plax up <name>` | Env checks run after derivation, before DB clone — failure rolls back, exit 1. Runtime checks run after settle — failure keeps the instance up, records `unhealthy`, exit 1. |
| `plax resume <name>` | Env checks after `.env` parse — failure leaves the instance suspended, records `unhealthy`, exit 1. Runtime checks after settle — failure keeps workloads running, records `unhealthy`, exit 1. Postgres unreachable → DB checks skipped with a note. |
| `plax rederive` | Env checks on each rederived instance; health recorded either way and persisted to the registry. |
| `plax verify <name>` | **New.** All checks standalone (runtime checks skipped on suspended instances). Updates health. Exit 1 on failure. |
| `plax ls` | `HEALTH` column added to table output. Values: `healthy`, `unhealthy`, or `—` (never verified). |
| `plax status <name>` | New `health` dimension, six total: `ok` for healthy, `drift` for unhealthy (detail includes `VerifiedAt`), `unknown` for never-verified instances. `StatusCmd`'s help text, `printReportTable`, and `printReportStderr` all gain the sixth dimension; `AGENTS.md`'s status-package description is updated to match. |

Commands with no change: `init`, `suspend`, `down`, `attach`, `exec`,
`send`, `recv`. (`base seed` and `base refresh` gain the non-emptiness
check; other `base *` commands are unchanged.)

### Example output — `plax up` failing env checks (rolled back)

```
> plax up i1
creating branch and worktree...
creating network plax-i1-net...
allocating ports...
deriving .env...

verification failed (layer 1):
  env-completeness: key SENDGRID_API_KEY is missing from derived .env
  env-scrubbed-leaks: scrubbed key STRIPE_SECRET's real value appears in derived .env
plax: error: verification failed: env-completeness (SENDGRID_API_KEY), env-scrubbed-leaks (STRIPE_SECRET)

exit code: 1 — instance rolled back; nothing is registered or left running
```

### Example output — `plax up` failing runtime checks (stays up)

```
> plax up i1
creating branch and worktree...
...
instance i1 up
  worktree:  .plax/worktrees/i1
  branch:    plax/i1
  database:  plax_i1
  ports:     PORT=3301 REDIS_PORT=26380
  logs:      .plax/logs/i1/

verification failed (layer 1):
  tcp-reachability: service redis on port 26380 is not reachable

instance i1 is up but unhealthy — investigate, then 'plax verify i1' to re-check or 'plax down i1' to tear down
plax: error: verification failed: tcp-reachability (127.0.0.1:26380)

exit code: 1
```

### Example output — `plax verify i1`

```
i1:
  [pass] env-completeness
  [pass] env-unresolved-holes
  [pass] env-scrubbed-leaks
  [fail] tcp-reachability: service redis on port 26380 is not reachable
  [pass] process-liveness
  [pass] db-existence
  [pass] db-provenance
```

### Example — `plax verify i3` on a suspended instance

```
> plax verify i3
note: i3 is suspended — runtime checks (tcp-reachability, process-liveness) skipped
i3:
  [pass] env-completeness
  [pass] env-unresolved-holes
  [pass] env-scrubbed-leaks
  [pass] db-existence
  [pass] db-provenance
```

### Example — `plax ls` with health

```
NAME     STATE      BRANCH               MAIL  PORTS                   HEALTH     CREATED
i1       running    plax/i1              0     6382 6383               unhealthy  5m ago
i2       running    plax/i2              0     6384 6385               healthy    3m ago
i3       suspended  plax/i3              0     6386 6387               —          2d ago
```

### Example — `plax base seed` with empty seed

```
> plax base seed
seeding base... error: seeded base has zero rows across all user tables
       — the seed command may have failed silently
exit code: 1
```

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| Template file missing (env checks) | `os.Stat` in `CheckEnv` | Skip env checks — nothing to verify. Runtime checks still run. |
| User `.env` missing (env checks) | `os.Stat` in `CheckEnv` | Treat user-env-keys set as empty. Completeness check compares derived against template+hole keys only. |
| Derived `.env` missing (env checks) | `os.Stat` in `CheckEnv` | Return `{Check: "env-completeness", Passed: false, Detail: "derived .env not found at PATH"}` |
| Scrubbed key has empty user value | Value is `""` | Pass — there is nothing to leak. |
| Dedicated service has no allocated port | Key not in ports map | Pass — the service was skipped for a reason plax already knows. |
| TCP dial timeout (2s) | `net.DialTimeout` returns `i/o timeout` | Return failure `{Detail: "service SERVICE on port PORT timed out after 2s"}` |
| Container not running but TCP accepts | SYN-ACK from another process | Unlikely with 127.0.0.1 binding, but the check is TCP-reachability, not container liveness. Acceptable blind spot. |
| Process PGID reused by OS during verify | `isAlive` returns true for wrong process | Same blind spot as the existing settle check. PID-reuse protection comes from PIDStarts, which the verify check does not extend — it only checks current liveness. |
| Instance DB dropped between clone and verify | `InstanceDBExists` returns false | `{Check: "db-existence", Passed: false, ...}` |
| Provenance table missing | `ReadProvenance` returns nil | `{Check: "db-provenance", Passed: false, ...}` |
| Env-check failure during `up` | Step 4.5 `CheckEnv` | Print failing checks → rollback all side effects (worktree, mailbox, network, ports) → exit 1. No registry record is written. |
| Runtime-check failure during `up`/`resume` | `RunVerify` returns `*VerificationError` | Health persisted `unhealthy`; workloads keep running; exit 1. No rollback — instance stays up for debugging. |
| Env-check failure during `resume` | `CheckEnv` after `.env` parse | Health persisted `unhealthy`; rollback stops just-started containers, instance stays suspended; exit 1. |
| Postgres unreachable (`resume`, `verify`) | BM open failed → `deps.BM == nil` | DB checks skipped with stderr note; remaining checks run; health reflects them. Resume/verify do not fail. |
| Instance suspended (`verify`) | `rec.State == StateSuspended` in `RunVerify` | Runtime checks skipped with stderr note; env + DB checks run; health updated from executed checks. |
| Registry save fails inside `RunVerify` during `up` | `Save()` error (non-VerificationError) | `up` returns the error → rollback runs; the registry-removal cleanup deletes the preliminary record, so no orphan record points at torn-down resources. |
| Registry save fails after verify (standalone) | `Save()` error | Return verification error (checks still valid) wrapped with save error. |
| Running `verify` on old instance (no Health field) | `rec.Health == ""` | Checks run normally. Empty Health is treated as never-verified by `ls` and `status`. |
| Seed command produces zero rows legitimately | Sum still 0 after 3 retries | False positive — error returned. Repo must insert at least one row. Acceptable trade-off. |
| Stats collector lag after seed | `n_live_tup` sum racily reads 0 | Mitigated by the retry loop (3 × 500ms); a genuinely empty seed still fails. |

---

## Tests

### Unit tests — `pkg/verify/verify_test.go`

- `TestVerify_EnvCompleteness_AllKeysPresent` —
  Template + user `.env` with non-overlapping keys. Verify all keys are
  found in derived output.
- `TestVerify_EnvCompleteness_MissingKey` —
  Key in user `.env` not in derived. Verify single failure result.
- `TestVerify_EnvCompleteness_HoleKeyExpected` —
  Hole key absent from the template is still expected (derivation appends
  it); verify a missing one fails.
- `TestVerify_EnvCompleteness_ScrubbedKeyExcluded` —
  Scrubbed key in user `.env` — verify it is NOT expected in completeness
  check.
- `TestVerify_EnvNoUnresolved_Clean` —
  Derived `.env` with no `{{`. All pass.
- `TestVerify_EnvNoUnresolved_HoleLeftBehind` —
  Derived `.env` contains `{{PORT}}`. Verify failure.
- `TestVerify_EnvNoUnresolved_CommentExcluded` —
  `# {{this_is_fine}}` in comment. Verify pass (comment ignored).
- `TestVerify_EnvNoScrubbedLeaks_NoLeak` —
  Scrubbed value not in derived output. Pass.
- `TestVerify_EnvNoScrubbedLeaks_LeakDetected` —
  Scrubbed value substring match in derived output. Fail.
- `TestVerify_EnvNoScrubbedLeaks_PlaceholderValue_Ignored` —
  User value equals template placeholder. Pass.
- `TestVerify_Services_TCPReachable` —
  Real loopback listener on the allocated port, verify pass.
- `TestVerify_Services_TCPUnreachable` —
  No listener on port, verify failure with expected detail.
- `TestVerify_Services_NoDedicatedServices` —
  No dedicated services in blueprint. Empty results, pass.
- `TestVerify_Processes_AllAlive` —
  Injected `isAlive` returns true for all. All pass.
- `TestVerify_Processes_SomeDead` —
  Injected `isAlive` returns false for one process. One failure result.
- `TestVerify_Processes_NoProcesses` —
  Empty PID map. Empty results.
- `TestVerify_Databases_AllExist_WithProvenance` —
  Fake BM: DB exists + provenance present. All pass.
- `TestVerify_Databases_MissingDB` —
  Fake BM: one DB absent. One db-existence failure.
- `TestVerify_Databases_MissingProvenance` —
  Fake BM: DB exists but no provenance. One db-provenance failure.
- `TestVerify_RunVerify_AggregatesResults` —
  Full RunVerify with mixed pass/fail. Verify VerificationError returned
  with correct Layer, and health persisted to the registry.
- `TestVerify_RunVerify_SkipsRuntimeChecks_WhenSuspended` —
  Suspended record with dead ports/PIDs: no tcp/process results, health
  computed from env+DB only.
- `TestVerify_RunVerify_SkipsDBChecks_WhenBMNil` —
  Nil BM: no db results; health computed from the rest.

### Unit tests — `pkg/derive/postgres/basemanager_test.go`

Require `PLAX_TEST_POSTGRES_URL`; self-skip without it.

- `TestBaseManager_SeedBase_NonEmptyData_Passes` —
  Seed inserts rows. SeedBase succeeds.
- `TestBaseManager_SeedBase_EmptyData_Fails` —
  Seed produces 0 user-table rows. Verify error message and that the base
  is re-locked afterwards.
- `TestBaseManager_RefreshBase_EmptySeed_Fails` —
  No-op seed during refresh. Verify the error and that base_next is
  cleaned up.

### Unit tests — `pkg/registry/registry_test.go`

- `TestRegistry_HealthRoundTrip` —
  Write instance with Health and non-nil VerifiedAt, read back, verify
  fields survive save/load.
- `TestRegistry_OldRecordNoHealth_DefaultEmpty` —
  Load registry without health fields (old JSON). Verify Health is empty
  string and VerifiedAt is nil.
- `TestRegistry_NeverVerified_OmitsVerifiedAt` —
  Marshal a record with empty Health/nil VerifiedAt; verify the JSON
  contains no `health` or `verified_at` keys.

### Unit tests — `pkg/instance` (existing fakes gain `InstanceProvenance` and `InstanceDBExists`)

- `TestUp_EnvCheckFailure_RollsBack` —
  Fake env state that fails completeness. Verify: returned error is a
  `*verify.VerificationError`, no registry record exists, worktree
  removed, ports released.
- `TestUp_RuntimeCheckFailure_StaysUpUnhealthy` —
  Fake a dead service/process at verify time. Verify: returned error is a
  `*verify.VerificationError`, registry record exists with
  `Health == "unhealthy"`, no rollback ran (worktree, containers, DBs
  intact).
- `TestRederive_HealthOutcome_Persisted` —
  Rederive an instance whose derived `.env` fails checks; reload the
  registry from disk and verify `unhealthy` survived. Repeat with a
  passing derivation flipping a previously unhealthy record back to
  `healthy`.

### Integration tests — `cmd/plax/e2e_test.go`

- `TestEndToEnd_Verification_EnvCompleteness` —
  `plax up` with a blueprint where a hole key gets dropped from
  derivation. Verify exit 1, the failing-check output, and that the
  instance was rolled back: absent from `plax ls`, worktree removed.
- `TestEndToEnd_Verify_DetectsStoppedService` —
  `plax up` a healthy instance, `docker stop` its redis container, then
  `plax verify i1`. Verify exit 1, tcp-reachability failure, and
  `unhealthy` in `plax ls`.
- `TestEndToEnd_Verification_BaseSeedEmpty` —
  `plax base seed` with no-op seed command. Verify exit 1 and error
  message.

---

## Acceptance criteria

- [x] `plax up` runs env checks after `.env` derivation, before any DB
      clones or container starts
- [x] `plax up` with an env-check failure exits 1, prints the failing
      checks (naming the offending keys), and rolls back — no registry
      record, worktree, database, container, or port allocation remains
- [x] `plax up` runs runtime checks (TCP, process, DB) after settle,
      before reporting success
- [x] `plax up` with a runtime-check failure exits 1, prints the failing
      checks, records `unhealthy`, and does NOT roll back — the instance
      stays up for debugging
- [x] A derived `.env` missing a key from the
      (template ∪ user-env ∪ holes) − scrub union produces an
      `env-completeness` failure
- [x] An unresolved `{{VAR}}` in the derived `.env` (outside comments)
      produces an `env-unresolved-holes` failure
- [x] A scrubbed key's real value present in the derived `.env` produces
      an `env-scrubbed-leaks` failure
- [x] An instance that fails verification is marked `unhealthy` in the
      registry and `plax ls` shows it
- [x] A failure after `up`'s preliminary registry write (e.g. verify's
      save fails) rolls back AND removes the registry record
- [x] `plax resume` re-verifies after restarting workloads; a
      verification failure exits 1 with workloads left running
- [x] `plax resume` with Postgres unreachable skips DB checks with a
      stderr note and otherwise succeeds
- [x] `plax rederive` records the env-check outcome for each rederived
      instance (both directions) and persists it to the registry on disk
- [x] `plax verify <name>` runs all checks and updates registry health
      (exit 1 on failure); `--pg-url` overrides the DSN
- [x] `plax verify` on a suspended instance skips runtime checks with a
      note and does not mark the instance unhealthy for being suspended
- [x] `plax verify` with Postgres unreachable skips DB checks with a note
- [x] `plax ls` shows `HEALTH` column: `healthy`, `unhealthy`, or `—`
      (old instances)
- [x] `plax status <name>` includes a sixth `health` dimension: `ok` /
      `drift` / `unknown`; the `StatusCmd` help text and `AGENTS.md`
      reflect six dimensions
- [x] `plax base seed` fails with a clear error when the seed produces
      zero user-table rows (after retries), and the base is left locked
- [x] `plax base refresh` fails the same way and cleans up `base_next`
- [x] Pre-existing instances without health data work correctly (empty
      health = pre-verification, no `verified_at` key written)
- [x] `go vet ./...` passes
- [x] `go test -race -count=1 ./...` passes (with and without
      `PLAX_TEST_POSTGRES_URL`)
- [x] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| No new external dependencies. | | | |

Standard library additions:

- `net` — `net.DialTimeout` for TCP reachability checks (`pkg/verify`)
- `time` — retry backoff in `assertSeedNonEmpty`; `VerifiedAt` timestamps
- `sort` — already imported where needed
- Required for `seed base` non-emptiness: pgx pool query — already
  available via the existing pool
