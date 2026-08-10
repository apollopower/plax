# Phase 7 — Robustness

## Objective

Harden the codebase against real-world failure modes: process group survivor
leaks, non-atomic mailbox writes, fragile Docker error classification, DSN
parsing bugs, and toolchain pin validation gaps. These are the issues that
produce silent data corruption or misleading diagnostics.

---

## Package layout

```
pkg/process/
  supervisor.go                # Fix: group survivor semantics, pgid guard
  supervisor_test.go           # New: SIGKILL-path, survivor, pgid-guard tests
pkg/mailbox/
  mailbox.go                   # Fix: atomic write via temp+rename, count guard
  mailbox_test.go              # New: concurrent-sender test, ordering fix
pkg/derive/docker/
  driver.go                    # Fix: errdefs instead of strings.Contains
  network.go                   # Fix: errdefs instead of strings.Contains
pkg/derive/postgres/
  basemanager.go               # Fix: dsnForDB URL parsing, InstanceProvenance errors
  basemanager_test.go          # New: dsnForDB query-param tests
pkg/toolchain/
  toolchain.go                 # Fix: tri-state MatchesPin, v-prefix pins, CombinedOutput
  toolchain_test.go            # New: v-prefix, stderr, tri-state tests
pkg/worktree/
  git.go                       # Fix: SchemaFilesAtRef error propagation
  git_test.go                  # New: tests for all four functions
pkg/status/
  status.go                    # Fix: codeDrift behind count, InstanceProvenance error
pkg/registry/
  registry.go                  # Add: version gate on Open, State type constants
```

---

## Fixes

### F7 — Process termination leaks group survivors on Linux

**File:** `pkg/process/supervisor.go`
**Problem:** When the group leader dies but children survive (npm not
forwarding SIGTERM), `aliveAs` returns false (leader start time is 0), and
`Terminate` returns success. The orphaned process keeps running while
`down` tears down the worktree and database.

**Fix:**
1. Add `ErrGroupSurvivors` sentinel error.
2. After the wait loop, if the leader is gone but `kill(-pgid, 0)` still
   succeeds (group non-empty), return `ErrGroupSurvivors` instead of `nil`.
3. Callers (`down.go`, `suspend.go`) already pattern-match on sentinels —
   add `ErrGroupSurvivors` to the same handling path (log warning, continue).

```
after wait loop:
  if StartTime(pgid) != startTime:
    if IsAlive(pgid):
      return ErrGroupSurvivors  // leader gone, members alive
    return nil                  // group fully empty
```

4. Add `pgid <= 0` guard at the top of `Terminate` and `IsAlive`:

```go
if pgid <= 0 {
    return fmt.Errorf("process: invalid pgid %d", pgid)
}
```

**Tests:**
- `TestTerminate_SurvivorLeak` — spawn `sh -c 'trap "" TERM; sleep 300'`,
  call `Terminate`, assert `ErrGroupSurvivors`.
- `TestTerminate_InvalidPGID` — call with `pgid = 0` and `pgid = -5`,
  assert error.
- `TestTerminate_SIGKILLPath` — spawn a process that ignores SIGTERM,
  verify SIGKILL escalation covers it (on macOS path where `startTime==0`).

---

### F8 — Mailbox `Send` is not atomic

**File:** `pkg/mailbox/mailbox.go`
**Problem:** `O_CREATE|O_EXCL` guarantees unique names but not atomic
writes. A concurrent `Recv` can read a partial/empty file between `OpenFile`
and `Write`.

**Fix:** Write to a temp file in the same directory, then `os.Rename`:

```
Send(root, name, msg):
  ...
  tmpName := fmt.Sprintf(".tmp_%d_%s.json", now.UnixNano(), nonce)
  tmpPath := filepath.Join(dir, tmpName)
  write msg JSON to tmpPath (O_CREATE|O_EXCL|O_WRONLY, 0600)
  finalName := fmt.Sprintf("%d_%s.json", now.UnixNano(), nonce)
  finalPath := filepath.Join(dir, finalName)
  os.Rename(tmpPath, finalPath)
```

The `.tmp_` prefix ensures `Recv` (which filters by `.json` suffix and
sorts lexically) never sees the temp file. The rename is atomic on POSIX.

Also add `count < 1` guard to `Recv`:

```go
if count < 1 {
    return nil, fmt.Errorf("mailbox: recv count must be >= 1, got %d", count)
}
```

**Tests:**
- `TestSend_ConcurrentNoTornReads` — 50 goroutines send, 1 goroutine
  continuously recv; assert no partial reads.
