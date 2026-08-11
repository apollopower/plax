# Phase 9 — Polish

## Objective

Clean up remaining code quality issues: dead code, missing doc comments,
stderr layering violations, test hygiene, and minor Go idiom violations.
No behavior changes — this phase makes the codebase consistent with its
own conventions.

---

## Package layout

```
pkg/blueprint/
  blueprint.go                 # Add: doc comments on all exported types
  validate.go                  # Fix: error messages naming correct party
  validate_test.go             # Fix: shared fixture → per-test constructor
  init.go                      # Fix: stderr warnings → return warnings
pkg/registry/
  registry.go                  # Add: UpdateInstance method, sentinel errors
pkg/portpool/
  pool.go                      # Remove: Reserve (dead code) or justify
  probe.go                     # Add: portability comment, scanner error check
pkg/toolchain/
  toolchain.go                 # Add: doc comments on all exported API
  toolchain_test.go            # Fix: test names to TestToolchain_*
pkg/process/
  supervisor.go                # Fix: contradictory comment, errors.Is style
pkg/mailbox/
  mailbox.go                   # Fix: stderr prints → return skip info
                               # Add: doc comments on exported API
pkg/instance/
  up.go                        # Fix: step numbering, sortedKeys → sort.Strings
                               # Fix: splitEnv → strings.Cut
  suspend.go                   # Add: doc comment on Suspend
  resume.go                    # Add: doc comment on Resume
  rederive.go                  # Fix: remove unused ctx parameter
pkg/status/
  status.go                    # Remove: unused Dimension.Name field
                               # Fix: dataDrift unused rec parameter
pkg/doctor/
  doctor_test.go               # Fix: containsStr → strings.Contains
cmd/plax/
  main.go                      # Fix: runSend stdout/stderr separation
                               # Fix: default case in command switch
                               # Fix: runStatus/runDoctor silent error swallowing
pkg/testutil/
  testutil.go                  # No changes needed
```

---

## Fixes

### P1 — Doc comments on exported blueprint types

**File:** `pkg/blueprint/blueprint.go`
**Problem:** The blueprint is the contract between repo and tool. Every
exported type and field should have a doc comment explaining its semantics.

```go
// Blueprint is the top-level config, stored as plax.json in the repo root.
type Blueprint struct { ... }

// ServiceDef describes one service the repo needs (database, cache, etc).
type ServiceDef struct { ... }

// PortDef maps a container port to an env var for hole resolution.
// Default is the host port from compose, used to pre-fill .env.example holes.
type PortDef struct { ... }

// ProcessDef describes a native process to spawn in the worktree.
type ProcessDef struct { ... }
```

Also add doc comments to all isolation constants and `EnvConfig`.

---

### P2 — Validation error messages name the correct party

**File:** `pkg/blueprint/validate.go`
**Problems:**
- Line 85: process-port collision says "collides with service" when both
  are processes.
- Line 65: two ports within the same service say "services \"web\" and
  \"web\"".

**Fix:** Track the type of the first claimant:

```go
type portVarClaim struct {
    kind string // "service" or "process"
    name string
}
```

Report: `port var "REDIS_PORT" collides: service "redis" and process "workers"`.

---

### P3 — Test fixture isolation in `validate_test.go`

**File:** `pkg/blueprint/validate_test.go`
**Problem:** `validBP` is a package-level struct containing maps. Every
test does a shallow copy, then mutates the shared map. Tests pass only
because of execution order.

**Fix:** Replace `var validBP = ...` with `func newValidBP() *Blueprint`:

```go
func newValidBP() *Blueprint {
    return &Blueprint{
        Version: 1,
        Name:    "test",
        Services: map[string]ServiceDef{
            "db": {Isolation: IsolationLogical, Type: "postgres", Image: "postgres:16"},
        },
        Processes: []ProcessDef{
            {Name: "app", Isolation: IsolationNative, Command: "run", PortVar: "PORT"},
        },
    }
}
```

Each test calls `bp := newValidBP()` instead of `bp := *validBP`.

---

### P4 — Blueprint init stderr warnings → return warnings

**File:** `pkg/blueprint/init.go`
**Problem:** `InitFromRepo` prints warnings to `os.Stderr` (lines 51, 63,
95, 100, 135, 163, 168). A `pkg/` library coupled to stderr can't be
silenced or tested.

**Fix:** Change the signature to return warnings:

```go
func InitFromRepo(root string) (*Blueprint, []string, error)
```

The caller (`cmd/plax/main.go`) prints warnings to stderr. Tests can
assert on the warning slice.

---

### P5 — Registry encapsulation: UpdateInstance and sentinel errors

**File:** `pkg/registry/registry.go`
**Problems:**
- `Suspend`/`Resume` write `deps.Registry.Instances[name] = rec` directly
  because no `UpdateInstance` method exists.
