# Phase 3 — Instance Lifecycle

## Objective

Wire the blueprint, registry, port allocator, Postgres driver, and Docker
driver into CLI commands that create, list, and destroy running instances —
the first phase where a user can run `plax up i1` and get a working
environment.

---

## Package layout

```
cmd/plax/main.go              # Add up, down, ls, attach, exec to kong CLI
pkg/
  worktree/
    worktree.go               # Git branch + worktree create/remove
    worktree_test.go          # Tests against temp git repos
  derive/
    env/
      derive.go               # .env derivation: hole substitution, template rendering
      derive_test.go          # Unit tests for derivation and rendering
  process/
    supervisor.go             # Spawn, signal, terminate native process groups
    supervisor_test.go        # Tests with real subprocesses
  instance/
    instance.go               # Deps struct, Up, Down orchestration with rollback
    up.go                     # Up algorithm steps (split for readability)
    down.go                   # Down algorithm steps
    instance_test.go          # Unit tests with fake BM/Docker (require git only)
```

End-to-end coverage lives in `cmd/plax/e2e_test.go` (requires git + Postgres +
Docker; skipped otherwise).

---

## Type specifications

### `pkg/worktree/worktree.go`

No exported types. Functions operate on `repoRoot` and `name` strings.

```go
package worktree

// BranchName returns the git branch name for an instance.
func BranchName(name string) string // "plax/<name>"

// WorktreeRelPath returns the worktree path relative to repo root.
func WorktreeRelPath(name string) string // ".plax/worktrees/<name>"

// BranchExists reports whether the instance branch exists.
func BranchExists(repoRoot, name string) bool

// Create creates a git branch (plax/<name>) at current HEAD and adds a
// worktree at .plax/worktrees/<name>. Returns the absolute worktree path.
// Fails if the branch already exists.
func Create(repoRoot, name string) (string, error)

// Remove removes the worktree and force-deletes the branch.
// Plax owns the branch — unmerged commits are the user's responsibility
// to push before running down. The branch is deleted even when the worktree
// itself is already gone (a stranded branch would block the next create).
func Remove(repoRoot, name string) error
```

### `pkg/derive/env/derive.go`

No exported types. Functions operate on maps and file paths.

```go
package env

// Derive reads the env template file, substitutes holes with rendered
// values, and writes the result to outputPath.
//
// templatePath: absolute path to the env template (e.g. .env.example).
// overridesPath: absolute path to the user's own .env file (may not exist).
//   Non-hole values from this file take precedence over the template.
// holes: KEY → template string with {{VAR}} placeholders (from blueprint).
// values: VAR → resolved value (e.g. "DB_NAME" → "plax_i1", "REDIS_PORT" → "6380").
// outputPath: absolute path where the derived .env is written.
//
// Precedence for each key:
//  1. Hole keys → rendered template (per-instance values)
//  2. Keys in overrides file → user's value (secrets, machine-specific config)
//  3. Template lines → copied verbatim (defaults, comments)
//
// Hole keys absent from the template are appended.
func Derive(templatePath string, overridesPath string, holes map[string]string, values map[string]string, outputPath string) error

// ParseFile reads a .env file and returns key-value pairs.
// Skips blank lines and comments (#). Strips surrounding quotes from values.
func ParseFile(path string) (map[string]string, error)

// Render replaces {{VAR}} placeholders in tmpl with values.
// Returns error if a placeholder references a variable not in values.
func Render(tmpl string, values map[string]string) (string, error)
```

### `pkg/process/supervisor.go`

Functions operate on PIDs and process group IDs. `ErrStaleProcess` is the
one exported sentinel.

```go
package process

// Spawn starts command as a detached process group leader.
// The command runs in dir with the given environment (merged over os.Environ).
// Stdout and stderr are appended to logPath.
// Returns the process group ID (equals the leader's PID) and the leader's
// start time (clock ticks since boot, from /proc) for identity checks.
// startTime is 0 on platforms without /proc (e.g. macOS).
//
// The process is NOT waited on — it outlives the plax command that spawned it.
func Spawn(name, command string, env []string, dir string, logPath string) (pgid int, startTime int64, err error)

// StartTime returns the process's start time from /proc/<pid>/stat, or 0
// when the process is gone or the platform has no /proc.
func StartTime(pid int) int64

// Terminate sends SIGTERM to the process group, waits for the timeout,
// then sends SIGKILL. No-op if the process group is already dead.
//
// When startTime is nonzero, the group is only signaled if its leader still
// has that start time — a reused PGID returns ErrStaleProcess and is never
// signaled. Pass 0 to skip identity verification.
func Terminate(pgid int, startTime int64, timeout time.Duration) error

// IsAlive reports whether a process group exists.
func IsAlive(pgid int) bool
```

⚠ Platform limitation: PID-reuse protection requires `/proc` (Linux). On
macOS, start times are 0 and Terminate falls back to PGID-only behavior.

### `pkg/instance/instance.go`

