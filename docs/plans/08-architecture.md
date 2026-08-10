# Phase 8 — Architecture

## Objective

Bring the codebase into alignment with its stated values: CSP for concurrency,
thin CLI shell, and registry inter-process safety. These are structural
changes — they touch multiple packages and introduce new patterns.

---

## Package layout

```
pkg/portpool/
  pool.go                      # Refactor: CSP allocator goroutine
  pool_test.go                 # New: concurrent allocation tests
pkg/instance/
  up.go                        # Refactor: concurrent container/process start
  down.go                      # Refactor: concurrent stop (where safe)
  instance.go                  # Refactor: narrower Deps or constructor validation
pkg/registry/
  registry.go                  # Add: file locking (flock) for Open/Save
  lock.go                      # New: advisory file lock helper
cmd/plax/
  main.go                      # Refactor: extract business logic to pkg/
pkg/stamp/
  stamp.go                     # New: computeStamp/stampNotice (from main.go + doctor.go)
pkg/doctor/
  doctor.go                    # Use pkg/stamp instead of duplicating logic
pkg/derive/env/
  derive.go                    # Fix: DeriveMerged atomic write
```

---

## Refactors

### R1 — CSP-based port allocation

**Files:** `pkg/portpool/pool.go`
**Problem:** AGENTS.md mandates CSP for port allocation. Current
implementation uses direct shared-map mutation with zero synchronization.
Two concurrent `Allocate` calls would race.

**Design:** Single allocator goroutine owning port state. Requests arrive
over a channel; replies return over per-request channels.

```go
type PortPool struct {
    reqCh chan allocRequest
    done  chan struct{}
}

type allocRequest struct {
    instance string
    service  string
    reply    chan allocResult
}

type allocResult struct {
    port int
    err  error
}

func New(start, end int, reg *registry.Registry) *PortPool {
    p := &PortPool{
        reqCh: make(chan allocRequest),
        done:  make(chan struct{}),
    }
    go p.run(start, end, reg)
    return p
}

func (p *PortPool) run(start, end int, reg *registry.Registry) {
    for {
        select {
        case req := <-p.reqCh:
            port, err := allocate(start, end, reg, req.instance, req.service)
            req.reply <- allocResult{port, err}
        case <-p.done:
            return
        }
    }
}

func (p *PortPool) Allocate(instance, service string) (int, error) {
    reply := make(chan allocResult, 1)
    p.reqCh <- allocRequest{instance, service, reply}
    r := <-reply
    return r.port, r.err
}

func (p *PortPool) Close() {
    close(p.done)
}
```

Move default range values (3000–4000) into `New()`. Reject invalid ranges
(`start >= end`, `start < 1024`, `end > 65535`) at construction.

**Tests:**
- `TestPortPool_ConcurrentAllocate` — 20 goroutines allocate simultaneously,
  assert all get unique ports, no duplicates.
- `TestPortPool_ConcurrentAllocateRelease` — interleaved allocate/release
  from multiple goroutines.
- `TestPortPool_New_InvalidRange` — `New(0, 0, reg)` uses defaults;
  `New(5000, 4000, reg)` returns error.

---

### R2 — Concurrent container and process startup

**Files:** `pkg/instance/up.go`
**Problem:** Containers start sequentially, then processes start
sequentially. For 3 containers + 2 processes, this is 5 sequential
operations when they could be 2 parallel batches.

**Design:** Use `golang.org/x/sync/errgroup` (already in `go.mod` as
indirect) to fan out container starts, then fan out process starts.
Keep cleanup paths sequential.

```go
// Step 7: Start containers concurrently.
g, gctx := errgroup.WithContext(ctx)
for name, svc := range dedicatedServices {
    g.Go(func() error {
        return startContainer(gctx, deps, name, svc, containerIDs)
    })
}
if err := g.Wait(); err != nil {
    return fmt.Errorf("starting containers: %w", err)
}

// Step 8: Start processes concurrently.
g2, gctx2 := errgroup.WithContext(ctx)
for _, proc := range deps.Blueprint.Processes {
    g2.Go(func() error {
        return startProcess(gctx2, deps, proc, pids, pidStarts)
    })
}
if err := g2.Wait(); err != nil {
    return fmt.Errorf("starting processes: %w", err)
}
```

