# Phase 6 — Correctness Fixes

## Objective

Fix six user-facing correctness bugs discovered during the final review pass.
Each is small (1–10 lines) but breaks real workflows today. No new features,
no refactors — surgical fixes only.

---

## Package layout

```
cmd/plax/main.go               # Fix: init name, doctor --json exit code
pkg/blueprint/init.go          # Fix: bare compose port defaults
pkg/derive/postgres/
  basemanager.go               # Fix: DATABASE_URL env override, SQL injection
pkg/blueprint/validate.go      # Fix: isolation enum validation
pkg/blueprint/validate_test.go # New tests for enum validation
pkg/blueprint/init_test.go     # New test asserting Default on bare ports
pkg/derive/postgres/
  basemanager_test.go          # New tests for env override and SQL safety
```

---

## Bug fixes

### F1 — `plax init` names the blueprint `"."`

**File:** `cmd/plax/main.go`
**Problem:** `runInit` passes `cmd.Root` (defaults to `"."`) unresolved.
`filepath.Base(".")` returns `"."`, so the scaffold's `name` field is `"."`.
**Fix:** Call `filepath.Abs(cmd.Root)` before passing to `InitFromRepo`.

```go
// Before
bp, err := blueprint.InitFromRepo(cmd.Root)

// After
absRoot, err := filepath.Abs(cmd.Root)
if err != nil {
    return fmt.Errorf("resolving root: %w", err)
}
bp, err := blueprint.InitFromRepo(absRoot)
```

**Test:** `TestInit_DotRoot_ResolvesName` — call `InitFromRepo(".")` (after
`os.Chdir` to a temp dir) and assert `bp.Name` is the directory base name,
not `"."`.

---

### F2 — Bare `host:container` compose ports drop the host default

**File:** `pkg/blueprint/init.go` (compose port parsing, ~line 158)
**Problem:** For `"8080:80"`, the regex captures `m[1]="8080"` (host) and
`m[2]="80"` (container). The code sets `containerPort = m[2]` but never
assigns `defaultHostPort = m[1]`. The `Default` field feeds
`buildPortVarMap`, which maps `localhost:8080` references in `.env.example`
to `{{WEB_PORT}}`. Without it, the most common compose form produces
`FIXME_PORT_8080` holes.

**Fix:**

```go
containerPort = m[2]
defaultHostPort = m[1]  // add this line
```

**Test:** Extend `TestInit_ComposePortsBareNumber` to assert
`Ports["80"].Default == "8080"` alongside the existing `Var` assertion.

---

### F3 — `DATABASE_URL` environment override may not take effect

**File:** `pkg/derive/postgres/basemanager.go` (`runCommand`, ~line 519)
**Problem:** `cmd.Environ()` returns the parent environment. Appending
`DATABASE_URL=...` creates a duplicate when the parent already has one.
On Linux, `getenv` returns the *first* match — the parent's value, not the
per-instance DSN. Seed/migrate commands run against the wrong database.

**Fix:** Replace any existing `DATABASE_URL` entry instead of appending.

```go
env := cmd.Environ()
key := "DATABASE_URL="
val := key + bm.dsnForDB(dbName)
found := false
for i, e := range env {
    if strings.HasPrefix(e, key) {
        env[i] = val
        found = true
        break
    }
}
if !found {
    env = append(env, val)
}
cmd.Env = env
```

**Test:** `TestRunCommand_EnvOverride` — set `DATABASE_URL` in the test
process environment, invoke `runCommand`, and assert the child process sees
the per-database DSN, not the parent value.

---

### F4 — SQL injection in `terminateConnections`

**File:** `pkg/derive/postgres/basemanager.go` (~line 568)
**Problem:** `dbName` is interpolated into a SQL string literal via
`fmt.Sprintf`. Every other query in the file uses `$1` parameterized
queries. This one should too.

**Fix:**

```go
// Before
"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()",

// After
"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
```

Pass `dbName` as an argument to `Exec`/`Query` instead of embedding it.

**Test:** `TestTerminateConnections_SpecialChars` — create a database with
a name containing a single quote (e.g., `it's_a_test`), call
`terminateConnections`, verify no error. On the current code this would
inject a syntax error.

---

### F5 — `plax doctor --json` always exits 0

