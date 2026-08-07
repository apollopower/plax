# Phase 4 — State Management and Drift Detection

## Objective

Give instances a second lifecycle state (`suspended`) and make drift
visible: `suspend`/`resume` to stop and restart workloads without losing
state, `status` for a five-dimension drift report, `doctor` for a full
validation pass, and `rederive` to regenerate `.env` files after the
template changes.

---

## Changes from the previous sketch

This plan replaces the first-draft `04-state.md`. Decisions made while
grounding it in the shipped Phase 2/3 code:

| Sketch said | This plan says | Why |
|---|---|---|
| `rederive [--all]`; without `--all` only running/suspended | `rederive` covers every registered instance; `--all` dropped | The registry only holds live states (`running`, `suspended`) — the flag's distinction was vestigial |
| `base refresh` updates the registry config stamp | `base refresh` touches nothing in the registry | The stamp hashes blueprint input files (compose, template, toolchain); base staleness is already reported by the Data dimension via provenance versions. Refresh shipped in Phase 2 — Phase 4 only verifies it end-to-end |
| Host drift compares recorded `node@X bun@Y` against the machine now | Record resolved tool versions at `up` (new `Provenance.ToolVersions`), compare against re-resolved versions at `status` | Phase 3 records the toolchain *file hash*, which cannot see a machine-side upgrade with an unchanged pin — the exact drift the design doc (§3) calls out |
| Code drift against `main` | Against the base ref recorded at `up` (`InstanceRecord.BaseRef`); fallback chain for Phase 3 records | Instances are created from whatever HEAD the user is on, which is not always `main` |
    | Schema drift via the ORM's applied-migrations table | Hash comparison: instance provenance `schema_hash` vs migration filenames on the base ref via `git ls-tree` | The applied-migrations table name is ORM-specific. Set-difference is deferred until the blueprint declares one (see Deferred items) |
| Migrations live at `src/db/migrations` (hardcoded in Phase 2) | New optional blueprint field `seed.migrations_dir`; empty keeps the Phase 2 default | The hardcoded path is sample-repo residue; the field makes it a contract |

---

## Package layout

```
cmd/plax/main.go               # Add suspend, resume, status, doctor, rederive; stamp notice
pkg/
  instance/
    suspend.go                 # Suspend: stop workloads, keep state
    resume.go                  # Resume: port probe, restart, drift report
    rederive.go                # Rederive: regenerate instance .env files
    up.go                      # Record BaseRef/BaseCommit/ToolVersions at creation
    instance.go                # DockerDriver += StartService
    suspend_test.go            # Suspend tests (fake Docker, real git)
    resume_test.go             # Resume tests (fake Docker/BM, real git)
    rederive_test.go           # Rederive tests (temp repos)
  status/
    status.go                  # Drift report: build + render (table and JSON)
    status_test.go             # Per-dimension tests (real git, fake BM)
  doctor/
    doctor.go                  # Four-area validation pass
    doctor_test.go             # Fixture-based checks
  toolchain/
    toolchain.go               # Parse .tool-versions, resolve versions, compare
    toolchain_test.go          # Parser + resolver tests (fake binaries on PATH)
  portpool/
    probe.go                   # ProbeFree, PortOwner (OS-level port checks)
    probe_test.go              # Listener round-trip tests
  worktree/
    git.go                     # HeadRef, RefExists, AheadBehind, SchemaFilesAtRef
    git_test.go                # Tests against temp git repos
  derive/
    postgres/
      basemanager.go           # + InstanceProvenance; migrations dir from blueprint
      provenance.go            # + HashMigrationNames (extracted from ComputeSchemaHash)
    docker/
      driver.go                # + StartService
    env/
      derive.go                # + DeriveMerged (overrides as map, for rederive)
```

End-to-end coverage extends `cmd/plax/e2e_test.go` with suspend/resume and
drift scenarios (requires git + Postgres + Docker + python3; skipped
otherwise).

---

## Type specifications

### Registry schema changes (`pkg/registry/registry.go`)

Additive only; all new fields are `omitempty`, so registries written by
Phase 3 load unchanged and no `Version` bump is needed.

```go
type InstanceRecord struct {
    // ... existing fields (Phase 1/3) ...

    // BaseRef is the branch HEAD pointed at when the instance was created
    // (e.g. "main"). Empty when HEAD was detached or the record predates
    // Phase 4.
    BaseRef    string `json:"base_ref,omitempty"`
    // BaseCommit is the SHA BaseRef pointed at, for ahead-only comparison
    // when the ref itself is gone.
    BaseCommit string `json:"base_commit,omitempty"`
}

type Provenance struct {
    BaseVersion int               `json:"base_version"`
    Toolchain   string            `json:"toolchain"`             // file hash (Phase 3)
    // ToolVersions maps tool name → resolved version string at creation
    // time (e.g. "nodejs" → "v22.19.0"). Nil for Phase 3 records.
    ToolVersions map[string]string `json:"tool_versions,omitempty"`
}
```

### Valid `InstanceRecord.State` values

| State | Meaning | Set by |
|---|---|---|
| `"running"` | Workloads are up (PIDs and ContainerIDs recorded) | `up`, `resume` |
| `"suspended"` | Workloads stopped; ports kept by record only | `suspend` |

Transitions: `up` → running; `suspend` running → suspended (idempotent on
suspended); `resume` suspended → running; `down` from either. No other
transitions exist.

### Blueprint schema change (`pkg/blueprint/blueprint.go`)

```go
type SeedConfig struct {
    Migrate string `json:"migrate"`
    Command string `json:"command"`
    Workdir string `json:"workdir"`
    // MigrationsDir is the repo-relative directory of migration files,
    // used for the schema hash. Empty keeps the Phase 2 default
    // "src/db/migrations".
    MigrationsDir string `json:"migrations_dir,omitempty"`
}
```

### `pkg/toolchain/toolchain.go`

```go
package toolchain

// ParsePins reads a .tool-versions file: tool name → pinned version.
// Lines are "tool version [more...]"; the first version wins. Blank lines
// and # comments are skipped. A missing file returns nil, nil — callers
// treat it as "no toolchain recorded", not an error.
func ParsePins(path string) (map[string]string, error)

// ResolveVersions runs each pinned tool's version command and returns
// tool → resolved version (first line of output, trimmed). A tool whose
// binary is missing or fails is absent from the result. Each exec has a
// 2s timeout so a hung shim cannot block the caller.
func ResolveVersions(pins map[string]string) map[string]string

// Diff compares recorded against current. One Diff per tool whose
// resolved version changed, appeared, or disappeared.
type Diff struct {
    Tool     string `json:"tool"`
    Recorded string `json:"recorded"` // "" when absent at creation
    Current  string `json:"current"`  // "" when absent now
}

func CompareVersions(recorded, current map[string]string) []Diff

// MatchesPin reports whether a resolved version string satisfies a pin,
// e.g. pin "22.19.0" matches resolved "v22.19.0". Heuristic: take each
// whitespace-separated token of the resolved line, strip a "v" or "go"
// prefix, and match when the token equals the pin or extends it at a dot
// boundary (pin "1.26" matches "go1.26.5"; pin "1.2" does not match
// "11.2.3"). Non-semver pins ("lts", "latest") never match — callers
// report them as unverifiable, not mismatched.
func MatchesPin(pin, resolved string) bool
```