⚠ The rollback cleanup for containers must remain correct: the closure
captures the shared `containerIDs` map. Since `errgroup` goroutines write
concurrently, protect the map with a `sync.Mutex` or use a channel to
collect results. Prefer channel-based collection per CSP value:

```go
type containerResult struct {
    name string
    id   string
    err  error
}
ch := make(chan containerResult, len(dedicatedServices))
for name, svc := range dedicatedServices {
    go func() {
        id, err := startContainer(ctx, deps, name, svc)
        ch <- containerResult{name, id, err}
    }()
}
for range dedicatedServices {
    r := <-ch
    if r.err != nil {
        return fmt.Errorf("starting container %s: %w", r.name, r.err)
    }
    containerIDs[r.name] = r.id
}
```

Same pattern for `down.go` (concurrent stop) and `suspend.go`.

**Tests:**
- `TestUp_ConcurrentContainerStart` — fake Docker driver records call
  order; assert containers started concurrently (not sequentially).
- `TestUp_ConcurrentProcessStart` — same for native processes.
- `TestUp_ConcurrentPartialFailure` — one container fails, assert rollback
  cleans up the others.

---

### R3 — Registry file locking

**Files:** `pkg/registry/registry.go`, new `pkg/registry/lock.go`
**Problem:** Two concurrent `plax` processes can corrupt each other's
registry state (lost update on Save).

**Design:** Advisory file locking via `flock(2)` on a `.plax/registry.lock`
file. Lock is held for the duration of Open→mutate→Save.

```go
// lock.go
//go:build unix

package registry

import (
    "os"
    "syscall"
)

func lockFile(path string) (*os.File, error) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
    if err != nil {
        return nil, err
    }
    if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
        f.Close()
        return nil, fmt.Errorf("registry: acquiring lock: %w", err)
    }
    return f, nil
}

func unlockFile(f *os.File) {
    syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
    f.Close()
}
```

`Open` acquires the lock; `Save` releases it. For read-only operations
(`ls`, `status`), use `LOCK_SH` instead of `LOCK_EX`.

Add `Close()` method to `Registry` that releases the lock if held.

**Tests:**
- `TestRegistry_ConcurrentOpenSave` — two goroutines open, mutate, save;
  assert both sets of changes are present (no lost update).
- `TestRegistry_LockTimeout` — hold lock in one goroutine, attempt to open
  in another with a timeout; assert timeout error.

---

### R4 — Extract business logic from `main.go`

**Files:** `cmd/plax/main.go`, new `pkg/stamp/stamp.go`
**Problem:** `main.go` is 1192 lines and contains business logic:
`pgConnString`, `computeStamp`, `stampNotice`, `loadInstanceEnv`,
`findShell`, `formatPorts`, `formatAge`. The stamp logic is duplicated in
`pkg/doctor`.

**Design:**
1. Create `pkg/stamp/` — move `computeStamp`, `stampNotice`,
   `printStampNotice` there. Export `Compute(bp *blueprint.Blueprint,
   root string) (registry.BlueprintStamp, error)` and `Check(reg
   *registry.Registry, bp *blueprint.Blueprint, root string) (string,
   bool)` (returns notice text and whether it changed).
