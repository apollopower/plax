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
  verify.go             # CheckResult, VerifyEnv, VerifyServices, VerifyProcesses, VerifyDatabases, RunVerify
  verify_test.go        # Unit tests for each check function
pkg/registry/
  registry.go           # InstanceRecord gains Health, VerifiedAt; new HealthHealthy/HealthUnhealthy constants
pkg/derive/postgres/
  basemanager.go        # SeedBase: after seed, assert base has non-empty user-table rows
pkg/instance/
  up.go                 # Weave env checks after derivation; weave runtime checks after settle
  resume.go             # Weave env checks after reading .env; runtime checks after settle
  rederive.go           # Re-verify env derivation after writing each instance's .env
  instance.go           # BaseManager interface gains InstanceProvenance, InstanceDBExists
pkg/status/
  status.go             # New "Health" dimension in Report
cmd/plax/
  main.go               # New VerifyCmd, plax verify <name> subcommand
docs/plans/
  index.md              # Add plan 15 to phase table
```

---

## Type specifications

### `pkg/registry/registry.go` — `InstanceRecord`

```go
type InstanceRecord struct {
    // ... existing fields unchanged ...

    Health     Health    `json:"health,omitempty"`
    VerifiedAt time.Time `json:"verified_at,omitempty"`
}
```

```go
type Health string

const (
    HealthHealthy   Health = "healthy"
    HealthUnhealthy Health = "unhealthy"
)
```

`Health` — the result of the last verification (`"healthy"` or `"unhealthy"`).
Default (empty string) means the instance predates the verification feature
and has never been checked. `VerifiedAt` is a timestamp of the last
verification run.

### `pkg/verify/verify.go` — `CheckResult`

```go
type CheckResult struct {
    Check    string `json:"check"`            // e.g. "env-completeness", "tcp-reachability"
    Layer    int    `json:"layer"`            // 1 (unconditional) or 2 (repo hook, future)
    Passed   bool   `json:"passed"`
    Detail   string `json:"detail"`           // human-readable message
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
    // Builds a multi-line error from results.
}
```

### `pkg/verify/verify.go` — `RunVerify`

```go
func RunVerify(ctx context.Context, deps *Deps, name string) ([]CheckResult, error)
```

Returns results and either nil (all passed) or `*VerificationError` (at least
one check failed).

### `pkg/verify/verify.go` — `Deps`

```go
type Deps struct {
    Blueprint *blueprint.Blueprint
    Registry  *registry.Registry
    BM        BMInterface
    RepoRoot  string
}