Tool-name → binary mapping (asdf plugin names differ from binaries):

| Tool name | Binary | Version command |
|---|---|---|
| `nodejs` | `node` | `--version` |
| `golang` | `go` | `version` (no dash form) |
| `python` | `python3` | `--version` |
| anything else | identity | `--version`, fallback `version` |

### `pkg/portpool/probe.go`

```go
package portpool

// ProbeFree reports whether nothing is listening on 127.0.0.1:port.
// It is the exported form of the bind-and-close check Allocate already
// uses. Advisory only — the OS may grant the port to someone else before
// the caller binds it (TOCTOU); Docker/spawn failures remain the authority.
func ProbeFree(port int) bool

// PortOwner identifies the process listening on port. Returns ok=false
// when no listener exists or the platform cannot say. Linux implementation:
// /proc/net/tcp{,6} for the socket inode, then /proc/<pid>/fd for the
// owner; cmdline is /proc/<pid>/cmdline with NULs rendered as spaces.
func PortOwner(port int) (pid int, cmdline string, ok bool)
```

⚠ Platform limitation: `PortOwner` needs `/proc` (Linux), like the PID
identity guard. Elsewhere it returns `ok=false` and resume's error names
the port without an owner.

### `pkg/worktree/git.go`

```go
package worktree

// HeadRef returns the current branch name and commit SHA of the checkout
// at repoRoot. ref is "" on a detached HEAD.
func HeadRef(repoRoot string) (ref, commit string, err error)

// RefExists reports whether ref resolves to a commit.
func RefExists(repoRoot, ref string) bool

// AheadBehind counts commits on each side of baseRef...branch:
// ahead = commits in branch not in baseRef, behind = the reverse
// (git rev-list --left-right --count; left is behind, right is ahead).
func AheadBehind(repoRoot, baseRef, branch string) (ahead, behind int, err error)

// SchemaFilesAtRef lists the migration filenames directly inside dir on
// ref, without touching any checkout: git ls-tree <ref> -- <dir>/, keeping
// blobs only (matching os.ReadDir's non-recursive, files-only semantics).
// Names are bare (directory prefix stripped) so the hash matches
// postgres.HashMigrationNames over the same directory on disk.
// A ref without the directory returns an empty slice.
func SchemaFilesAtRef(repoRoot, ref, dir string) ([]string, error)
```

### `pkg/derive/postgres` additions

```go
// HashMigrationNames hashes a sorted list of migration filenames
// (SHA-256, newline-joined, hex). Extracted from ComputeSchemaHash so the
// same algorithm runs over git ls-tree output; ComputeSchemaHash delegates
// to it and its on-disk behavior is unchanged.
func HashMigrationNames(names []string) string

// InstanceProvenance reads the provenance row from an instance database.
// Returns nil, nil when the database or the table does not exist.
// Instance databases are never locked, so no unlock dance is needed.
func (bm *BaseManager) InstanceProvenance(ctx context.Context, dbName string) (*ProvenanceRow, error)

// InstanceDBExists reports whether the instance database exists. Exported
// for doctor's dangling-record check — InstanceProvenance conflates
// "no database" with "no provenance row", and doctor needs the difference.
func (bm *BaseManager) InstanceDBExists(ctx context.Context, dbName string) (bool, error)
```

`BaseManager` replaces its two hardcoded `src/db/migrations` uses with the
blueprint's `Seed.MigrationsDir`, defaulting to the old path when empty.
No behavior change for existing blueprints.

### `pkg/derive/docker/driver.go` addition

```go
// StartService starts an existing stopped container. Returns
// alreadyRunning=true (and does nothing) when the container is already up,
// so resume's rollback only stops what it started.
func (d *Driver) StartService(ctx context.Context, containerID string) (alreadyRunning bool, err error)

// ServiceExists reports whether the container exists, running or stopped.
// ServiceRunning conflates "stopped" with "gone" (both return false), and
// doctor needs the difference: stopped is expected for a suspended
// instance, gone is not.
func (d *Driver) ServiceExists(ctx context.Context, containerID string) (bool, error)
```

`instance.DockerDriver` gains `StartService`; `doctor`'s driver interface
takes `ServiceRunning` + `ServiceExists`. The Phase 3 fakes grow the stubs.

### `pkg/derive/env/derive.go` addition

```go
// DeriveMerged is Derive with the overrides already parsed. Rederive
// merges the user's .env over the instance's existing .env (both minus
// hole keys) and passes the result here. Precedence is unchanged:
// holes > overrides > template.
func DeriveMerged(templatePath string, overrides map[string]string, holes map[string]string, values map[string]string, outputPath string) error
```

`Derive` becomes a thin wrapper: parse the overrides file, call
`DeriveMerged`. `ParseFileRaw` (exact-key, verbatim-value — exported from
the existing private function) is added alongside it; rederive uses this
so secrets round-trip without corruption (the existing exported
`ParseFile` strips quotes).

### `pkg/status/status.go`

```go
package status

// Level is one dimension's verdict.
type Level string

const (
    OK      Level = "ok"      // compared, no drift
    Drift   Level = "drift"   // compared, drift detected
    Unknown Level = "unknown" // could not compare (backend down, predates Phase 4)
)

type Dimension struct {
    Name   string `json:"-"`       // dimension name, set by Build
    Level  Level  `json:"status"`
    Detail string `json:"detail"`  // one line, e.g. "ahead 3, behind 1"
}

type Report struct {
    Instance  string            `json:"instance"`
    State     string            `json:"state"`
    Code      Dimension `json:"code"`
    Schema    Dimension `json:"schema"`
    Data      Dimension `json:"data"`
    Host      Dimension `json:"host"`
    Config    Dimension `json:"config"`
}

// Deps for status. BM may be nil — Data and Schema then report unknown.
// Docker is not needed: drift is about declarations, not liveness.
type Deps struct {
    Blueprint    *blueprint.Blueprint
    Registry     *registry.Registry
    BM           BaseManager            // BaseStatus + InstanceProvenance
    RepoRoot     string
    Branch       string                 // instance branch name
    CurrentStamp registry.BlueprintStamp // pre-computed by the CLI; avoids importing instance
}

// Build computes the drift report for name. It never fails because a
// dimension failed — each dimension degrades to Unknown with the reason.
// It fails only when the instance is not registered.
func Build(ctx context.Context, deps *Deps, name string) (*Report, error)
```