- `TestRecv_ZeroCount` — `Recv(root, name, 0)` returns error.
- `TestRecv_NegativeCount` — `Recv(root, name, -1)` returns error.

---

### F9 — Docker error classification via string matching

**File:** `pkg/derive/docker/driver.go`, `pkg/derive/docker/network.go`
**Problem:** 8 occurrences of `strings.Contains(err.Error(), "No such
container")` etc. Error text varies across Docker versions and locales.
One upgrade could silently break all classification.

**Fix:** Replace all `strings.Contains` with `errdefs` checks:

```go
import "github.com/docker/docker/errdefs"

// Before
if strings.Contains(err.Error(), "No such container") { ... }

// After
if errdefs.IsNotFound(err) { ... }
```

Apply to all 8 sites in `driver.go` and both sites in `network.go`.

**Dependency:** `github.com/docker/docker/errdefs` (already indirect via
`docker/docker`; promote to direct).

---

### F10 — `dsnForDB` breaks on DSNs with slashes in query params

**File:** `pkg/derive/postgres/basemanager.go` (~line 499)
**Problem:** `strings.LastIndex("/")` finds the wrong `/` when query params
contain slashes (e.g., `?search_path=foo/bar`).

**Fix:** Use `net/url`:

```go
func (bm *BaseManager) dsnForDB(dbName string) string {
    u, err := url.Parse(bm.dsn)
    if err != nil {
        // fallback to last-/ before ?
        ...
    }
    u.Path = "/" + dbName
    return u.String()
}
```

**Test:** `TestDSNForDB_QueryParamSlash` — DSN with
`?search_path=foo/bar`, assert the database name is replaced correctly.

---

### F11 — `InstanceProvenance` swallows connection errors

**File:** `pkg/derive/postgres/basemanager.go` (~line 374)
**Problem:** `pgxpool.New` failure returns `(nil, nil)` — indistinguishable
from "no provenance table."

**Fix:** Return the error:

```go
pool, err := pgxpool.New(ctx, bm.dsnForDB(dbName))
if err != nil {
    return nil, fmt.Errorf("instance provenance: connect to %s: %w", dbName, err)
}
```

Also fix `BaseStatus` (~line 400) to not silently drop provenance read errors.

**Ripple:** `pkg/status/status.go:80-83` — `Build` currently swallows the
error. Thread it into the Dimension detail.

---

### F12 — `MatchesPin` tri-state, v-prefix pins, stderr capture

**File:** `pkg/toolchain/toolchain.go`
**Problems:**
1. `MatchesPin` returns `bool` — can't distinguish "mismatch" from
   "unverifiable" (`lts`/`latest`). Doctor reports Fail for unverifiable
   pins instead of the spec'd Warn.
2. Pins with `v` prefix (e.g., `"v22.19.0"`) never match — prefix is
   stripped from resolved tokens but not from the pin.
3. `tryFlag` uses `cmd.Output()` (stdout only) — tools like `java -version`
   that print to stderr are reported as not installed.

**Fixes:**

1. Add a tri-state return type:

```go
type PinMatch int

const (
    PinMatchYes PinMatch = iota
    PinMatchNo
    PinMatchUnverifiable
)

func MatchesPin(pin, resolved string) PinMatch { ... }
```

Update `doctor.go` caller: `PinMatchUnverifiable` → Warn, `PinMatchNo` →
Fail, `PinMatchYes` → Pass.

2. Normalize the pin before comparison:

```go
pin = strings.TrimPrefix(pin, "v")
pin = strings.TrimPrefix(pin, "go")
```

3. Use `CombinedOutput()` in `tryFlag`:

```go
out, err := cmd.CombinedOutput()
```

**Tests:**
- `TestMatchesPin_VPrefixedPin` — pin `"v22.19.0"` matches resolved `"22.19.0"`.
- `TestMatchesPin_LTS` — pin `"lts"` returns `PinMatchUnverifiable`.
- `TestMatchesPin_Latest` — pin `"latest"` returns `PinMatchUnverifiable`.
- `TestTryFlag_StderrOutput` — fake binary printing to stderr, exit 0.

---

### F13 — `SchemaFilesAtRef` swallows git errors

**File:** `pkg/worktree/git.go` (~line 61)
**Problem:** Returns `(nil, nil)` on any git failure. A corrupt repo is
indistinguishable from "no migration files at this ref."

**Fix:**

```go
out, err := cmd.Output()
if err != nil {
    return nil, fmt.Errorf("git ls-tree %s: %w", ref, err)
}
```

**Test:** `TestSchemaFilesAtRef_MissingRef` — call with a nonexistent ref,
assert error is non-nil.