type BMInterface interface {
    InstanceDBExists(ctx context.Context, dbName string) (bool, error)
    InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error)
}
```

### `pkg/verify/verify.go` — Verifier funcs

Every check is a standalone function (or a signature-compatible func type)
so unit tests can substitute fakes per check:

```go
type EnvVerifier func(templatePath, userEnvPath, derivedEnvPath string, holes map[string]string, scrub map[string]bool) []CheckResult
type ServiceVerifier func(ctx context.Context, services map[string]blueprint.ServiceDef, allocated map[string]int) []CheckResult
type ProcessVerifier func(pids map[string]int) []CheckResult
type DBVerifier func(ctx context.Context, dbNames map[string]string, bm BMInterface) []CheckResult
```

---

## Algorithms

### Env derivation checks — `VerifyEnv`

Called immediately after `.env` derivation in `Up`, `Resume`, and `Rederive`.
All checks read files only; no network access.

**1. Completeness check** — `preCheckEnvCompleteness`:

- Parse the template file. Parse the user's `.env` file.
- Build a set of expected keys: (template-live-keys ∪ user-env-keys) − scrub-set.
- Parse the derived `.env`. For each expected key, verify it appears in the
  derived output.
- ⚠ A key in both `scrub` and `holes` is excluded from the completeness
  check. Hole keys are always present in the template or appended; scrubbing
  does not apply to holes anyway, but the union/intersection must be correct.
- Return one result per missing key:
  ```
  {Check: "env-completeness", Passed: false,
   Detail: "key NAME is missing from derived .env",
   Artifact: "NAME"}
  ```

**2. Unresolved hole check** — `preCheckEnvNoUnresolved`:

- Read the entire derived `.env` text. Search for `{{` substrings.
- ⚠ A `{{` in a comment line (`#`) is not an unresolved hole — it's a literal
  curly brace in a comment. Skip comment lines.
- Return one result per occurrence:
  ```
  {Check: "env-unresolved-holes", Passed: false,
   Detail: "unresolved template hole {{VAR}} survives in derived .env",
   Artifact: "derived .env"}
  ```

**3. Scrubbed value leak check** — `preCheckEnvNoScrubbedLeaks`:

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

### TCP reachability check — `VerifyServices`

Called after the settle delay in `Up` and `Resume`. Runs concurrently:

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
     should retry once after 1 second.
   - Return one result per failure:
     ```
     {Check: "tcp-reachability", Passed: false,
      Detail: "service SERVICE on port PORT is not reachable",
      Artifact: "127.0.0.1:PORT"}
     ```

### Process liveness check — `VerifyProcesses`

The existing settle check already verifies processes are alive at
settle+300ms. The verification layer formalizes this as a check rather
than a fatal error:

1. For each registered PID (from the registry or freshly spawned):
   - Call `process.IsAlive(pgid)`.
   - Return one result per failure:
     ```
     {Check: "process-liveness", Passed: false,
      Detail: "process PROC (PGID=pgid) is not alive",
      Artifact: "PROC"}
     ```

### Database checks — `VerifyDatabases`

Called after the settle delay:

**1. Existence check**: For each database name in the instance record:

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
    rec = deps.Registry.GetInstance(name)
    if !found: return nil, "instance %q not found"

    results = []

    // Env checks (static, no context needed)
    templatePath = join(deps.RepoRoot, deps.Blueprint.Env.Template)
    userEnvPath  = join(deps.RepoRoot, ".env")
    derivedPath  = join(rec.WorktreePath, ".env")
    scrub        = buildScrubSet(deps.Blueprint)
    results += VerifyEnv(templatePath, userEnvPath, derivedPath,
                         deps.Blueprint.Env.Holes, scrub)

    // Service TCP check
    results += VerifyServices(ctx, deps.Blueprint.Services, rec.Ports)

    // Process check
    results += VerifyProcesses(rec.PIDs)

    // DB checks
    dbNames = registry.DBNamesFromRecord(rec)
    results += VerifyDatabases(ctx, dbNames, deps.BM)

    // Update registry health
    allPassed = true
    for each r in results:
        if !r.Passed: allPassed = false

    rec.VerifiedAt = now
    rec.Health = allPassed ? HealthHealthy : HealthUnhealthy
    deps.Registry.UpdateInstance(name, rec)
    if err := deps.Registry.Save(); err != nil:
        return results, fmt.Errorf("saving verification results: %w", err)

    if !allPassed:
        return results, &VerificationError{Results: results, Layer: 1}
    return results, nil
```

### Flow integration: `Up`

Insert between Steps 4 and 5 (after `.env` derivation, before DB clone):

```
// Step 4.5: Run static env checks (fail fast, no boot cost wasted)
if deps.Blueprint.Env.Template != "" {
    results = verify.CheckEnv(templatePath, overridesPath, envPath,
                              deps.Blueprint.Env.Holes, scrub)
    if anyFailed(results):
        return &VerificationError{Results: results, Layer: 1}
}
```

Insert after Step 7 (settle check), before registry write:

```
// Step 7.5: Verify runtime state
// Write preliminary registry record so RunVerify can read/write it.
rec = ...registry.InstanceRecord... // build as today
rec.Health = registry.HealthHealthy
deps.Registry.AddInstance(name, rec)
deps.Registry.Save()

vDeps = &verify.Deps{Blueprint: deps.Blueprint, Registry: deps.Registry,
                      BM: deps.BM, RepoRoot: deps.RepoRoot}
results, err = verify.RunVerify(ctx, vDeps, name)
if err != nil:
    if verr, ok := err.(*verify.VerificationError):
        printVerificationErrors(verr)
        // Instance already written with unhealthy status by RunVerify
    else:
        return err // real error (e.g., save failure)
```

⚠ The registry is written twice in `Up`: once for `RunVerify` to find the
instance, and then potentially updated by `RunVerify` itself. The
double-write approach reuses `RunVerify` as-is, which is also called from
standalone `plax verify`.

### Flow integration: `Resume`

Same pattern — env checks after `.env` read, runtime checks after
settle+300ms. The `RunVerify` call updates health.

⚠ `InstanceProvenance` and `InstanceDBExists` must be added to the
`BaseManager` interface in `instance.go` so `RunVerify` can call them.

### Flow integration: `Rederive`

After writing each instance's `.env`, run only the static env checks:

```
// After os.Rename(tmpPath, instEnvPath) ...:
results = verify.CheckEnv(templatePath, userEnvPath, instEnvPath,
                          deps.Blueprint.Env.Holes, scrub)
if !allPassed(results):
    rec = deps.Registry.Instances[name]
    rec.Health = registry.HealthUnhealthy
    rec.VerifiedAt = now
    deps.Registry.Instances[name] = rec
    fmt.Fprintf(os.Stderr, "warning: %s: .env verification failed "+
                "— instance may be unhealthy\n", name)
```

### Base seed non-emptiness — `derive/postgres/basemanager.go`

In `SeedBase`, after the seed command succeeds and before bumping provenance:

1. Connect to the base database.
2. Run `SELECT COALESCE(SUM(n_live_tup), 0) FROM pg_stat_user_tables`.
3. If the result is 0, return an error:
   `"seeded base has zero rows across all user tables — the seed command may have failed silently"`.
4. ⚠ `pg_stat_user_tables` excludes system catalogs (tables in `pg_catalog`
   and `information_schema`), so empty migrations followed by a no-op seed
   correctly fails.
5. ⚠ A repo with only system-defined data (e.g., an extension-only setup)
   would always have 0 rows. This is an acceptable false positive — the
   error message is clear and the repo can override by ensuring the seed
   creates at least one user-table row.

---

## CLI specification

### New command: `plax verify <name>`

```
Usage: plax verify <name> [--root <path>] [--json]

Run verification checks against an existing instance and update its health
state in the registry. Exits 1 if any check fails.

Flags:
  --root, -r   Repo root directory (default: .)
  --json       Output results as JSON array
```

| Command | Change |
|---|---|
| `plax up <name>` | Env checks run before DB clone; runtime checks run after settle. Health recorded in registry. Exit 1 on failure. |
| `plax resume <name>` | Same verification as `up` after workloads restart. Health updated. |
| `plax rederive` | Env checks run on each rederived instance. Unhealthy status recorded on failure. |
| `plax verify <name>` | **New.** Run all verification checks standalone. Updates health. Exit 1 on failure. |
| `plax ls` | `HEALTH` column added to table output. Values: `healthy`, `unhealthy`, or `—` (for old instances). |
| `plax status <name>` | New "Health" dimension in report. `ok` for healthy, `drift` for unhealthy, `unknown` for pre-verification instances. |

Commands with no change: `init`, `suspend`, `down`, `attach`, `exec`, `send`,
`recv`, `base *`.

### Example output — `plax up` with verification failure

```
instance i1 up
  worktree:  .plax/worktrees/i1
  branch:    plax/i1
  database:  plax_i1

verification failed (layer 1):
  env-completeness: key SENDGRID_API_KEY is missing from derived .env
  env-scrubbed-leaks: scrubbed key STRIPE_SECRET's real value appears in derived .env

instance i1 is up but unhealthy — run 'plax doctor' and 'plax verify i1' to diagnose

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
| Template file missing (env checks) | `os.Stat` in `VerifyEnv` | Skip env checks — nothing to verify. Runtime checks still run. |
| User `.env` missing (env checks) | `os.Stat` in `VerifyEnv` | Treat user-env-keys set as empty. Completeness check compares derived against template keys only. |
| Derived `.env` missing (env checks) | `os.Stat` in `VerifyEnv` | Return `{Check: "env-completeness", Passed: false, Detail: "derived .env not found at PATH"}` |
| Scrubbed key has empty user value | Value is `""` | Pass — there is nothing to leak. |
| Dedicated service has no allocated port | Key not in ports map | Pass — the service was skipped for a reason plax already knows. |
| TCP dial timeout (2s) | `net.DialTimeout` returns `i/o timeout` | Return failure `{Detail: "service SERVICE on port PORT timed out after 2s"}` |
| Container not running but TCP accepts | SYN-ACK from another process | Unlikely with 127.0.0.1 binding, but the check is TCP-reachability, not container liveness. Acceptable blind spot. |
| Process PGID reused by OS during verify | `process.IsAlive` returns true for wrong process | Same blind spot as the existing settle check. PID-reuse protection comes from PIDStarts, which the verify check does not extend — it only checks current liveness. |
| Instance DB dropped between clone and verify | `InstanceDBExists` returns false | `{Check: "db-existence", Passed: false, ...}` |
| Provenance table missing | `ReadProvenance` returns nil | `{Check: "db-provenance", Passed: false, ...}` |
| Postgres unreachable (verify) | `bm.InstanceDBExists` returns error | Return partial results with error. Health not updated. |
| Registry save fails after verify | `Save()` error | Return verification error (checks still valid) wrapped with save error. |
| Running `verify` on old instance (no Health field) | `rec.Health == ""` | Checks run normally. Empty Health is treated as pre-verification by `ls` and `status`. |
| Seed command produces zero rows legitimately | `pg_stat_user_tables` sum is 0 | False positive — error returned. Repo must insert at least one row. Acceptable trade-off. |

---

## Tests

### Unit tests — `pkg/verify/verify_test.go`

- `TestVerify_EnvCompleteness_AllKeysPresent` —
  Template + user `.env` with non-overlapping keys. Verify all keys are found
  in derived output.
- `TestVerify_EnvCompleteness_MissingKey` —
  Key in user `.env` not in derived. Verify single failure result.
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
  Fake listener on 127.0.0.1, verify pass.
- `TestVerify_Services_TCPUnreachable` —
  No listener on port, verify failure with expected detail.
- `TestVerify_Services_NoDedicatedServices` —
  No dedicated services in blueprint. Empty results, pass.
- `TestVerify_Processes_AllAlive` —
  Mock `IsAlive` returns all true. All pass.
- `TestVerify_Processes_SomeDead` —
  Mock one dead process. One failure result.
- `TestVerify_Processes_NoProcesses` —
  Empty PID map. Empty results.
- `TestVerify_Databases_AllExist_WithProvenance` —
  Mock DB exists + provenance present. All pass.
- `TestVerify_Databases_MissingDB` —
  Mock one DB absent. One db-existence failure.
- `TestVerify_Databases_MissingProvenance` —
  Mock DB exists but no provenance. One db-provenance failure.
- `TestVerify_RunVerify_AggregatesResults` —
  Full RunVerify with mixed pass/fail. Verify VerificationError returned
  with correct Layer.

### Unit tests — `pkg/derive/postgres/basemanager_test.go`

- `TestBaseManager_SeedBase_NonEmptyData_Passes` —
  Seed inserts rows. SeedBase succeeds.
- `TestBaseManager_SeedBase_EmptyData_Fails` —
  Seed produces 0 user-table rows. Verify error message.

### Unit tests — `pkg/registry/registry_test.go`

- `TestRegistry_HealthRoundTrip` —
  Write instance with Health and VerifiedAt, read back, verify fields
  survive save/load.
- `TestRegistry_OldRecordNoHealth_DefaultEmpty` —
  Load registry without health fields (old JSON). Verify Health is empty
  string.

### Integration tests — `cmd/plax/e2e_test.go`

- `TestEndToEnd_Verification_EnvCompleteness` —
  `plax up` with blueprint where a hole key gets dropped from derivation.
  Verify exit 1 and unhealthy in `ls`.
- `TestEndToEnd_Verification_BaseSeedEmpty` —
  `plax base seed` with no-op seed command. Verify exit 1 and error message.

---

## Acceptance criteria

- [ ] `plax up` runs env checks after `.env` derivation, before any DB
      clones or container starts
- [ ] `plax up` runs runtime checks (TCP, process, DB) after settle,
      before reporting success
- [ ] A derived `.env` missing a key from the template+user-env union
      produces an `env-completeness` failure
- [ ] An unresolved `{{VAR}}` in the derived `.env` (outside comments)
      produces an `env-unresolved-holes` failure
- [ ] A scrubbed key's real value present in the derived `.env` produces
      an `env-scrubbed-leaks` failure
- [ ] An instance that fails verification is marked `unhealthy` in the
      registry and `plax ls` shows it
- [ ] `plax up` with verification failure exits 1 and prints the failing
      checks
- [ ] `plax up` with verification failure does NOT roll back the instance —
      it stays up for debugging
- [ ] `plax resume` re-verifies after restarting workloads
- [ ] `plax rederive` re-verifies env derivation for each instance and
      marks unhealthy on failure
- [ ] `plax verify <name>` runs all checks and updates registry health
      (exit 1 on failure)
- [ ] `plax ls` shows `HEALTH` column: `healthy`, `unhealthy`, or `—`
      (old instances)
- [ ] `plax status <name>` includes a Health dimension: `ok` / `drift` /
      `unknown`
- [ ] `plax base seed` fails with a clear error when the seed produces zero
      user-table rows
- [ ] Pre-existing instances without health data work correctly (empty
      health = pre-verification)
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (with and without
      `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| No new external dependencies. | | | |

Standard library additions:

- `net` — `net.DialTimeout` for TCP reachability checks (`pkg/verify`)
- `sort` — already imported where needed
- Required for `seed base` non-emptiness: pgx pool query — already
  available via the existing pool