### `pkg/doctor/doctor.go`

```go
package doctor

type Level string

const (
    Pass Level = "ok"
    Warn Level = "warn"
    Fail Level = "fail"
)

type Check struct {
    Area    string `json:"area"`    // blueprint-vs-repo, blueprint-vs-registry, repo-vs-machine, base
    Level   Level  `json:"level"`
    Message string `json:"message"` // what was found, and the remedy when failed
}

type Report struct {
    Checks []Check `json:"checks"`
}

// Failed reports whether any check failed (drives the exit code).
func (r *Report) Failed() bool

// Run executes all checks. BM and Docker may be nil — their reachability
// checks fail and their dependent checks are skipped, not duplicated.
func Run(ctx context.Context, deps *Deps) *Report

type Deps struct {
    Blueprint *blueprint.Blueprint
    Registry  *registry.Registry
    BM        BaseManager  // nil when Postgres is unreachable
    Docker    DockerDriver // nil when the daemon is unreachable
    RepoRoot  string
}
```

### `pkg/instance` additions

| Function | Signature | Behavior |
|---|---|---|
| `Suspend` | `func Suspend(ctx context.Context, deps *Deps, name string) error` | Stop processes and containers; keep DB, ports, worktree, containers. State → suspended, PIDs cleared. Idempotent. |
| `Resume` | `func Resume(ctx context.Context, deps *Deps, name string) error` | Probe ports, start containers, respawn processes, verify liveness, state → running. All-or-nothing: failure rolls back to suspended. |
| `Rederive` | `func Rederive(ctx context.Context, deps *Deps) error` | Regenerate `.env` for every registered instance; print a key-level diff per changed file. |
| `ComputeBlueprintStamp` | (moves to `cmd/plax/main.go` as an unexported helper) | The logic (SHA-256 over compose, env template, toolchain) lives in the CLI so `status` does not import `instance`. Status gets the pre-computed stamp via `Deps.CurrentStamp`. |