- No sentinel errors — callers can't `errors.Is` on registry failures.

**Fix:**

```go
var ErrInstanceExists = errors.New("registry: instance already exists")
var ErrInstanceNotFound = errors.New("registry: instance not found")
var ErrPortAllocated = errors.New("registry: port already allocated")

func (r *Registry) UpdateInstance(name string, rec InstanceRecord) error {
    if _, ok := r.Instances[name]; !ok {
        return fmt.Errorf("%w: %s", ErrInstanceNotFound, name)
    }
    r.Instances[name] = rec
    return nil
}
```

Update `suspend.go`, `resume.go`, `portpool/pool.go` to use the new
methods/sentinels.

---

### P6 — Remove dead code

| What | Where | Action |
|---|---|---|
| `Reserve` method | `pkg/portpool/pool.go:49-56` | Remove (never called outside tests) |
| `sortedKeys` insertion sort | `pkg/instance/up.go:408-420` | Replace with `sort.Strings` |
| `splitEnv` hand-rolled | `pkg/instance/up.go:398-405` | Replace with `strings.Cut` |
| `containsStr` | `pkg/doctor/doctor_test.go:259-273` | Replace with `strings.Contains` |
| `Dimension.Name` field | `pkg/status/status.go:26-29` | Remove (write-only, excluded from JSON) |
| `deriveConnString` wrapper | `cmd/plax/main.go:1190-1192` | Remove, call `pgConnString` directly |
| `tryFlag` unused `name` param | `pkg/toolchain/toolchain.go:79` | Remove parameter |
| Dead `r.path = path` | `pkg/registry/registry.go:88` | Remove (field set at construction) |
| Dead comment re: future function | `pkg/derive/postgres/basemanager.go:611` | Remove or convert to TODO |

---

### P7 — Step numbering and error wrapping in `up.go`

**File:** `pkg/instance/up.go`
**Fix:** Renumber steps sequentially (1, 2, 3, 4, 5, 6, 7). Wrap bare
`return err` calls with context.

---

### P8 — Doc comments on Suspend, Resume, and toolchain API

**Files:** `pkg/instance/suspend.go`, `pkg/instance/resume.go`,
`pkg/toolchain/toolchain.go`

Add doc comments following the standard set by `Up` and `Down`:

```go
// Suspend terminates all processes and stops all containers for the named
// instance without removing its worktree, database, or registry entry.
// The instance can be restored with Resume.
func Suspend(...) error { ... }

// Resume restarts a suspended instance: starts stopped containers, probes
// port availability, re-derives .env, and spawns processes.
func Resume(...) error { ... }
```

For `toolchain.go`, restore the doc comments from `04-state.md`:

```go
// ParsePins reads a .tool-versions file and returns tool→version mappings.
// Returns (nil, nil) if the file does not exist.
func ParsePins(path string) (map[string]string, error) { ... }

// ResolveVersions probes each tool's binary and returns tool→resolved-version.
// Tools whose binaries are missing are silently omitted.
func ResolveVersions(pins map[string]string) map[string]string { ... }

// MatchesPin reports whether a resolved version satisfies a pin.
// Non-semver pins (lts, latest) return PinMatchUnverifiable.
func MatchesPin(pin, resolved string) PinMatch { ... }

// CompareVersions returns the names of tools whose resolved versions differ
// from the baseline, or that were added/removed. Sorted for determinism.
func CompareVersions(baseline, current map[string]string) []string { ... }
```

---

### P9 — Mailbox: stderr prints → return skip info, doc comments

**File:** `pkg/mailbox/mailbox.go`
**Problem:** `recvFile` prints warnings to `os.Stderr` (lines 106, 111, 120).
A `pkg/` library should return this information to the caller.

**Fix:** Change `Recv`/`RecvAll` to return a result struct:

```go
type RecvResult struct {
    Messages []Message
    Skipped  []string // filenames that could not be parsed
}
```

The CLI layer prints skipped-file warnings to stderr.

Also add doc comments:

```go
// Send writes a message to the named instance's mailbox.
// The write is atomic: the message appears fully formed or not at all.
// Returns an error if the mailbox directory does not exist.
func Send(root, name string, msg Message) (string, error) { ... }

// Recv reads up to count messages from the named instance's mailbox,
// oldest first, and removes them. Delivery is at-least-once: a message
// whose removal fails will be redelivered on the next call.
func Recv(root, name string, count int) (*RecvResult, error) { ... }
```

---

### P10 — `Rederive` unused context, `dataDrift` unused parameter

**File:** `pkg/instance/rederive.go`
**Fix:** Remove `ctx context.Context` from `Rederive` signature (it's never
used). Update the caller in `main.go`.