2. Move `pgConnString` to `pkg/derive/postgres/` as an exported function.
3. Move `loadInstanceEnv` to `pkg/derive/env/` (it merges env for attach).
4. Move `findShell` to a small `pkg/shell/` or into `cmd/plax` as a
   private helper (it's CLI-specific).
5. Move `formatPorts`, `formatAge` to `cmd/plax` format helpers (they're
   display logic, fine to keep in CLI).

Remove `deriveConnString` wrapper (calls `pgConnString` directly).

After this refactor, `main.go` should contain only: Kong CLI struct
definitions, command routing, output formatting, and error display.

---

### R5 — `DeriveMerged` atomic write

**File:** `pkg/derive/env/derive.go`
**Problem:** `os.WriteFile` at line 70 is not atomic. A kill mid-write
leaves a truncated `.env`.

**Fix:** Write to temp file in same directory, then `os.Rename`:

```go
dir := filepath.Dir(outputPath)
tmp, err := os.CreateTemp(dir, ".env-*.tmp")
if err != nil {
    return fmt.Errorf("env: create temp: %w", err)
}
if _, err := tmp.WriteString(out); err != nil {
    tmp.Close()
    os.Remove(tmp.Name())
    return fmt.Errorf("env: write temp: %w", err)
}
tmp.Close()
if err := os.Rename(tmp.Name(), outputPath); err != nil {
    os.Remove(tmp.Name())
    return fmt.Errorf("env: rename: %w", err)
}
```

---

### R6 — `Deps` struct constructor validation

**File:** `pkg/instance/instance.go`
**Problem:** The `Deps` struct is a grab bag. Callers must know which
fields are required for which operation. Nil fields cause panics.

**Design:** Add constructor functions that validate required fields:

```go
func NewUpDeps(bp *blueprint.Blueprint, reg *registry.Registry, pool
    *portpool.PortPool, bm BaseManager, docker DockerDriver, root string)
    (*Deps, error) {
    if bp == nil { return nil, errors.New("instance: blueprint required") }
    if reg == nil { return nil, errors.New("instance: registry required") }
    ...
    return &Deps{...}, nil
}
```

Alternatively, split into per-operation structs (`UpDeps`, `DownDeps`).
The constructor approach is less invasive.

---

## CLI specification

No new commands or flags. The `main.go` refactor is internal only.

---

## Error handling

| Failure mode | Expected behavior |
|---|---|
| 20 goroutines allocate ports concurrently | All receive unique ports, no duplicates, no races |
| Registry locked by another process | `Open` blocks until lock is available (or returns timeout error) |
| Container start fails mid-batch | Other containers roll back, error names the failing service |
| `DeriveMerged` killed mid-write | Original `.env` file is intact (temp file abandoned) |
| Stamp computation needed by doctor | Uses shared `pkg/stamp`, no duplication |

---

## Tests

| Test | What it verifies |
|---|---|
| `TestPortPool_ConcurrentAllocate` | R1: unique ports under concurrency |
| `TestPortPool_ConcurrentAllocateRelease` | R1: interleaved alloc/release |
| `TestPortPool_New_InvalidRange` | R1: constructor validation |
| `TestUp_ConcurrentContainerStart` | R2: parallel container starts |
| `TestUp_ConcurrentProcessStart` | R2: parallel process starts |
| `TestUp_ConcurrentPartialFailure` | R2: rollback on partial failure |
| `TestRegistry_ConcurrentOpenSave` | R3: no lost updates |
| `TestRegistry_LockTimeout` | R3: lock contention behavior |
| `TestStamp_Compute_Matches` | R4: extracted stamp logic works |
| `TestDeriveMerged_AtomicWrite` | R5: no truncated output |
| `TestNewUpDeps_MissingFields` | R6: constructor validation |

---

## Acceptance criteria

- [ ] `go test -race` passes with 20 concurrent port allocations
- [ ] Two concurrent `plax up` commands do not corrupt the registry
- [ ] Container starts are concurrent (measurable: 3 containers start in ~1x single-container time, not 3x)
- [ ] Process starts are concurrent
- [ ] `main.go` contains no business logic (only routing, formatting, error display)
- [ ] `pkg/stamp` is the single source of stamp computation
- [ ] `doctor.go` imports `pkg/stamp` instead of duplicating logic
- [ ] `DeriveMerged` writes atomically
- [ ] `Deps` constructors validate required fields
- [ ] All existing tests continue to pass
- [ ] `go vet ./...` and `golangci-lint run` report no issues

---

## Dependencies

| Module | Version | Why |
|---|---|---|
| `golang.org/x/sync` | v0.22.0 | `errgroup` for concurrent startup (promoted from indirect) |