**File:** `cmd/plax/main.go` (~line 986)
**Problem:** The `--json` branch returns from `enc.Encode(report)` before
reaching the `os.Exit(1)` at the bottom of `runDoctor`. Scripting callers
cannot detect failure via exit code.

**Fix:** Check `report.Failed()` before encoding:

```go
if cmd.JSON {
    enc := json.NewEncoder(os.Stdout)
    enc.SetIndent("", "  ")
    if err := enc.Encode(report); err != nil {
        return err
    }
    if report.Failed() {
        os.Exit(1)
    }
    return nil
}
```

**Test:** Integration test invoking `plax doctor --json` against a fixture
with a known failing check; assert exit code 1 and valid JSON on stdout.

---

### F6 — Isolation enum never validated

**File:** `pkg/blueprint/validate.go` (`ValidateStructural`)
**Problem:** `ValidateStructural` checks service/process shape but never
validates that `Isolation` is one of the five defined values
(`logical`, `dedicated`, `shared`, `native`). A typo like
`"isolation": "dedicatd"` passes validation, then `up.go` silently skips
the container. The instance comes up "running" with the service absent.

**Fix:** Add a validation check for each service and process:

```go
validIsolations := map[ServiceIsolation]bool{
    IsolationLogical:   true,
    IsolationDedicated: true,
    IsolationShared:    true,
}
for name, svc := range bp.Services {
    if !validIsolations[svc.Isolation] {
        errs = append(errs, fmt.Errorf(
            "service %q: unknown isolation %q (want logical, dedicated, or shared)",
            name, svc.Isolation))
    }
}
validProcessIsolations := map[ProcessIsolation]bool{
    IsolationNative: true,
}
for _, proc := range bp.Processes {
    if !validProcessIsolations[proc.Isolation] {
        errs = append(errs, fmt.Errorf(
            "process %q: unknown isolation %q (want native)",
            proc.Name, proc.Isolation))
    }
}
```

**Test:** `TestValidate_UnknownServiceIsolation` — blueprint with
`"isolation": "dedicatd"` → one error naming the service and the invalid
value. `TestValidate_UnknownProcessIsolation` — same for a process.

---

## CLI specification

No new commands or flags. All changes are internal to existing commands.

---

## Error handling

| Failure mode | Expected behavior |
|---|---|
| `plax init` run in repo root | Blueprint `name` is the directory base name, not `"."` |
| Compose file with `"8080:80"` port | `Default` is `"8080"`, hole resolves to `{{WEB_PORT}}` |
| Parent env has `DATABASE_URL` | Child process sees the per-instance DSN |
| Database name with special chars | `terminateConnections` executes safely via parameterized query |
| `plax doctor --json` with failures | Exit code 1, valid JSON on stdout |
| Blueprint with typo'd isolation | Validation error naming the service and the invalid value |

---

## Tests

| Test | What it verifies |
|---|---|
| `TestInit_DotRoot_ResolvesName` | F1: `filepath.Abs` before `Base` |
| `TestInit_ComposePortsBareNumber` (extend) | F2: `Default` field populated |
| `TestRunCommand_EnvOverride` | F3: child sees per-instance DSN |
| `TestTerminateConnections_SpecialChars` | F4: no SQL injection |
| `TestDoctor_JSON_ExitCode` | F5: exit 1 on failures with `--json` |
| `TestValidate_UnknownServiceIsolation` | F6: typo'd service isolation rejected |
| `TestValidate_UnknownProcessIsolation` | F6: typo'd process isolation rejected |

---

## Acceptance criteria

- [x] `plax init` in a repo root produces a blueprint whose `name` matches the directory base name
- [x] `plax init` with a compose file containing `"8080:80"` produces a `PortDef` with `Default: "8080"`
- [x] Seed command runs against the correct database when parent has `DATABASE_URL` set
- [x] `terminateConnections` with a quote-containing database name succeeds
- [x] `plax doctor --json` exits 1 when any check fails
- [x] `plax init` + hand-edited `plax.json` with `"isolation": "dedicatd"` → `plax doctor` reports validation error
- [x] All existing tests continue to pass
- [x] `go vet ./...` and `golangci-lint run` report no issues

---

## Dependencies

No new dependencies.