**File:** `pkg/status/status.go`
**Fix:** Remove unused `rec *registry.InstanceRecord` parameter from
`dataDrift`.

---

### P11 — CLI stdout/stderr separation and error swallowing

**File:** `cmd/plax/main.go`
**Fixes:**

1. `runSend` non-JSON mode: print the filename to stdout (it's a record),
   not stderr.

2. Add `default` case to the command switch:

```go
default:
    return fmt.Errorf("unknown command: %s", ctx.Command())
```

3. `runStatus`: when `loadBlueprintAndConnString` fails, print a stderr
   warning before continuing with degraded status.

4. `runDoctor`: use `loadBlueprintAndConnString` (which produces a clear
   "not found" message) instead of `loadBlueprint` (which wraps as
   "parsing").

5. `runAttach`: handle `filepath.Abs` error instead of discarding it.

---

### P12 — Portability comment on `PortOwner`

**File:** `pkg/portpool/probe.go`
**Fix:** Add comment to `PortOwner`:

```go
// PortOwner returns the PID and command name of the process bound to port.
// Linux-only: reads /proc/net/tcp and /proc/<pid>/. Returns ok=false on
// other platforms. ProbeFree is the portable alternative.
```

Also check `sc.Err()` after the scanner loop.

---

### P13 — Test naming convention

**Files:** All `*_test.go`
**Fix:** Rename tests to follow `Test<Package>_<Behavior>` per AGENTS.md.
E.g., `TestAllocate_FirstFree` → `TestPortPool_AllocateFirstFree`.

This is a bulk rename — do it with a single pass, no logic changes.

---

### P14 — Golden file comparison with `go-cmp`

**File:** `pkg/blueprint/init_test.go`
**Problem:** AGENTS.md mandates `go-cmp` for golden files, but the golden
test uses string equality and `go-cmp` isn't in `go.mod`.

**Fix:** Add `github.com/google/go-cmp` to `go.mod`. Use `cmp.Diff` for
golden file comparison.

---

### P15 — Process package: fix contradictory comment, errors.Is style

**File:** `pkg/process/supervisor.go`
**Fixes:**
1. Line 29 vs 58-63: reconcile "NOT waited on" with `_ = cmd.Wait()`.
   The correct comment: "Wait runs in a goroutine to reap the child if it
   dies while plax lives. If plax exits first, the child is reparented to
   init and reaped there."
2. Lines 119, 139: `err == syscall.ESRCH` → `errors.Is(err, syscall.ESRCH)`.

---

## CLI specification

No new commands or flags. All changes are internal.

---

## Error handling

| Failure mode | Expected behavior |
|---|---|
| `Recv` encounters corrupt message file | Filename returned in `Skipped`, not printed to stderr |
| `InitFromRepo` encounters warnings | Warnings returned as `[]string`, caller prints them |
| `Rederive` called | No unused `ctx` parameter |
| `Suspend` updates registry | Uses `UpdateInstance` method, not direct map write |
| Test isolation in `validate_test.go` | Each test gets a fresh blueprint via `newValidBP()` |

---

## Tests

| Test | What it verifies |
|---|---|
| `TestValidate_PortVarConflict_ProcessVsProcess` | P2: correct party named |
| `TestValidate_PortVarConflict_SameService` | P2: same-service collision message |
| `TestInit_Warnings_Returned` | P4: warnings in return value, not stderr |
| `TestRegistry_UpdateInstance` | P5: method works, sentinel error on missing |
| `TestMailbox_RecvResult_Skipped` | P9: skipped files in result struct |
| `TestDeriveMerged_AtomicWrite` | P5 (from Phase 8, if not done there) |

---

## Acceptance criteria

- [x] All exported types in `pkg/blueprint` have doc comments
- [x] All exported functions in `pkg/toolchain` have doc comments
- [x] All exported functions in `pkg/mailbox` have doc comments
- [x] `Suspend` and `Resume` have doc comments
- [x] No `pkg/` package writes to `os.Stderr` (except `testutil`)
- [x] No dead code remains (`Reserve`, `sortedKeys`, `splitEnv`, `containsStr`, `deriveConnString`, `Dimension.Name`)
- [x] All test names follow `Test<Package>_<Behavior>` convention
- [x] `validate_test.go` uses per-test constructors
- [x] Golden file comparison uses `go-cmp`
- [x] `up.go` step numbering is sequential
- [x] `errors.Is` used consistently for errno checks
- [x] All existing tests continue to pass
- [x] `go vet ./...` and `golangci-lint run` report no issues

---

## Dependencies

| Module | Version | Why |
|---|---|---|
| `github.com/google/go-cmp` | latest | Golden file comparison per AGENTS.md convention |