```go
package instance

import (
    "github.com/apollopower/plax/pkg/blueprint"
    "github.com/apollopower/plax/pkg/registry"
    "github.com/apollopower/plax/pkg/portpool"
    "github.com/apollopower/plax/pkg/derive/postgres"
    "github.com/apollopower/plax/pkg/derive/docker"
)

// BaseManager is the subset of postgres.BaseManager that lifecycle
// orchestration needs. An interface so tests can fake it.
type BaseManager interface {
    BaseStatus(ctx context.Context) (postgres.BaseInfo, error)
    CloneBase(ctx context.Context, targetDB string) error
    DropInstanceDB(ctx context.Context, dbName string) error
}

// DockerDriver is the subset of docker.Driver that lifecycle orchestration
// needs. An interface so tests can fake it.
type DockerDriver interface {
    CreateNetwork(ctx context.Context, name string) error
    RemoveNetwork(ctx context.Context, name string) error
    RunService(ctx context.Context, cfg docker.ServiceConfig) (string, error)
    StopService(ctx context.Context, containerID string) error
    RemoveService(ctx context.Context, containerID string) error
    ServiceRunning(ctx context.Context, containerID string) (bool, error)
}

// Deps holds the dependencies for instance lifecycle operations.
// Assembled by the CLI layer and passed to Up/Down.
//
//   Up:    all fields required
//   Down:  Blueprint and Pool unused; BM and Docker may be nil — Down skips
//          that backend's resources with a warning and continues teardown
//   ls:    Registry, RepoRoot
//   attach/exec: Registry, RepoRoot
type Deps struct {
    Blueprint *blueprint.Blueprint
    Registry  *registry.Registry
    Pool      *portpool.PortPool
    BM        BaseManager
    Docker    DockerDriver
    RepoRoot  string // absolute path to the repo root
}
```

**Public functions:**

| Function | Signature | Behavior |
|---|---|---|
| `Up` | `func Up(ctx context.Context, deps *Deps, name string) error` | Creates a full instance: branch, worktree, network, ports, .env, database, containers, processes, registry entry. Rolls back all side effects on failure. |
| `Down` | `func Down(ctx context.Context, deps *Deps, name string) error` | Destroys an instance: kills processes, removes containers, drops database, removes network, releases ports, removes worktree, deletes branch, removes registry entry. |

### Instance name validation

Instance names must match `^[a-z][a-z0-9_]*$` and be at most 32 characters.
This constraint exists because the name is embedded in:

- Git branch names (`plax/<name>`) — no spaces, no special chars
- Docker container/network names (`plax-<name>-redis`) — DNS-safe
- Postgres database names (`plax_<name>`) — used as an unquoted SQL
  identifier, which is why hyphens are **not** allowed even though git and
  Docker would accept them
- Filesystem paths (`.plax/worktrees/<name>`) — no slashes

### Valid `InstanceRecord.State` values in Phase 3

| State | Meaning |
|---|---|
| `"running"` | All processes and containers are up |
| `"suspended"` | Reserved for Phase 4. Not set by Phase 3 code. |

---

## Algorithms

All algorithms assume the caller holds a valid `*Deps` constructed by the CLI
layer. Context cancellation is respected throughout.

### Rollback pattern

`Up` uses a cleanup stack. Each step that produces a side effect appends a
cleanup function. On failure, cleanups run in reverse order (LIFO):

```go
var cleanups []func()
success := false
defer func() {
    if !success {
        for i := len(cleanups) - 1; i >= 0; i-- {
            cleanups[i]() // errors logged to stderr, not returned
        }
    }
}()

// ... each step appends its cleanup ...

success = true
return nil
```

Cleanup errors are logged to stderr but never returned — the original error
is what the user needs to see.

Cleanups run with `context.WithoutCancel(ctx)`: when context cancellation
(e.g. Ctrl-C via `signal.NotifyContext` in the CLI) is what caused the
failure, Docker and Postgres cleanup calls must still complete.

Container and process cleanups are registered **before** their start loops —
closures capture the ID maps, so a mid-loop failure still rolls back every
workload started so far.

### Up