Per-command `Deps` needs (extends the Phase 3 table):

  - **Suspend**: Registry. Docker optional — nil skips container stops with a
  warning. Blueprint, Pool, BM, RepoRoot unused (RepoRoot is in the shared
  Deps struct for Down; suspend's path does not reference it).
    - **Resume**: Registry, RepoRoot, Blueprint, Docker (required when the
  record has containers). Pool unused (ports come from the record). BM unused
  by the mechanics; the CLI opens it tolerantly for the drift report.
- **Rederive**: Registry, RepoRoot, Blueprint. No backends at all — it is
  files only.
- **Status**: Registry, RepoRoot, Blueprint. BM optional.

---

## Algorithms

Context cancellation is respected throughout, as in Phase 3. `Suspend` is
best-effort like `Down` (a half-suspended instance is safely re-suspendable);
`Resume` is all-or-nothing like `Up` (a half-resumed instance rolls back to
suspended so it stays retryable).

### Suspend

1. **Load record.** `deps.Registry.GetInstance(name)` → must exist.
   ⚠ Not found → `instance "<name>" not found`. Exit 1.
2. **Idempotence.** If `rec.State == "suspended"`: print
   `instance <name> is already suspended` to stderr, exit 0.
3. **Stop native processes.** For each `rec.PIDs`:
   `process.Terminate(pgid, rec.PIDStarts[proc], 5*time.Second)`.
   ⚠ Same tolerance as `Down`: already-dead is a no-op, `ErrStaleProcess`
   is a note, other errors are warnings. Suspend never fails because a
   process would not die — the port probe at resume will name the survivor.
4. **Stop dedicated containers.** For each `rec.ContainerIDs`:
   `deps.Docker.StopService(ctx, cid)`. Stopped containers keep their
   config and named volumes; that is what resume reuses.
   ⚠ Nil Docker → warning, skip. Stop failure → warning, continue.
5. **Update registry.** `State → "suspended"`, `PIDs = nil`,
   `PIDStarts = nil` (omitempty drops them). Keep Ports, ContainerIDs,
   DBName, Provenance, BaseRef, BaseCommit, CreatedAt. Save.
   ⚠ Ports stay allocated in the registry — by record only. Nothing is
   bound; the OS may give them away while the instance sleeps (§2.2).
6. **Print** `instance <name> suspended` to stderr.

The database is deliberately untouched: a `logical` service is not a
workload, and suspending must not drop data.

### Resume

1. **Load record.** Must exist; `rec.State` must be `"suspended"`.
   ⚠ `"running"` → `instance "<name>" is already running`. Exit 1.
2. **Probe ports.** For each var → port in `rec.Ports`:
   `portpool.ProbeFree(port)`. If taken, `portpool.PortOwner(port)` and fail:
   - owner known → `port <N> (<VAR>) is in use by pid <pid> (<cmdline>) — free the port and retry, or run 'plax down <name>' and 'plax up <name>' to reallocate`
   - unknown → `port <N> (<VAR>) is in use (owner unknown) — ...` (same remedy)
   ⚠ Resume never moves to a different port. The old one is written into
     `.env`, bookmarks, and OAuth redirect URIs the machine does not
     control (design §4).
   ⚠ Probing happens before any side effect, so this failure is clean.
3. **Start containers.** If `len(rec.ContainerIDs) > 0` and
   `deps.Docker == nil` → fail: `docker unavailable — cannot start <N>
   container(s); fix Docker and retry`. Otherwise for each recorded
   container, `StartService(ctx, cid)`:
   ⚠ `No such container` → fail: `container for "<svc>" no longer exists —
     run 'plax down <name>' then 'plax up <name>' to rebuild`. Data in the
     instance DB survives; named volumes are separate from containers.
   ⚠ Track which containers this call started (`alreadyRunning=false`)
     for rollback.
4. **Respawn native processes.** Same mechanics as `Up` step 8 with two
   differences:
   - The values map is rebuilt from the record — `{DB_NAME: rec.DBName}` ∪
     `{var: strconv.Itoa(port)}` over `rec.Ports` — not from the pool.
   - The process env is parsed from the worktree's **existing** `.env`,
     not re-derived. Suspend/resume never rewrite user-visible files;
     regenerating `.env` is `rederive`'s explicit job.
   ⚠ `.env` missing (blueprint declares a template) → fail:
     `env: .env not found at <path> — run 'plax rederive' to regenerate`.
   ⚠ Logs append to the same `.plax/logs/<name>/<proc>.log` files.
5. **Liveness sweep.** Same as `Up` step 9: settle 300ms, every container
   `ServiceRunning`, every process `IsAlive`. An immediate exit fails the
   resume with the same messages as `up`.
6. **Rollback on any failure in steps 3–5.** Stop the containers this call
   started (skip ones already running), terminate the processes this call
   spawned, clear any partial PIDs, leave `State = "suspended"`, save.
   The error is returned; resume stays retryable. Rollback uses
   `context.WithoutCancel(ctx)` for the same reason as `Up`.
 7. **Update registry.** `State → "running"`, fresh `PIDs` and `PIDStarts`.
   Save.
 8. **Return to the CLI.** `instance.Resume` does not import `status`. The
   CLI (in `cmd/plax`) calls `status.Build` after `Resume` returns, prints
   the report to stderr, then `instance <name> resumed`.
   ⚠ Resume is never silent (design §4). The report goes to stderr because
   resume has no stdout record; `plax status` owns the stdout form. If the
   report itself cannot be built (BM down, etc.) its dimensions degrade to
   `unknown` — Build does not fail on dimension errors.

### Status — `Build`

Load the record (not found → error, exit 1). Compute five dimensions
independently; each yields `ok`, `drift`, or `unknown` with a one-line
detail. State (running/suspended) does not gate any dimension.

**Code** — branch vs base ref:
1. Determine the base, in order:
   a. `rec.BaseRef` when set and still resolving (`worktree.RefExists`).
   b. `rec.BaseCommit` when set (detached HEAD at creation, or the ref was
      deleted since) → ahead-only against the recorded SHA:
      `ahead <A>, behind ? (base ref unavailable)`.
   c. The fallback chain `main`, `master`, `origin/HEAD` — for records
      that predate Phase 4 (both fields empty).
   d. None of the above → `unknown`, `no base ref recorded`.
2. `worktree.AheadBehind(repoRoot, base, rec.Branch)`.
3. `ahead 0, behind 0` → ok, `up to date with <base>`. Otherwise drift,
   `ahead <A>, behind <B>`.
   ⚠ Branch deleted externally → `unknown`, `branch plax/<name> not found`.

**Schema** — migration set the DB was built with vs the set on the base
ref now:
1. Instance side: `deps.BM.InstanceProvenance(ctx, rec.DBName)` →
   `SchemaHash`. Nil BM, missing DB, or missing provenance → `unknown`.
2. Repo side: `worktree.SchemaFilesAtRef(repoRoot, base, migrationsDir)`
   (same base resolution as Code) → `postgres.HashMigrationNames(names)`.
3. Equal (and non-empty) → ok, `migrations match <base>`. Differ → drift,
   `database was built from a different migration set than <base> declares
   — re-migrate the instance, or 'plax down' + 'plax up' to rebuild from a refreshed base`. Either side empty → `unknown`.
   ⚠ This is a hash comparison: it sees that the set changed, not which
     files, and it cannot see ad-hoc DDL applied without a migration file.
     Set-difference against an applied-migrations table is deferred (the
     table name is ORM-specific; see Deferred items).

**Data** — base version the instance was built from vs the base now:
1. `InstanceProvenance(ctx, rec.DBName).Version` vs
   `BaseStatus(ctx).ProvenanceVer`.
 2. Equal → ok, `built from base v<N> (current)`. Differ → drift,
   `built from base v<A> — base is now v<B> (stale; 'plax down' + 'plax
   up' to rebuild from the new base)`. Nil BM → `unknown (postgres
   unreachable)`; missing base → `unknown (base database missing)`;
   provenance row absent (`Version` 0) → `unknown (no provenance row in
   base — run 'plax base reset' to repair)`.
   ⚠ Read from the databases, not the registry: the provenance row exists
     precisely so the registry cannot lie about this (design §3).

**Host** — resolved toolchain at creation vs now:
1. `rec.Provenance.ToolVersions` nil → `unknown (recorded before Phase 4)`.
2. `toolchain.ParsePins(repoRoot/<bp.Toolchain>)` → `ResolveVersions` →
   `CompareVersions(recorded, current)`.
3. No diffs → ok, `toolchain unchanged (<tool>@<ver>, ...)`. Diffs →
   drift, one clause per tool: `nodejs v22.19.0 → v22.20.1, bun 1.3.11 →
   missing`.
   ⚠ Toolchain file absent → `unknown (no toolchain file)`.

**Config** — blueprint inputs then vs now (registry-global by design, §3:
what drifted is the repo, so the stamp lives in the registry, not per
instance):
 1. `deps.CurrentStamp` vs `deps.Registry.BlueprintStamp`, per file.
   (CurrentStamp is pre-computed by the CLI — same logic as the stamp
   notice — so `status` does not import `instance`.)
2. All equal → ok, `compose, env template, toolchain unchanged`.
   Differences → drift, one clause per file:
   `docker-compose.yml changed; .env.example changed`.
3. Stored stamp all-empty (registry predates stamping) → `unknown (no
   stamp recorded)`.
   ⚠ The stamp is re-stamped by every `up` — it tracks "the blueprint
     inputs as of the last `up`", which is the re-approval point.

### Doctor — `Run`

Four areas. Each check appends one `Check{Area, Level, Message}`; a failed
check's message names the remedy. Order is fixed: blueprint-vs-repo,
blueprint-vs-registry, repo-vs-machine, base.

**Area 1 — blueprint vs repo:**
1. `ValidateStructural` errors → one Fail each. (Further checks still run.)
2. `ValidateBlueprint` warnings → one Warn each (hole presence etc.).
3. Parse `docker-compose.yml` (goccy/go-yaml, same as `init`):
   - each blueprint service whose image no compose service declares → Warn
     `service <name> (<image>) not found in docker-compose.yml — blueprint
     may need a recheck`
   - each compose service whose image no blueprint service declares → Warn
     `compose service <name> (<image>) is not in the blueprint`
   - compose missing/unparseable → Warn (compose is an init input, not a
     runtime dependency; `up` does not read it)
4. A `shared` service with a writable volume → Fail. Forward-looking:
   `ServiceDef` has no volumes field yet, so this passes vacuously until
   volumes land.
5. Stamp: recompute vs registry per file → one Warn per changed file
   (`docker-compose.yml changed since the last 'plax up' — recheck the
   blueprint`). All-empty stored stamp → skip silently.

**Area 2 — blueprint vs registry:**
6. Port allocation whose instance is absent → Fail `port <N> allocated to
   unknown instance "<name>" — remove it from .plax/registry.json`.
7. Port allocation whose service/process is no longer declared → Warn
   `port <N> allocated to <inst>/<svc> but the blueprint declares no such
   service`.
8. Per instance record:
   - worktree path missing on disk → Fail `<name>: worktree missing —
     run 'plax down <name>' to clean up the record`
   - branch missing (`worktree.BranchExists`) → Fail, same remedy
   - database missing (BM non-nil, `InstanceDBExists` false) →
     Fail `<name>: database <db> missing — 'plax down' + 'plax up' to rebuild`
   - container gone (Docker non-nil, `ServiceExists` false) → Fail,
     same remedy — at any state: a suspended instance expects its
     containers stopped, not deleted
   - state `running` but a recorded PID is dead or fails the start-time
     identity check, or a recorded container exists but is stopped → Warn
     `<name>: <proc> is not running but state is "running" — crashed?
     'plax suspend' then 'plax resume' to restart`

**Area 3 — repo vs machine:**
9. Toolchain: `ParsePins`; per pin — binary not found → Fail `<tool>: pin
   <ver> but not installed`; resolved but not `MatchesPin` → Fail `<tool>:
   pinned <pin>, installed <resolved>`; non-semver pin → Warn `<tool>: pin
   "<pin>" is not a fixed version — cannot verify`. Missing toolchain file
   → skip with an ok-level note.
10. Docker daemon reachable (`NewDriver` already done by the CLI; nil →
    Fail `docker: cannot connect to daemon`).
11. Postgres reachable (nil BM → Fail `postgres: cannot connect`).

**Area 4 — base health** (skipped when BM is nil; check 11 already failed):
12. Base missing → Fail `plax_base does not exist — run 'plax base reset'`.
13. Base unlocked → Fail `plax_base is not locked — run 'plax base reset'
    to repair` (the flag can be cleared by hand, and the failure it causes
    surfaces as an intermittent `up` error blamed on the wrong thing, §3).
14. Base without a provenance row → Warn.
15. `plax_base_next` exists → Warn `staged plax_base_next exists — a
    refresh swap was deferred; run 'plax base refresh' to finish it or
    'plax base reset' to discard`.

### Rederive

Regenerate `.env` after the env template or holes change. Files only — no
backend is touched, and instances are not restarted (a note tells the user
to `suspend`/`resume` to apply).

1. Load blueprint and registry. Blueprint without `env.template` →
   `nothing to rederive: blueprint has no env template`. Exit 0.
2. Parse the user's `.env` at the repo root (may not exist → empty).
3. For each instance in the registry (sorted by name, deterministic
   output):
   a. `values` from the record: `{DB_NAME: rec.DBName}` ∪ recorded ports.
    b. Parse the instance's existing `.env` with `env.ParseFileRaw`
       (exact-key, verbatim-value — exported from the existing private
       function so secrets like `SECRET="a # b"` round-trip without
       corruption). Missing is not an error — the instance layer is
       simply empty and the file is regenerated from the template and
       user `.env` (this is what repairs a `.env` deleted while
       suspended; resume points here).
    c. Build merged overrides: non-hole keys of the instance `.env`, then
       non-hole keys of the user **parse** (also `ParseFileRaw`) on top.
       "Hole keys" means the **current** blueprint's hole set.
      ⚠ Precedence: user `.env` > instance `.env` > template. A secret the
        user deleted from their `.env` survives in the instance's — the
        instance keeps what it was born with.
      ⚠ A key removed from the hole set since creation is now an ordinary
        key and keeps its last rendered value — frozen, documented.
   d. `env.DeriveMerged(templatePath, merged, bp.Env.Holes, values, tmpPath)`
      — write to a temp file in the worktree, compare, and only replace
      `.env` when content differs (mtime stability for file watchers).
   e. Diff old vs new on change: one `- KEY=<old>` / `+ KEY=<new>` line
      per changed, added, or removed key, printed to stdout under an
      `<name>:` header. Values are shown verbatim — this is the user's own
      file on their own machine, same exposure as `cat .env`.
   f. Per-instance failure → Warn, continue with the rest; exit 1 at the
      end if any failed.
4. Stderr summary: `rederived <k> of <n> instance(s)`; when any changed,
   `note: restart instances to apply ('plax suspend <name>' && 'plax
   resume <name>')`.

### Tolerant blueprint load

Commands that previously only opened the registry — `ls`, `attach`, `exec`
— now need the blueprint for the stamp notice (and `attach` for the drift
hint). The blueprint is loaded *tentatively*: when `plax.json` is missing or
unparseable, the command proceeds without the notice/hint, never failing:

```go
func loadBlueprint(repoRoot string) (*blueprint.Blueprint, error)
```

The CLI wraps this so the notice helper accepts a nil blueprint and skips
silently. All three commands call the notice **and** `attach` calls
`status.Build` for the drift hint — both degrade cleanly when the blueprint
is unavailable.

### Config stamp notice

A helper in `cmd/plax/main.go`, run by every registry command except
`init`, `doctor`, and the `base` namespace (those either have no registry
or no blueprint, or print the details themselves):

```go
func computeStamp(repoRoot string, bp *blueprint.Blueprint) registry.BlueprintStamp
func stampNotice(stamp registry.BlueprintStamp, reg *registry.Registry)
```

`computeStamp` (formerly `instance.ComputeBlueprintStamp`, now in the CLI)
recomputes the stamp. When the stored stamp is non-empty and any file's
hash differs, `stampNotice` prints one line to stderr:
`note: blueprint inputs changed since last 'plax up' — run 'plax doctor'
for details`.

⚠ `up` prints the notice too, before proceeding — and then re-stamps, as
  it has since Phase 3. The stamp is "last approved inputs"; `up` is the
  approval point.
⚠ Three file reads and three hashes per command — cheap enough to run
  unconditionally (design §3).

### Attach/exec additions

- On a suspended instance, both print to stderr before doing anything:
  `note: instance <name> is suspended — services and processes are
  stopped`. They still run: the worktree, `.env`, and database are all
  usable, and poking at a sleeping instance is legitimate.
- `attach` additionally prints a drift hint (delivers the Phase 3 deferred
  item): `status.Build` with a nil BM — Postgres is never opened on this
  path, so Data and Schema come back `unknown` and only real `drift` rows
  (code, host, config) feed the hint. When any shows drift:
  `note: drift detected (<dimensions>) — run 'plax status <name>'`.
  ⚠ Best-effort: failures degrade to `unknown` rows, never to a failed
    attach. Latency is bounded — a few git calls plus one ≤2s exec per
    pinned tool — and adds nothing when the toolchain file is absent.

---

## CLI specification

All commands accept `--root` (default `.`), consistent with Phases 1–3.

### `plax suspend <name>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional, default `.`) |
| Exit 0 | Suspended, or already suspended (idempotent) |
| Exit 1 | Instance not found; registry save failed |
| Stderr | Progress, warnings, `instance <name> suspended` |
| Stdout | Nothing |

No `--pg-url`: suspend touches no database.

### `plax resume <name>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional), `--pg-url <dsn>` (optional; drift report only) |
| Exit 0 | Resumed; drift report printed to stderr |
| Exit 1 | Not found; already running; port occupied; container missing; Docker unavailable with containers recorded; spawn/liveness failure (rolled back to suspended) |
| Stderr | Progress, then the drift report, then `instance <name> resumed` |
| Stdout | Nothing |

### `plax status <name>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional), `--pg-url <dsn>` (optional), `--json` |
| Exit 0 | Always, when the instance exists — drift and unknown are information, not failure |
| Exit 1 | Instance not found |
| Stdout | Table (default) or JSON |
| Stderr | Nothing |

**Table output** — one record per line, stable fields (design §5: awk and
friends must work on it):

```
NAME  DIMENSION  STATUS   DETAIL
i1    code       ok       up to date with main
i1    schema     drift    database was built from a different migration set than main declares
i1    data       drift    built from base v3 — base is now v5 (stale)
i1    host       ok       toolchain unchanged (nodejs@v22.19.0, bun@1.3.11)
i1    config     ok       compose, env template, toolchain unchanged
```

**JSON output:** the `status.Report` struct, dimensions keyed by name.

### `plax doctor`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional), `--pg-url <dsn>` (optional), `--json` |
| Exit 0 | No check failed (warnings allowed) |
| Exit 1 | Any check failed |
| Stdout | Check lines (default) or JSON |
| Stderr | Nothing |

**Default output** — grouped by area, one line per check:

```
blueprint vs repo:
  [ok]   blueprint parses and validates
  [warn] docker-compose.yml changed since the last 'plax up' — recheck the blueprint
blueprint vs registry:
  [ok]   2 instances, all resources present
repo vs machine:
  [ok]   nodejs v22.19.0 (pinned 22.19.0)
  [fail] bun: pinned 1.3.11, installed 1.4.0
base:
  [ok]   plax_base v5, locked
```

### `plax rederive`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional) |
| Exit 0 | All instances rederived (or nothing to do) |
| Exit 1 | Any per-instance failure (others still processed) |
| Stdout | Per-instance key diffs for changed files |
| Stderr | Summary, restart note |

No `--all` (see Changes from the previous sketch). No `--pg-url`: files
only.

### Modified commands

- `plax ls` — unchanged; `STATE` column now also shows `suspended`.
- `plax attach` / `plax exec` — new stderr notes on suspended instances;
  `attach` may print the drift hint. Exit codes and stdio unchanged.

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| `suspend`/`resume`/`status` on unknown instance | `GetInstance` false | `instance "<name>" not found`. Exit 1. |
| `suspend` on suspended instance | `rec.State` | Note, exit 0 (idempotent). |
| `resume` on running instance | `rec.State` | `instance "<name>" is already running`. Exit 1. |
| Port occupied at resume | `ProbeFree` false | `port <N> (<VAR>) is in use by pid <pid> (<cmdline>) — ...` → exit 1, no side effects (probe precedes them). Owner unknown → same without pid. |
| TOCTOU: port grabbed after probe | Docker `start` fails / process bind dies in sweep | Surface the underlying error; rollback to suspended. The probe is advisory, not a guarantee. |
| Container deleted while suspended | `StartService` → no such container | `container for "<svc>" no longer exists — run 'plax down' then 'plax up' to rebuild`. Other started containers stopped; state stays suspended. Exit 1. |
| Docker down at resume, containers recorded | `deps.Docker == nil` | `docker unavailable — cannot start <N> container(s)`. No side effects. Exit 1. |
| `.env` deleted while suspended | `ParseFile` → `ErrNotExist` | `env: .env not found — run 'plax rederive' to regenerate`. Rollback to suspended. Exit 1. |
| Process dies on resume | Liveness sweep | Same message as `up` (`process "<name>" exited immediately after start — see .plax/logs/...`). Rollback to suspended. Exit 1. |
| SIGINT during resume | `signal.NotifyContext` | In-flight step aborts; rollback runs with a non-canceled context. Exit 1. |
| Postgres down at resume | BM construction fails (tolerant) | Resume proceeds; drift report's Data/Schema rows show `unknown (postgres unreachable)`. Exit 0. |
| Postgres down at `status` | nil BM | Data, Schema → `unknown`. Exit 0. |
| Phase 3 record (no BaseRef/ToolVersions) | nil/empty fields | Code falls back to `main`/`master`/`origin/HEAD`; Host → `unknown (recorded before Phase 4)`. Exit 0. |
| Base ref deleted | `RefExists` false, `BaseCommit` set | Code → drift/ok with `behind ?` — ahead-only against the recorded SHA. |
| `git` failure in status | non-zero exit | That dimension → `unknown (<err>)`. Exit 0. |
| `rederive` with no env template | `bp.Env.Template == ""` | `nothing to rederive`. Exit 0. |
| `rederive` instance `.env` missing | `ErrNotExist` | Not an error — the instance layer is empty and the file is regenerated from template + user `.env`. |
| `rederive` template missing | `os.ReadFile` → `ErrNotExist` | `env: template not found at <path>`. No writes. Exit 1. |
| `rederive` unknown `{{VAR}}` | `Render` | `env: template references unknown variable {{VAR}}` → a hole references a var the record cannot supply (e.g. a port var added to the blueprint after creation). Skip instance. Exit 1. |
| `doctor` with broken blueprint JSON | `json.Unmarshal` | CLI fails before `Run`: `parsing plax.json: ...`. Exit 1. |
| Registry save failure (suspend/resume) | `Save` error | `registry: write: <err>`. Exit 1. Side effects match the pre-command state as closely as rollback could manage; re-running the command is safe. |

---

## Tests

### Test prerequisites

Same tiers as Phase 3: `toolchain`, `portpool` probe, `status` (with
fakes), `doctor` (with fakes), and the `instance` suspend/resume/rederive
tests need nothing beyond git; `toolchain` resolver tests use fake
binaries on a temp PATH. The extended e2e requires git + Postgres +
Docker + python3 and holds the `plax_base` advisory lock
(`pkg/testutil.LockPostgres`).

### Unit tests

**`pkg/toolchain/toolchain_test.go`:**

- `TestToolchain_ParsePins_Basic` — `nodejs 22.19.0` → `{"nodejs": "22.19.0"}`
- `TestToolchain_ParsePins_CommentsAndBlanks` — `#` lines and blank lines skipped
- `TestToolchain_ParsePins_MultiVersion` — `python 3.12.1 3.11.7` → first wins
- `TestToolchain_ParsePins_Missing` — absent file → nil, nil
- `TestToolchain_ResolveVersions_FakeBinaries` — temp dir of stub `node`/`bun` scripts on PATH → resolved strings
- `TestToolchain_ResolveVersions_MissingBinary` — absent from result
- `TestToolchain_CompareVersions` — changed/added/removed/unchanged cases
- `TestToolchain_MatchesPin` — pin `22.19.0` vs `v22.19.0` → true; pin `1.26` vs `go version go1.26.5 linux/amd64` → true (dot boundary); pin `1.2` vs `11.2.3` → false; outright mismatch → false; `lts` → false

**`pkg/portpool/probe_test.go`:**

- `TestProbeFree_Bound` — in-process `net.Listen` → false; after close → true
- `TestPortOwner_FindsSelf` — listener owned by the test process → `os.Getpid()` (Linux; skip elsewhere)
- `TestPortOwner_NoListener` — ok=false

**`pkg/worktree/git_test.go`** (temp git repos):

- `TestHeadRef_Branch` — on `main` → `("main", sha)`; detached → `("", sha)`
- `TestRefExists` — branch, garbage ref
- `TestAheadBehind` — branch 2 ahead, main 1 ahead after merge-back scenario → `(2, 1)`; up-to-date → `(0, 0)`
- `TestSchemaFilesAtRef` — files listed without touching the checkout; nested dir excluded; missing dir → empty; hash parity with `HashMigrationNames` over the same files on disk

**`pkg/status/status_test.go`** (real temp git repo; fake BM):

- `TestStatus_AllOK` — fresh instance → five `ok` rows
- `TestStatus_CodeDrift` — commit on base after creation → `behind 1`
- `TestStatus_CodeBaseRefFallback` — record without BaseRef compares against `main`
- `TestStatus_DataDrift` — fake BM reports base v2, instance v1 → drift with versions in detail
- `TestStatus_DataUnknown_NoBM` — nil BM → `unknown`, no error, Build succeeds
- `TestStatus_HostDrift` — fake resolver change → drift names the tool
- `TestStatus_HostUnknown_Phase3Record` — nil ToolVersions → `unknown (recorded before Phase 4)`
- `TestStatus_SchemaDrift` — migration file added on base ref → drift
- `TestStatus_ConfigDrift` — touch `.env.example` after stamp → drift names the file
- `TestStatus_NotFound` — error
- `TestStatus_SuspendedInstance` — report builds fully for suspended records

**`pkg/instance/suspend_test.go` / `resume_test.go`** (extend Phase 3 fakes; real git):

- `TestSuspend_Success` — process dead, container stopped, state suspended, PIDs cleared, ports kept, DB untouched (fake BM sees no drops)
- `TestSuspend_AlreadySuspended` — idempotent, no signals sent
- `TestSuspend_NotFound` — error
- `TestSuspend_NilDocker` — containers skipped with warning, still suspends
- `TestResume_Success` — containers started, processes respawned with recorded ports, state running, fresh PIDs, `.env` not rewritten (byte-identical)
- `TestResume_NotSuspended` — running instance → error, no side effects
- `TestResume_PortTaken` — test binds the recorded port → error names pid; state stays suspended; nothing started
- `TestResume_MissingContainer` — fake Docker reports gone → clear error, rollback, still suspended
- `TestResume_ProcessDiesImmediately` — `exit 1` command → rollback to suspended; originally-running container (started pre-resume) is not stopped by rollback
- `TestResume_NilDockerWithContainers` — error before any side effect
- `TestUp_RecordsBaseRefAndToolVersions` — new provenance fields populated

**`pkg/instance/rederive_test.go`:**

- `TestRederive_TemplateChange` — new key in template lands in instance `.env`; diff printed
- `TestRederive_PreservesInstanceSecrets` — key deleted from user `.env` keeps the instance's born-with value
- `TestRederive_UserEnvWins` — user `.env` value beats the instance's for a non-hole key
- `TestRederive_HoleReRender` — hole values still render from recorded ports/DB
- `TestRederive_NoChange_NoWrite` — identical output leaves mtime alone, prints no diff
- `TestRederive_NoTemplate` — clean no-op
- `TestRederive_MissingInstanceEnv_Regenerated` — deleted `.env` rebuilt from template + user `.env`

**`pkg/registry/registry_test.go`** (addition):

- `TestOpen_Phase3RegistryRecord` — a JSON literal written by the Phase 3
  schema (no `base_ref`, `base_commit`, `tool_versions`) opens cleanly,
  keeps State/PIDs/Ports intact, and saves without inventing the new keys

**`pkg/doctor/doctor_test.go`** (fixture dirs; fake BM/Docker):

- `TestDoctor_AllPass` — healthy fixture → exit-clean report, no fails
- `TestDoctor_ComposeDrift` — compose edited after stamp → Warn names the file
- `TestDoctor_ComposeServiceNotInBlueprint` — extra compose service → Warn
- `TestDoctor_DanglingWorktree` — record whose worktree was deleted → Fail with `plax down` remedy
- `TestDoctor_PortForUnknownInstance` — hand-seeded allocation → Fail
- `TestDoctor_ToolchainMismatch` — fake `node` resolves wrong version → Fail; missing binary → Fail
- `TestDoctor_BaseUnlocked` / `TestDoctor_BaseMissing` / `TestDoctor_BaseNextStaged` — Fail / Fail / Warn with remedies
- `TestDoctor_RunningButPIDDead` — stale record → Warn suggesting suspend/resume
- `TestDoctor_NilBackends` — reachability fails, dependent checks skipped not duplicated

**`pkg/derive/postgres`** (integration, `PLAX_TEST_POSTGRES_URL`):

- `TestInstanceProvenance_RoundTrip` — clone inherits base version; source rewritten
- `TestInstanceProvenance_MissingDB` — nil, nil
- `TestHashMigrationNames_Parity` — same list from disk and git hashes identically

### End-to-end tests (`cmd/plax/e2e_test.go` additions)

`TestEndToEnd_SuspendResume`:
- `up i1` → curl 200 → `suspend` → curl fails (connection refused), the
  redis container is stopped but not removed, and `ls` shows `suspended`
- while suspended: `exec i1 -- printenv PORT` still works and prints the
  suspended note; `attach` (stdin `exit`) prints the note and opens a
  shell that exits cleanly
- bind the recorded `PORT` with a test listener → `resume` fails, names
  the test's pid, exit 1 → release → `resume` succeeds → curl 200 on the
  **same** port → stderr carries the drift report → `down`

`TestEndToEnd_DriftAndDoctor`:
- `up i1` → commit on the fixture's base branch → `status i1` shows code
  `behind 1`; `--json` parses and matches
- `base refresh` → `status i1` data row shows `v1 → v2` stale
- `attach` (stdin `exit`) prints the drift hint naming the drifted
  dimensions
- edit `docker-compose.yml` → `ls` prints the stamp notice → `doctor`
  warns naming the file → exit 0 (warn-only)
- hand-seed a dangling port allocation (unknown instance) into the
  registry → `doctor` exits 1 naming the port → remove it
- append a key to `.env.example` → `rederive` prints the `+ KEY` diff and
  the instance `.env` gains it; byte-stability on a second run
- `suspend i1` → `exec` prints the suspended note → `resume i1` prints
  the report → `down`

⚠ New fixtures init with `git init -b main` so the code-drift assertions
do not depend on the host's `init.defaultBranch`; existing fixtures keep
working because `up` records whatever base ref HEAD is on.

Both hold the advisory lock and skip under the same gates as the Phase 3
e2e.

---

## Acceptance criteria

Verified by the named tests, the e2e scenarios, or package unit tests as
noted.

- [ ] `plax suspend i1` stops processes and containers; `ls` shows `suspended`; ports remain allocated in the registry; the database still answers queries *(TestSuspend_Success; e2e: DB check while suspended)*
- [ ] `plax suspend i1` twice is a no-op the second time, exit 0 *(TestSuspend_AlreadySuspended)*
- [ ] `plax resume i1` restarts everything on the same ports and prints the drift report to stderr *(e2e TestEndToEnd_SuspendResume)*
- [ ] `plax resume i1` with its port bound by another process fails, names the pid and command, and changes nothing *(TestResume_PortTaken; e2e)*
- [ ] `plax resume` never reallocates a different port for a taken one *(design §4; asserted by the port-taken tests)*
- [ ] A failed resume (dead container, dying process) leaves the instance suspended and retryable *(TestResume_MissingContainer, TestResume_ProcessDiesImmediately)*
- [ ] `plax status i1` prints five parseable rows; each dimension independently reaches `ok`, `drift`, or `unknown` *(status unit tests)*
- [ ] Committing on the base branch after `up` makes `status` report `behind 1` *(e2e)*
- [ ] `plax base refresh` makes `status` report the instance's base version as stale *(e2e)*
- [ ] Editing `docker-compose.yml` makes `status` report config drift and makes every other registry command print the one-line notice to stderr *(e2e; TestStatus_ConfigDrift)*
- [ ] `status` with Postgres down still prints the report with `unknown` Data/Schema rows and exits 0 *(TestStatus_DataUnknown_NoBM)*
- [ ] Instances created before Phase 4 report Code via the main/master fallback and Host as `unknown (recorded before Phase 4)` *(TestStatus_CodeBaseRefFallback, TestStatus_HostUnknown_Phase3Record)*
- [ ] `plax doctor` on a healthy repo exits 0; with a failed check exits 1 and names the remedy *(doctor unit tests; e2e)*
- [ ] `plax doctor` catches: compose/blueprint mismatch, dangling worktree or branch, port allocated to an unknown instance, toolchain mismatch, unlocked base, staged `plax_base_next` *(doctor unit tests)*
- [ ] `plax rederive` after editing `.env.example` updates every instance's `.env`, prints a key-level diff, and preserves secrets that no longer exist in the user's `.env` *(rederive unit tests; e2e)*
- [ ] `plax rederive` with no changes writes nothing and prints no diff *(TestRederive_NoChange_NoWrite)*
- [ ] `plax attach i1` on a suspended instance prints the suspended note; on a drifted instance prints the drift hint; neither blocks the shell *(e2e)*
- [ ] `plax exec i1` works on a suspended instance (with the note) *(e2e)*
- [ ] `up` records `BaseRef`, `BaseCommit`, and resolved `ToolVersions` in the registry *(TestUp_RecordsBaseRefAndToolVersions)*
- [ ] Registries written by Phase 3 load unchanged; no migration step *(TestOpen_Phase3RegistryRecord — additive omitempty fields)*
- [ ] `gofmt -s`, `go vet ./...`, `go test -race -count=1 ./...` (with and without `PLAX_TEST_POSTGRES_URL`), and `golangci-lint run` all pass

---

## Dependencies

No new external modules. Everything builds on the standard library and the
modules already in `go.mod`:

| Need | Import | Already used by |
|---|---|---|
| Port probe | `net` | `portpool` (Phase 1) |
| Port owner | `os`, `strings`, `strconv` over `/proc` | `process` (Phase 3, same mechanism) |
| Git queries | `os/exec` | `worktree` (Phase 3) |
| Compose parsing (doctor) | `github.com/goccy/go-yaml` v1.19.2 | `init` (Phase 1) |
| Tool version exec | `os/exec`, `context` (2s timeout) | — |
| Hashing | `crypto/sha256` | `instance`, `postgres` (Phase 2/3) |

---

## Concurrency note

Unchanged from Phase 3: the registry has no file locking, and
suspend/resume share the single-user, one-command-at-a-time assumption.
`up` during a `base refresh` remains safe by the staged-swap design;
`resume` racing a port grab is the documented TOCTOU case — the probe
narrows the window, Docker and the bind error close it.

---

## Deferred items

| Item | Deferred to | Reason |
|---|---|---|
| Schema set-difference via applied-migrations table | When the blueprint declares `seed.migrations_table` | Table names are ORM-specific (`schema_migrations`, `_prisma_migrations`, ...); hash comparison covers "changed" without the config surface |
| `resume --repair` (recreate missing containers in place) | When container loss proves common | `down`+`up` is the honest repair today; data lives in the DB and named volumes, not the container |
| `rederive --dry-run` | When someone asks | The diff-after-write already shows what changed; instances keep old values until restarted |
| `mise.toml` toolchain parsing | When a repo uses it | `.tool-versions` covers the sample; the format branch lives behind `ParsePins` |
| `PortOwner` on macOS | When macOS support matters | Same `/proc` boundary as the PID identity guard |
| Readiness checks beyond the liveness sweep | When a workload needs one | The sweep catches immediate exits; "ready" is app-specific |
| Registry file locking (`flock`) | When concurrent use is real | Single-user tool; noted since Phase 3 |
| Per-instance config stamps | When global proves confusing | The design puts the stamp in the registry on purpose (§3) — what drifted is the repo, not the instance |
| `index.md` and `design.md` reconciliation | When the implementation's shape stabilizes | This plan changes Schema/Host drift methods, drops `--all` from rederive, and scopes `refresh` to Phase 2 — the design doc and index still document the old sketch |