---

### F14 — `codeDrift` suppresses behind count for commit-hash bases

**File:** `pkg/status/status.go` (~line 127)
**Problem:** When `BaseRef == "" && BaseCommit != ""`, detail says
`"behind ?"` even though `behind` was computed successfully. The condition
is unnecessary — `git rev-list --count <sha>...<branch>` works for commit
hashes.

**Fix:** Remove the conditional; always show the computed `behind` value.

---

### F15 — Registry version gate and State type

**File:** `pkg/registry/registry.go`
**Problems:**
1. `Open` accepts any `version` in the file — a future v2 file would be
   read silently through a v1-shaped struct.
2. `InstanceRecord.State` is a bare `string` with scattered literals
   (`"running"`, `"suspended"`).

**Fixes:**

1. Add version check in `Open`:

```go
if r.Version != 1 {
    return nil, fmt.Errorf("registry: unsupported version %d (want 1)", r.Version)
}
```

2. Add typed state:

```go
type State string

const (
    StateRunning   State = "running"
    StateSuspended State = "suspended"
)
```

Update `InstanceRecord.State` to `State`. Update all callers (`up.go`,
`suspend.go`, `resume.go`, tests) to use the constants.

---

## CLI specification

No new commands or flags. All changes are internal to existing packages.

---

## Error handling

| Failure mode | Expected behavior |
|---|---|
| Process group leader dies, children survive | `ErrGroupSurvivors` returned; caller logs warning, continues teardown |
| `Terminate(0, ...)` or `Terminate(-5, ...)` | Error returned immediately, no signal sent |
| Concurrent mailbox send + recv | No torn reads; recv never sees partial files |
| Docker error message text changes | Classification still works via `errdefs` |
| DSN has `?search_path=foo/bar` | Database name replaced correctly |
| Instance DB unreachable | `InstanceProvenance` returns error, not nil |
| Tool prints version to stderr | Detected as installed |
| Pin is `lts` or `latest` | Doctor warns "unverifiable," does not fail |
| Pin is `v22.19.0` | Matches resolved `22.19.0` |
| Git ref missing in `SchemaFilesAtRef` | Error propagated to caller |
| Registry file has `version: 2` | `Open` returns error |

---

## Tests

| Test | What it verifies |
|---|---|
| `TestTerminate_SurvivorLeak` | F7: `ErrGroupSurvivors` on leader-dead/group-alive |
| `TestTerminate_InvalidPGID` | F7: pgid <= 0 returns error |
| `TestTerminate_SIGKILLPath` | F7: SIGKILL escalation covered |
| `TestSend_ConcurrentNoTornReads` | F8: atomic write under concurrency |
| `TestRecv_ZeroCount` / `TestRecv_NegativeCount` | F8: count guard |
| `TestDSNForDB_QueryParamSlash` | F10: DSN with slashes in params |
| `TestMatchesPin_VPrefixedPin` | F12: v-prefix normalization |
| `TestMatchesPin_LTS` / `TestMatchesPin_Latest` | F12: tri-state for non-semver pins |
| `TestTryFlag_StderrOutput` | F12: stderr capture |
| `TestSchemaFilesAtRef_MissingRef` | F13: error propagation |
| `TestOpen_UnsupportedVersion` | F15: version gate |
| `TestState_Constants` | F15: State type used consistently |

---

## Acceptance criteria

- [ ] `Terminate` returns `ErrGroupSurvivors` when leader dies but children survive
- [ ] `Terminate` with `pgid <= 0` returns error without sending signals
- [ ] Mailbox `Send` + concurrent `Recv` never produces torn reads
- [ ] All Docker error classification uses `errdefs`, zero `strings.Contains` on error text
- [ ] `dsnForDB` handles DSNs with slashes in query parameters
- [ ] `InstanceProvenance` returns error on connection failure
- [ ] `MatchesPin` returns `PinMatchUnverifiable` for `lts`/`latest`
- [ ] Doctor reports Warn (not Fail) for non-semver pins
- [ ] `SchemaFilesAtRef` propagates git errors
- [ ] `codeDrift` always shows computed `behind` count
- [ ] `Open` rejects registry files with unknown version
- [ ] `InstanceRecord.State` uses typed constants throughout
- [ ] All existing tests continue to pass
- [ ] `go vet ./...` and `golangci-lint run` report no issues

---

## Dependencies

| Module | Version | Why |
|---|---|---|
| `github.com/docker/docker/errdefs` | v27.1.1+incompatible | Typed Docker error classification (promoted from indirect) |