1. **Validate instance name and blueprint structure.**
   Name must match `^[a-z][a-z0-9_]*$`, max 32 chars.
   ⚠ Return error: `invalid instance name "<name>": must match ^[a-z][a-z0-9_]*$ (max 32 chars)`

   Then `blueprint.ValidateStructural(deps.Blueprint)` must return no errors.
   This catches duplicate process names (which would orphan the first
   process's PID in the `pids` map), port-var collisions, and service names
   that collide after Docker name sanitization (`foo_bar` vs `foo-bar`)
   before any side effect. Hole-presence warnings from `ValidateBlueprint`
   are deliberately not fatal — derivation appends missing holes.

2. **Check instance does not exist.**
   - `deps.Registry.GetInstance(name)` → must return false.
     ⚠ If found, return error: `instance "<name>" already exists`
   - `worktree.BranchExists(deps.RepoRoot, name)` → must return false.
     ⚠ If branch exists but registry has no record, a previous `up` failed
       partway. Return error: `branch "plax/<name>" exists but instance is not
       registered — run 'git branch -D plax/<name>' to clean up`

3. **Check base database.**
   - `deps.BM.BaseStatus(ctx)` → must return `Exists=true, Locked=true`.
     ⚠ If absent: `base database does not exist — run 'plax base reset' first`
     ⚠ If unlocked: `base database is not locked — run 'plax base reset' to repair`

4. **Create branch.**
   `git branch plax/<name>` at current HEAD.
   Cleanup: `git branch -D plax/<name>`

5. **Create worktree.**
   `git worktree add .plax/worktrees/<name> plax/<name>`
   ⚠ Creates the `.plax/worktrees/` directory if absent.
   Cleanup: `git worktree remove .plax/worktrees/<name>`

6. **Create Docker network.**
   `deps.Docker.CreateNetwork(ctx, "plax-"+name+"-net")`
   ⚠ Idempotent — safe if network already exists from a partial previous run.
   Cleanup: `deps.Docker.RemoveNetwork(ctx, "plax-"+name+"-net")`

7. **Allocate ports.**
   Iterate the blueprint and allocate one port per port-bearing entity.
   `allocated` maps port var name → host port number.

   For each `dedicated` service with a `ports` map — one allocation per
   entry (the container port key is used later in step 13 to build the
   Docker port binding; here we only need the var name):
   ```
   for _, portDef := range svc.Ports {
       port := deps.Pool.Allocate(name, svcName)
       allocated[portDef.Var] = port
   }
   ```

   For each `native` process with a `port_var`:
   ```
   port := deps.Pool.Allocate(name, procName)
   allocated[proc.PortVar] = port
   ```

   `logical` services are skipped — they share the host's existing port.
   `shared` and `external` services are skipped — they declare no ports.

   Build the values map used for .env derivation and command templating:
   ```go
   values := map[string]string{"DB_NAME": "plax_" + name}
   for varName, port := range allocated {
       values[varName] = strconv.Itoa(port)
   }
   ```

   Cleanup: release all allocated ports via `deps.Pool.Release(port)`.

8. **Derive .env.** Skipped when `deps.Blueprint.Env.Template` is empty —
   a blueprint with no env template has nothing to derive.
   ```
   env.Derive(
       filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template),
       filepath.Join(deps.RepoRoot, ".env"),  // user overrides; may not exist
       deps.Blueprint.Env.Holes,
       values,
       filepath.Join(worktreePath, ".env"),
   )
   ```
   No separate cleanup — removing the worktree (step 5 cleanup) removes .env.

9. **Clone database.**
   `deps.BM.CloneBase(ctx, "plax_"+name)`
   ⚠ Postgres serializes TEMPLATE clones — concurrent `up` calls queue.
   Cleanup: `deps.BM.DropInstanceDB(ctx, "plax_"+name)`

10. **Read base provenance.**
    `deps.BM.BaseStatus(ctx)` → `info.ProvenanceVer`
    Stored in `InstanceRecord.Provenance.BaseVersion`.
    No side effects, no cleanup needed.

11. **Compute toolchain hash.**
    SHA-256 of the file at `filepath.Join(deps.RepoRoot, deps.Blueprint.Toolchain)`.
    Stored in `InstanceRecord.Provenance.Toolchain`.
    ⚠ If the toolchain file does not exist, store empty string. Not an error.
    No side effects, no cleanup needed.

12. **Compute blueprint stamp.**
    SHA-256 of each file relative to `deps.RepoRoot`:
    - `docker-compose.yml` → `BlueprintStamp.ComposeHash`
    - The env template file (`deps.Blueprint.Env.Template`, typically `.env.example`) → `BlueprintStamp.EnvExampleHash`
    - The toolchain file (`deps.Blueprint.Toolchain`, typically `.tool-versions`) → `BlueprintStamp.ToolchainHash`
    Store in `deps.Registry.BlueprintStamp`.
    ⚠ If a file is absent, its hash is empty string. Not an error.
    ⚠ The compose filename is hardcoded as `docker-compose.yml` — the same
      convention `plax init` uses to find it. Not a blueprint field.
    No cleanup — the stamp is part of the registry save in step 15.

13. **Start dedicated containers.**
    For each service with `isolation: "dedicated"`:

    Build the port map — for each entry in the service's `ports` map, the
    container port (the map key) maps to the allocated host port:
    ```go
    portMap := map[string]int{}
    for containerPort, portDef := range svc.Ports {
        portMap[containerPort] = allocated[portDef.Var]
    }
    ```

    Build the service env — merge the service's `env` block with the
    allocated port variables:
    ```go
    svcEnv := map[string]string{}
    for k, v := range svc.Env {
        svcEnv[k] = v
    }
    for _, portDef := range svc.Ports {
        svcEnv[portDef.Var] = strconv.Itoa(allocated[portDef.Var])
    }
    ```

    ```go
    cfg := docker.ServiceConfig{
        InstanceName: name,
        ServiceName:  svcName,
        Image:        svc.Image,
        Command:      svc.Command,
        Env:          svcEnv,
        PortMap:      portMap,
        NetworkName:  "plax-" + name + "-net",
    }
    containerID := deps.Docker.RunService(ctx, cfg)
    containerIDs[svcName] = containerID
    ```
    ⚠ On failure, stop and remove all containers started so far in this loop.
    Cleanup: stop + remove all containers, remove their volumes.

14. **Start native processes.**
    Create log directory: `.plax/logs/<name>/`

    For each process in `deps.Blueprint.Processes`:
    - Build the environment:
      ```
      env := os.Environ()
      for k, v := range derivedEnvVars {  // parsed from <worktree>/.env
          env = setOrReplace(env, k, v)
      }
      if proc.PortVar != "" {
          env = setOrReplace(env, proc.PortVar, strconv.Itoa(allocated[proc.PortVar]))
        }
      ```
      ⚠ The derived .env already contains rendered hole values (e.g.
        `NEXTAUTH_URL=http://localhost:3001`). The explicit `port_var` set
        ensures the process's own port var exists even if it's not a hole key.
    - If `proc.Command` contains `{{VAR}}` placeholders, substitute with values.
    - Spawn:
      ```go
      pgid, startTime := process.Spawn(
          proc.Name,
          renderedCommand,
          env,
          filepath.Join(worktreePath, proc.Workdir),
          filepath.Join(logDir, proc.Name+".log"),
      )
      pids[proc.Name] = pgid
      pidStarts[proc.Name] = startTime  // identity guard against PGID reuse
      ```
    ⚠ On failure, terminate all process groups started so far in this loop.
    Cleanup: terminate all started process groups (registered before the
    loop; see "Rollback pattern").

15. **Verify workloads stayed up.** Sleep ~300ms, then for each container
    `deps.Docker.ServiceRunning(ctx, cid)` must be true and for each process
    `process.IsAlive(pgid)` must be true. A workload that exited immediately
    (typo'd command, missing binary, bad image entrypoint) fails `up` with a
    clear error and full rollback — it must not be recorded as "running".
    This is an early-exit check, not a readiness/health check.

16. **Write registry.**
    ```go
    deps.Registry.AddInstance(name, registry.InstanceRecord{
        Branch:       "plax/" + name,
        WorktreePath: worktreePath,
        CreatedAt:    time.Now(),
        State:        "running",
        Ports:        allocated,
        DBName:       "plax_" + name,
        ContainerIDs: containerIDs,
        PIDs:         pids,
        PIDStarts:    pidStarts,
        Provenance: registry.Provenance{
            BaseVersion: baseInfo.ProvenanceVer,
            Toolchain:   toolchainHash,
        },
    })
    deps.Registry.Save()
    ```

17. **Print summary to stderr.**
    ```
    instance <name> up
      worktree:  .plax/worktrees/<name>
      branch:    plax/<name>
      database:  plax_<name>
      ports:     PORT=3001 REDIS_PORT=6380 GOTENBERG_PORT=3031
      logs:      .plax/logs/<name>/
    ```

### Down

1. **Load instance record.**
   `deps.Registry.GetInstance(name)` → must return true.
   ⚠ If not found: `instance "<name>" not found`

2. **Terminate native processes.**
   For each `pid` in `rec.PIDs`:
   ```go
   process.Terminate(pid, rec.PIDStarts[name], 5*time.Second)
   ```
   ⚠ No-op on already-dead processes. Do not fail.
   ⚠ The recorded start time guards against PID reuse: if the original
     process is gone and its PGID now belongs to an unrelated process,
     Terminate returns `ErrStaleProcess` without signaling anything. Log a
     note and continue — the instance's process is already gone.

3. **Stop and remove dedicated containers.**
   For each `containerID` in `rec.ContainerIDs`:
   ```go
   deps.Docker.StopService(ctx, containerID)
   deps.Docker.RemoveService(ctx, containerID)
   ```
   ⚠ Both are no-ops on missing containers. Do not fail.

4. **Remove container volumes.**
   For each service with `isolation: "dedicated"` that had volumes:
   ```go
   deps.Docker.RemoveVolume(ctx, volumeName)
   ```
   ⚠ Volume names are reconstructed from the naming convention:
     `plax-<instance>-<service>-<vol>`. Since `ServiceDef` does not yet declare
     volumes, this step is a no-op in Phase 3. When volumes are added to the
     blueprint, this step iterates the same list that `up` used.
   ⚠ No-op on missing volumes. Do not fail.

5. **Drop instance database.**
   ```go
   deps.BM.DropInstanceDB(ctx, rec.DBName)
   ```
   ⚠ No-op if absent. Do not fail.

6. **Remove Docker network.**
   ```go
   deps.Docker.RemoveNetwork(ctx, "plax-"+name+"-net")
   ```
   ⚠ No-op if absent. Do not fail.

7. **Remove worktree and branch.**
   ```go
   worktree.Remove(deps.RepoRoot, name)
   ```
   ⚠ `git worktree remove` may fail if the worktree has uncommitted changes.
     Use `--force` — the worktree is disposable by design.
   ⚠ `git branch -D` always force-deletes. Plax owns the branch. If there are
     unmerged commits, print a note to stderr: `note: branch plax/<name> had
     unmerged commits`

8. **Remove registry entry.**
   ```go
   deps.Registry.RemoveInstance(name)  // also sweeps port allocations
   deps.Registry.Save()
   ```

9. **Print confirmation to stderr.**
   ```
   instance <name> down
   ```

`Down` is deliberately not atomic. Steps 2–7 are idempotent and tolerant of
missing resources — failures are logged to stderr and execution continues.
The CLI builds Down's dependencies tolerantly: if Postgres or Docker is
unavailable, the corresponding field is nil and Down skips only that
backend's resources with a warning. Step 8 (registry removal) always runs,
even if earlier steps failed, because the registry entry is the source of
truth for what exists. Leaving a stale entry would prevent future `up` calls
with the same name and leak port allocations.

### DeriveEnv

Inputs: template file path, overrides file path, holes map, values map, output path.

1. Load the overrides file (the user's own `.env`). Not an error if absent —
   the template is the fallback.

2. Read the template file line by line.

3. For each line:
   a. If blank or starts with `#` → write verbatim to output.
   b. Extract the key (everything before the first `=`).
   c. If the key is in `holes`:
      - `Render(holes[key], values)` → renderedValue
      - Write `KEY=renderedValue` to output.
      - Mark the key as found.
   d. Else if the key is in `overrides`:
      - Write `KEY=userValue` to output.
      ⚠ This is how secrets reach the instance — the user's `.env` has real
        values, the template has empty placeholders.
   e. Otherwise → write the line verbatim.

4. For each key in `holes` that was NOT found in the template:
   - `Render(holes[key], values)` → renderedValue
   - Append `KEY=renderedValue` at the end of the output.
   ⚠ This handles env vars introduced by the blueprint that are not yet in
     `.env.example`.

5. Write the output to `outputPath`.

### Render

Input: template string, values map.

1. Find all `{{VAR}}` placeholders using the regex `\{\{(\w+)\}\}`.

2. For each placeholder `VAR`:
   - If `VAR` is in `values` → replace with `values[VAR]`.
   - If `VAR` is NOT in `values` → return error:
     `env: template references unknown variable {{VAR}}`
   ⚠ Unknown variables are always an error. A hole template referencing a
     variable that was never allocated is a blueprint bug and should fail loudly.

3. Return the rendered string.

---

## CLI specification

All Phase 3 commands accept `--root` (default `.`) for repo root, consistent
with Phase 1 and 2 commands.

### `plax up <name>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional) |
| Exit 0 | Instance created and running |
| Exit 1 | Any step fails (see error table) |
| Stderr | Progress messages, then summary block |
| Stdout | Nothing |
| Idempotent | No — second call with same name returns error |

### `plax down <name>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional) |
| Exit 0 | Instance fully torn down |
| Exit 1 | Instance not found, or unexpected error |
| Stderr | Progress messages |
| Stdout | Nothing |
| Idempotent | No — second call returns "not found" error |

### `plax ls`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional, default `.`), `--json` |
| Exit 0 | Always |
| Stderr | Nothing |
| Stdout | Table (default) or JSON (`--json`) |

**Table output (default):**

```
NAME   STATE     BRANCH              PORTS                     CREATED
i1     running   plax/i1             3001 6380 3031            5m ago
i2     running   plax/i2             3002 6381 3032            2m ago
```

Ports are the allocated host port numbers, space-separated, sorted ascending.
Created is relative time (`5m ago`, `2h ago`, `3d ago`).

With no instances, prints the header row only.

**JSON output (`--json`):**

Prints the full `registry.Instances` map as JSON.

### `plax attach <name>`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional, default `.`) |
| Exit 0 | Shell exited normally |
| Exit 1 | Instance not found, .env missing, no shell found |
| Stdin | Inherited from parent |
| Stdout/Stderr | Inherited from parent |

Spawns an interactive shell with:
- Working directory: instance worktree path
- Environment: `os.Environ()` merged with parsed `<worktree>/.env` (derived
  values override host values)
- Shell discovery: `$SHELL` env var → `/bin/bash` → `/bin/sh`

The shell runs with `--login` to source the user's profile.

### `plax exec <name> -- <cmd> [args...]`

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional, default `.`) |
| Exit | Exit code of the executed command |
| Stdin | Inherited from parent |
| Stdout/Stderr | Inherited from parent |

Same environment setup as `attach`, but runs the given command
non-interactively. The `--` separator distinguishes the instance name from
the command.

Example: `plax exec i1 -- bun test`

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| Invalid instance name | Regex `^[a-z][a-z0-9_]*$` fails | `invalid instance name "<name>": must match ^[a-z][a-z0-9_]*$ (max 32 chars)`. Exit 1. |
| Invalid blueprint | `ValidateStructural` returns errors | `invalid blueprint: <joined errors>`. No side effects. Exit 1. |
| Ctrl-C during `up` | SIGINT cancels the operation context (`signal.NotifyContext`) | In-flight step aborts; rollback runs with a non-canceled context. Exit 1. |
| Workload exits immediately | Post-start liveness sweep finds dead process/container | `process|service "<name>" exited immediately after start`. Rollback. Exit 1. |
| Stale PGID in `down` | Recorded start time does not match the live process | Note to stderr (`already gone`), never signals the reused group. Continues. |
| Instance already exists | Registry has record | `instance "<name>" already exists`. Exit 1. |
| Branch exists but no registry record | `BranchExists` returns true | `branch "plax/<name>" exists but instance is not registered — run 'git branch -D plax/<name>' to clean up`. Exit 1. |
| Base database missing | `BaseStatus.Exists` is false | `base database does not exist — run 'plax base reset' first`. Exit 1. |
| Base database unlocked | `BaseStatus.Locked` is false | `base database is not locked — run 'plax base reset' to repair`. Exit 1. |
| Port pool exhausted | `Pool.Allocate` returns error | `portpool: no free port in range N-M`. Rollback all prior steps. Exit 1. |
| .env template missing | `os.ReadFile` returns `ErrNotExist` | `env: template not found at <path>`. Rollback. Exit 1. |
| Unknown template variable | `Render` finds `{{VAR}}` not in values | `env: template references unknown variable {{VAR}}`. Rollback. Exit 1. |
| CloneBase fails | Postgres error (55006, etc.) | Wrapped error from Phase 2. Rollback. Exit 1. |
| Docker daemon unreachable | `NewDriver` returns error | `docker: cannot connect to daemon. Is Docker running?`. Rollback. Exit 1. |
| Docker image pull fails | `RunService` → `pullImage` error | `docker: pull <image>: <err>`. Rollback. Exit 1. |
| Container start fails (port bound) | `ContainerStart` error | `docker: start container <name>: port is already allocated`. Rollback. Exit 1. |
| Process spawn fails | `process.Spawn` returns error | `process: spawn <name>: <err>`. Rollback. Exit 1. |
| Registry save fails | `Save` returns error | `registry: write: <err>`. Exit 1. (Side effects exist but instance is usable — re-run `up` will fail on "already exists"; user runs `down` then `up`.) |
| `down` on nonexistent instance | `GetInstance` returns false | `instance "<name>" not found`. Exit 1. |
| `git worktree remove` fails | git exits non-zero | Log to stderr, continue teardown. |
| `git branch -D` has unmerged commits | git output contains "not fully merged" | Note to stderr, continue. |
| Instance not found for attach/exec | `GetInstance` returns false | `instance "<name>" not found`. Exit 1. |
| .env missing for attach/exec | `os.ReadFile` returns `ErrNotExist` | `env: .env not found at <path> — was the instance created with 'plax up'?`. Exit 1. |
| No shell found for attach | `$SHELL` unset, `/bin/bash` and `/bin/sh` missing | `attach: no shell found (checked $SHELL, /bin/bash, /bin/sh)`. Exit 1. |

---

## Tests

### Test prerequisites

Unit tests for `derive/env`, `blueprint`, `registry`, `portpool`, and
`process` have no external dependencies; `worktree` and `instance` tests
require git only. `pkg/instance` tests fake the Postgres and Docker backends
through the `BaseManager`/`DockerDriver` interfaces.

Integration tests (`pkg/derive/postgres`, `pkg/derive/docker`) require a
running Postgres (`PLAX_TEST_POSTGRES_URL`) and Docker daemon. The e2e test
(`cmd/plax`) requires both plus git and python3.

Suites that create or drop the well-known `plax_base` database share one
server across parallel package binaries, so they serialize on a Postgres
advisory lock (`pkg/testutil.LockPostgres`).

### Test fixtures

`pkg/derive/env/testdata/` — template files for derivation tests:

```
testdata/
  basic.env.example         # Standard template with holes and non-holes
  sparse.env.example        # Template missing some hole keys (tests append)
  comments.env.example      # Template with comments and blank lines
```

### Unit tests

**`pkg/derive/env/derive_test.go`:**

- `TestDerive_BasicSubstitution` — template with known holes → correct output
- `TestDerive_NonHoleLinesPreserved` — comments, blank lines, non-hole vars copied verbatim
- `TestDerive_MissingHoleInTemplate` — hole key not in template → appended at end
- `TestDerive_DBNameSubstitution` — `{{DB_NAME}}` → `plax_<name>`
- `TestDerive_MultipleHolesInOneValue` — value with two `{{VAR}}` placeholders → both replaced
- `TestDerive_UnknownVar` — `{{UNKNOWN}}` in template → error
- `TestParseFile_Basic` — `KEY=value` lines parsed correctly
- `TestParseFile_Comments` — `#` lines skipped, `KEY=value # comment` → value without comment
- `TestParseFile_QuotedValues` — `KEY="value"` → value without quotes
- `TestParseFile_EmptyValue` — `KEY=` → empty string
- `TestParseFile_ExportPrefix` — `export KEY=value` parses as `KEY`
- `TestParseFile_QuotedWithTrailingComment` — `KEY="abc" # note` → `abc`; quoted `#` preserved
- `TestDerive_ExportPrefixedHole` — `export PORT=...` template line is substituted in place
- `TestDerive_ExportPrefixedOverride` — user override matches export-prefixed template key
- `TestDerive_OverridePreservesQuoting` — `TOKEN="abc # def"` round-trips through derive + re-parse
- `TestRender_Basic` — `{{VAR}}` replaced with value
- `TestRender_UnknownVar` — error returned
- `TestRender_NoPlaceholders` — string without `{{}}` returned as-is

**`pkg/blueprint/validate_test.go`** (additions):

- `TestValidateStructural_IgnoresMissingHoles` — hole presence is a warning, not structural
- `TestValidate_DockerNameCollision` — `foo_bar` + `foo-bar` services → error
- `TestValidate_BadServiceName` / `TestValidate_BadProcessName` — charset enforced

**`pkg/worktree/worktree_test.go`** (requires git):

- `TestCreate_Success` — creates branch and worktree in a temp repo
- `TestCreate_BranchExists` — error on duplicate branch
- `TestRemove_Success` — removes worktree and branch
- `TestRemove_MissingWorktreeStillDeletesBranch` — pruned worktree → branch still deleted
- `TestBranchName` — `"i1"` → `"plax/i1"`
- `TestWorktreeRelPath` — `"i1"` → `".plax/worktrees/i1"`
- `TestBranchExists_True` — after Create, returns true
- `TestBranchExists_False` — before Create, returns false

**`pkg/process/supervisor_test.go`:**

- `TestSpawn_Success` — spawn `sleep 60`, verify PID > 0, verify alive, nonzero start time
- `TestSpawn_WithEnv` — spawn `env`, capture output, verify custom var present
- `TestSpawn_WithDir` — spawn `pwd`, verify output matches dir
- `TestSpawn_LogFile` — spawn `echo hello`, verify log file contains "hello"
- `TestSpawn_ProcessGroup` — spawn a parent that forks children, verify pgid == pid
- `TestTerminate_Graceful` — spawn `sleep 60`, terminate, verify dead
- `TestTerminate_StalePGIDNotSignaled` — mismatched start time → `ErrStaleProcess`, no signal
- `TestTerminate_ChildrenKilled` — spawn a parent that forks a child, terminate pgid, verify both dead
- `TestTerminate_AlreadyDead` — terminate a dead PID → no error (with and without start time)
- `TestIsAlive_Running` — alive process → true
- `TestIsAlive_Dead` — dead process → false
- `TestStartTime_Running` — start time stable while process lives
- `TestStartTime_Dead` — missing process → 0

**`pkg/instance/instance_test.go`** (requires git; backends faked):

- `TestUp_Success` — registry record, worktree, derived .env, cloned DB, started container, live process, stamps
- `TestUp_DuplicateName` — second `Up` with same name → error
- `TestUp_HyphenNameRejected` — `foo-bar` fails validation, no side effects
- `TestUp_InvalidBlueprint_NoSideEffects` — duplicate process names fail before any side effect
- `TestUp_RollbackOnCloneFailure` — worktree/branch/network/ports all cleaned
- `TestUp_RollbackOnImmediateExit` — `exit 1` process caught by the liveness sweep, full rollback
- `TestUp_RollbackOnCancel` — canceled context still rolls back with a live cleanup context
- `TestDown_Success` — registry empty, worktree/branch gone, DB dropped, containers/network removed, process dead
- `TestDown_NotFound` — error
- `TestDown_MissingWorktree_BranchDeleted` — tolerant of missing resources
- `TestDown_NilBackends_StillCleans` — nil BM/Docker skip those resources; everything else cleaned
- `TestDown_StalePGID_NotSignaled` — corrupted start time → process not signaled

### End-to-end test

`TestEndToEnd_TwoInstances` (`cmd/plax/e2e_test.go`) builds the real binary
and runs it against a synthetic fixture repo (generic: python http.server +
Postgres + Redis) created in a temp dir. Verify:
- `base reset` → `up i1` → `up i2` succeed
- Both are running simultaneously on different ports; `curl` returns 200 for both
- Different databases (`plax_i1`, `plax_i2`); containers running; networks exist
- `exec` sees the derived env (including a quoted secret round-trip) and
  propagates exit codes; `attach` opens a shell; `ls` lists both instances
- `up` with an immediately-exiting process fails and leaves zero residue
- `down` both: worktrees, branches, databases, containers, networks, ports,
  and registry entries all removed; `ls --json` prints `{}`
- `down` on a nonexistent instance fails clearly

Skipped unless `PLAX_TEST_POSTGRES_URL` is set and Docker, git, and python3
are available; must pass locally before merge. Holds the `plax_base`
advisory lock for its duration.

---

## Acceptance criteria

Verified by `cmd/plax/e2e_test.go` (real binary, real Postgres/Docker/git),
the `pkg/instance` unit tests, or the package unit tests, as noted.

- [x] `plax up i1` in the sample repo creates a running instance accessible at the allocated port *(e2e: curl 200)*
- [x] `plax up i1` a second time returns an error (not a no-op) *(TestUp_DuplicateName)*
- [x] `plax up i2` while `i1` is running creates a second non-colliding instance *(e2e: distinct ports, both 200)*
- [x] `plax up` with an invalid name (`"Foo"`, `""`, `"a/b"`, `"foo-bar"`) returns a clear error *(TestUp_HyphenNameRejected covers the shared regex path)*
- [x] `plax up` failure at any step leaves no residual state (branch, worktree, DB, containers, ports all cleaned up) *(TestUp_RollbackOnCloneFailure, TestUp_RollbackOnImmediateExit, TestUp_RollbackOnCancel, e2e i3)*
- [x] Ctrl-C during `up` aborts and rolls back with a non-canceled cleanup context *(TestUp_RollbackOnCancel)*
- [x] A workload that exits immediately fails `up` instead of being recorded as running *(TestUp_RollbackOnImmediateExit; e2e i3)*
- [x] `plax down i1` removes all traces: worktree, branch, database, containers, volumes, network, ports, registry entry *(e2e; no volumes exist yet — step is a documented no-op)*
- [x] `plax down` with Postgres or Docker unavailable still cleans everything else *(TestDown_NilBackends_StillCleans)*
- [x] `plax down` never signals a process group whose PGID was reused *(TestDown_StalePGID_NotSignaled; Linux `/proc` guard)*
- [x] `plax down nonexistent` returns a clear error *(e2e; TestDown_NotFound)*
- [x] `plax ls` prints a table with name, state, branch, ports, age *(e2e)*
- [x] `plax ls --json` prints valid JSON matching `registry.Instances` *(e2e: 2 instances, then `{}`)*
- [x] `plax ls` with no instances prints header only *(e2e)*
- [x] `plax attach i1` opens an interactive shell in the worktree with derived env *(e2e smoke: login shell launches and exits cleanly)*
- [x] `plax exec i1 -- echo $PORT` prints the allocated port *(e2e: `printenv PORT`)*
- [x] `plax exec i1 -- bun test` exits with the test command's exit code *(e2e: `exit 3` propagates)*
- [x] `.env` derivation: hole keys in template are replaced, hole keys not in template are appended, non-hole lines are verbatim *(env unit tests)*
- [x] `.env` derivation: quoted override values round-trip, `export` prefixes normalized, quote-aware comment stripping *(env unit tests + e2e quoted secret)*
- [x] `.env` derivation: `{{DB_NAME}}` resolves to `plax_<name>` *(unit + e2e)*
- [x] `.env` derivation: unknown `{{VAR}}` in template returns an error *(unit)*
- [x] Processes spawn as process group leaders; terminate kills the entire group *(supervisor tests)*
- [x] Process stdout/stderr are captured in `.plax/logs/<name>/<process>.log` *(supervisor tests)*
- [x] Registry `BlueprintStamp` is populated on `up` *(TestUp_Success)*
- [x] `InstanceRecord.Provenance.BaseVersion` reflects the base at creation time *(TestUp_Success)*
- [x] `InstanceRecord.Provenance.Toolchain` reflects the toolchain file hash at creation time *(TestUp_Success)*
- [x] `go vet ./...` passes
- [x] `go test -race -count=1 ./...` passes (both with and without `PLAX_TEST_POSTGRES_URL`; e2e included when set)
- [x] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| kong | `github.com/alecthomas/kong` | v1.16.0 | CLI framework (already in go.mod) |

Standard library (no new external dependencies):

- `os/exec` — git commands, process spawning
- `os/signal` — `NotifyContext` so Ctrl-C cancels `up` and triggers rollback
- `syscall` — `SysProcAttr{Setpgid: true}`, `Kill` with negative PID for groups
- `context` — `WithoutCancel` for rollback/cleanup paths
- `regexp` — `{{VAR}}` template placeholder parsing
- `crypto/sha256` — blueprint stamp, toolchain hash
- `strings` — .env parsing, template rendering
- `bufio` — line-by-line template reading
- `time` — relative time formatting for `ls`
- `fmt` — table output formatting

Existing project dependencies (no change):

- `github.com/apollopower/plax/pkg/blueprint` — parsed blueprint
- `github.com/apollopower/plax/pkg/registry` — instance persistence
- `github.com/apollopower/plax/pkg/portpool` — port allocation
- `github.com/apollopower/plax/pkg/derive/postgres` — database cloning
- `github.com/apollopower/plax/pkg/derive/docker` — container management

---

## Concurrency note

The registry has no file locking. Two simultaneous `plax up` commands could
corrupt it. This is acceptable for Phase 3 — Plax is a single-user tool
running on one machine, and the user controls when commands run. File locking
(e.g. `flock` on `.plax/registry.json`) is a candidate improvement if
concurrent access becomes a real problem.

---

## Deferred items

| Item | Deferred to | Reason |
|---|---|---|
| Volumes in blueprint (`ServiceDef.Volumes`) | When a service needs persistence | No sample service needs custom mount paths; driver mounts at `/data` |
| `suspend` / `resume` commands | Phase 4 | Needs port re-probe, drift report |
| `status` / drift report | Phase 4 | Needs provenance comparison, config stamp diff |
| `doctor` | Phase 4 | Needs blueprint-vs-repo validation |
| `rederive` | Phase 4 | Needs instance iteration + .env regeneration |
| Unread message count in `ls` | Phase 5 | Mailbox not built yet |
| Drift notice in `attach` | Phase 4 | Drift report not built yet |
| File locking on registry | When concurrency is a real problem | Single-user tool, sequential access is the norm |
| PID-reuse protection on macOS | When macOS support matters | Start-time identity requires `/proc`; macOS keeps PGID-only behavior |
| Readiness/health checks beyond early-exit | Phase 4 | The liveness sweep only catches immediate exits; real readiness belongs with `status`/drift |
