# Phase 2 — Derivation Engine

## Objective

Build the drivers that create, clone, seed, reset, and refresh resource
instances — Postgres (logical) and Docker (dedicated) — so Phase 3 can wire
them into `plax up`.

---

## Package layout

```
pkg/
  derive/
    postgres/
      basemanager.go          # BaseManager: CRUD for plax_base, clone, drop
      basemanager_test.go     # Integration tests against a running Postgres
      provenance.go           # _plax_provenance table DDL + stamp/read helpers
      provenance_test.go      # Stamp → read round-trip, version increment
    docker/
      driver.go               # RunService, StopService, RemoveService, RemoveVolume
      driver_test.go          # Tests against Docker daemon (skipped in CI without Docker)
      network.go              # CreateNetwork, RemoveNetwork per instance
```

---

## Type specifications

### `pkg/derive/postgres/basemanager.go`

```go
package postgres

import (
    "context"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/apollopower/plax/pkg/blueprint"
)

// BaseManager manages the shared Postgres base database and per-instance
// clones. It connects to the Postgres server the blueprint declares (via
// the logical service's env block), using the host and port provided at
// construction.
type BaseManager struct {
    pool      *pgxpool.Pool
    bp        *blueprint.Blueprint
    repoRoot  string            // working directory for migrate/seed commands
    baseName  string            // "plax_base" unless blueprint overrides
}

// NewBaseManager connects to Postgres and returns a ready BaseManager.
//
// connString is a full Postgres DSN (e.g. "postgres://postgres:postgres@
// localhost:5432/postgres?sslmode=disable"). It must point at an existing
// database (typically "postgres") — the base database itself is created/dropped
// by the manager.
//
// repoRoot is the path to a checkout of the repo where migration/seed commands
// run. bp is the parsed blueprint (needed for SeedConfig fields).
func NewBaseManager(ctx context.Context, connString string, repoRoot string, bp *blueprint.Blueprint) (*BaseManager, error)
```

**Public methods:**

| Method | Signature | Behavior |
|---|---|---|
| `CreateBase` | `(bm *BaseManager) CreateBase(ctx context.Context) error` | Creates empty base (migrated, no seed data). Idempotent — no-op if base already exists and is locked. |
| `SeedBase` | `(bm *BaseManager) SeedBase(ctx context.Context) error` | Runs seed command into base. **Only safe when no instances exist** — use `RefreshBase` for ongoing updates. |
| `ResetBase` | `(bm *BaseManager) ResetBase(ctx context.Context) error` | Drops and recreates base (migrated only, no seed data). Destructive. |
| `CloneBase` | `(bm *BaseManager) CloneBase(ctx context.Context, targetDB string) error` | TEMPLATE-clones the base into `targetDB`. Returns error if base is not locked (ALLOW_CONNECTIONS false means no concurrent connections; Postgres enforces this). |
| `RefreshBase` | `(bm *BaseManager) RefreshBase(ctx context.Context) error` | Staged refresh via `base_next` swap. Safe during concurrent clones. |
| `DropInstanceDB` | `(bm *BaseManager) DropInstanceDB(ctx context.Context, dbName string) error` | Terminates connections then drops the database. No-op if absent. |
| `BaseStatus` | `(bm *BaseManager) BaseStatus(ctx context.Context) (BaseInfo, error)` | Returns existence, lock state, provenance version, creation time, whether `base_next` exists. |
| `Close` | `(bm *BaseManager) Close()` | Closes the connection pool. |

```go
type BaseInfo struct {
    Exists         bool      `json:"exists"`
    Locked         bool      `json:"locked"`
    ProvenanceVer  int       `json:"provenance_version"`
    CreatedAt      time.Time `json:"created_at"`
    HasBaseNext    bool      `json:"has_base_next"`    // deferred swap pending
}
```

### `pkg/derive/postgres/provenance.go`

```go
package postgres

import "time"

// ProvenanceRow is the single row in _plax_provenance inside every
// database (base and every clone).
type ProvenanceRow struct {
    Version     int       `json:"version"`
    Source      string    `json:"source"`       // "base" or clone target name
    CreatedAt   time.Time `json:"created_at"`
    SeedCommand string    `json:"seed_command"`
    SchemaHash  string    `json:"schema_hash"`  // SHA-256 of sorted migration filenames
}

// CreateProvenance creates the _plax_provenance table in the current
// database if it does not exist, then inserts the given row.
func CreateProvenance(ctx context.Context, pool *pgxpool.Pool, row ProvenanceRow) error

// ReadProvenance reads the single row from _plax_provenance in the
// current database. Returns nil, nil if the table or row is absent.
func ReadProvenance(ctx context.Context, pool *pgxpool.Pool) (*ProvenanceRow, error)

// ComputeSchemaHash returns the SHA-256 hex string of a sorted, newline-
// joined list of migration filenames found under the given directory.
// Returns empty string if the directory does not exist or is empty.
func ComputeSchemaHash(migrationsDir string) (string, error)
```

**DDL:**

```sql
CREATE TABLE IF NOT EXISTS _plax_provenance (
    version       INTEGER NOT NULL,
    source        TEXT NOT NULL DEFAULT 'base',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    seed_command  TEXT NOT NULL DEFAULT '',
    schema_hash   TEXT NOT NULL DEFAULT ''
);
```

Single-row table. The column is `seed_command` (not `command`) to match the JSON tag conventions used throughout.

### `pkg/derive/docker/driver.go`

```go
package docker

import (
    "context"
    "github.com/docker/docker/api/types/container"
    "github.com/docker/docker/client"
    "github.com/apollopower/plax/pkg/blueprint"
)

// ServiceConfig is the resolved configuration for a dedicated service
// container. The orchestration layer (Phase 3) allocates ports and
// resolves env var names, then passes them here as concrete values.
type ServiceConfig struct {
    InstanceName string            `json:"instance_name"`   // e.g. "i1"
    ServiceName  string            `json:"service_name"`    // e.g. "redis"
    Image        string            `json:"image"`
    Command      []string          `json:"command,omitempty"`
    Env          map[string]string `json:"env,omitempty"`
    // PortMap maps container port (string, e.g. "6379") → host port (int).
    // Already resolved by the caller — this driver does not allocate ports.
    PortMap      map[string]int    `json:"port_map,omitempty"`
    Volumes      []string          `json:"volumes,omitempty"` // named volume names
    NetworkName  string            `json:"network_name"`      // e.g. "plax-i1-net"
}

// Driver manages Docker containers for dedicated services.
type Driver struct {
    cli *client.Client
}

func NewDriver() (*Driver, error)
func (d *Driver) Close() error
```

**Public methods:**

| Method | Signature | Behavior |
|---|---|---|
| `RunService` | `(d *Driver) RunService(ctx context.Context, cfg ServiceConfig) (containerID string, err error)` | Pulls image if missing, creates named volumes, creates container, connects to network, starts. Returns container ID. |
| `StopService` | `(d *Driver) StopService(ctx context.Context, containerID string) error` | Stop with 10s timeout. No-op if not running. |
| `RemoveService` | `(d *Driver) RemoveService(ctx context.Context, containerID string) error` | Force-remove container. |
| `RemoveVolume` | `(d *Driver) RemoveVolume(ctx context.Context, volumeName string) error` | Remove named volume. No-op if absent. |
| `CreateNetwork` | `(d *Driver) CreateNetwork(ctx context.Context, name string) error` | Create bridge network. Idempotent. |
| `RemoveNetwork` | `(d *Driver) RemoveNetwork(ctx context.Context, name string) error` | Remove network. No-op if absent. |

### `pkg/derive/docker/network.go`

```go
package docker

// Wrappers around the Docker SDK's network operations, kept in a separate
// file for clarity. The public API is on Driver (above).
```

### Blueprint type additions (Phase 1 follow-up)

`pkg/blueprint/blueprint.go` gains one field on `SeedConfig`:

```go
type SeedConfig struct {
    Migrate string `json:"migrate"` // e.g. "bun run db migrate"
    Command string `json:"command"` // e.g. "bun run db fixtures"
    Workdir string `json:"workdir"` // relative to repo root, e.g. "."
}
```

Validation rule V11 extends to check `seed.migrate` is non-empty. The
`migrate` field is **required** — there is no default, because guessing a
migration command is what `plax init` and the agent are for.

The blueprint example in `docs/plans/index.md` updates to:

```json
"seed": {
    "migrate": "bun run db migrate",
    "command": "bun run db fixtures",
    "workdir": "."
}
```

---

## Algorithms

All algorithms assume the caller holds a valid `*BaseManager` constructed via
`NewBaseManager`. Context cancellation is respected throughout.

Two rules apply everywhere below:

- **Temporary pools are closed by their opener.** Any step that opens a pool
  to a specific database (`plax_base`, `plax_base_next`, a clone) closes it
  in the same function via `defer pool.Close()`. The manager's own pool is
  closed only by `BaseManager.Close()`.
- **Migrate/seed commands receive `DATABASE_URL`.** The connection string
  passed to a command's environment is built by rendering the blueprint's
  `env.holes["DATABASE_URL"]` template with `{{DB_NAME}}` replaced by the
  target database name. For the sample blueprint, targeting `plax_base`
  yields `postgres://postgres:postgres@localhost:5432/plax_base`. The command
  inherits the process environment with this one variable overridden.

### CreateBase

1. Check if `plax_base` exists via `SELECT 1 FROM pg_database WHERE datname = 'plax_base'`.
   ⚠ If it exists AND is locked (`SELECT datallowconn FROM pg_database WHERE datname = 'plax_base'` returns false), return nil (idempotent).
   ⚠ If it exists and is NOT locked, return an error: "base exists but is not locked — run 'plax base reset' to recreate."

2. Connect to the `postgres` maintenance database and run:
   `CREATE DATABASE plax_base`
   ⚠ The pool was created against the `postgres` database, so no connection switch is needed.

3. Open a new pool connected **directly to** `plax_base` (not via `postgres`). Build the DSN by substituting the database name in the original connection string.
   ⚠ pgx pools are per-database. Opening a second pool for `plax_base` is necessary. Close it when done with this step.

4. Run migrations against `plax_base`:
   Execute `bp.Seed.Migrate` in `bm.repoRoot` (or `filepath.Join(bm.repoRoot, bp.Seed.Workdir)`) with `DATABASE_URL` rendered for `plax_base` (see rules above).
   ⚠ The command is shell-spawned — the repo's toolchain (Bun, Node) must be available on `$PATH`.
   ⚠ If the command exits non-zero, drop the database and return the error.

5. Compute schema hash:
   Find migration files under `bm.repoRoot` (the migration directory is repo-specific — for the sample: `src/db/migrations/`). Sort filenames, join with newlines, SHA-256.
   ⚠ If no migration directory exists, schema_hash is empty string. This is valid for repos that don't use file-based migrations.
   ⚠ The migrations directory is **hardcoded repo-layout knowledge for day one**. It becomes a blueprint field the day a second repo has a different layout — do not generalize it now.

6. Stamp provenance:
   `CreateProvenance(ctx, pool, ProvenanceRow{Version: 1, Source: "base", SeedCommand: bp.Seed.Command, SchemaHash: hash})`

7. Lock: `ALTER DATABASE plax_base WITH ALLOW_CONNECTIONS false`
   ⚠ This must run on the `postgres` pool, not the `plax_base` pool (which would be locked out).

8. Close the `plax_base` pool.

### SeedBase

⚠ **Precondition:** no instances exist. This is a development-time convenience.
For ongoing updates with live instances, use `RefreshBase`.

1. Verify `plax_base` exists. If absent, return error: "base does not exist — run 'plax base reset' first."

2. Unlock: `ALTER DATABASE plax_base WITH ALLOW_CONNECTIONS true`
   ⚠ From this point until re-lock, concurrent `CloneBase` calls can succeed on a partially seeded base. The caller is responsible for ensuring no clones are in flight.

3. Open a pool connected to `plax_base`.

4. Run seed command:
   Execute `bp.Seed.Command` in `filepath.Join(bm.repoRoot, bp.Seed.Workdir)` with `DATABASE_URL` rendered for `plax_base` (see rules above).
   ⚠ If the command exits non-zero, re-lock the base (attempt), close the pool, return the error.

5. Increment provenance:
   Read current version from `_plax_provenance`, increment, update the row.
   ⚠ The `schema_hash` is NOT recomputed here — migrations don't run during SeedBase.

6. Re-lock: `ALTER DATABASE plax_base WITH ALLOW_CONNECTIONS false`

7. Close the `plax_base` pool.

### ResetBase

1. Drop if exists: terminate all connections to `plax_base`, then `DROP DATABASE IF EXISTS plax_base`.
   ⚠ Connection termination: `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'plax_base' AND pid <> pg_backend_pid()`.

2. Call `CreateBase`. (Steps 2–8 above, reusing the same logic.)

### CloneBase

1. Verify `plax_base` exists. If absent, return error.

2. Verify base is locked: `SELECT datallowconn FROM pg_database WHERE datname = 'plax_base'`.
   ⚠ Returns false → locked → proceed. Returns true → unlocked → return error: "base is not locked; clones are unsafe."

3. Check `targetDB` does not already exist. If it does, return error: "database <targetDB> already exists."

4. `CREATE DATABASE <targetDB> TEMPLATE plax_base`
   ⚠ Postgres serializes TEMPLATE clones — concurrent calls to CloneBase queue on the template lock. Clones are ~100ms, so this is acceptable.
   ⚠ If a connection is active on `plax_base` (e.g. a stray psql session someone opened before the lock was set), Postgres returns error 55006 ("source database is being accessed by other users"). Return this as a wrapped error.

5. The clone inherits `_plax_provenance` from the base. Update the `source` field to `targetDB`:
   `UPDATE _plax_provenance SET source = $1`, running on a pool connected to the clone.
   ⚠ Open a temporary pool to the clone for this update, then close it.

### RefreshBase

Staged refresh: creates `plax_base_next` from scratch, then swaps it into place.
Avoids two problems with copying the old base: carrying forward data the seed script
no longer declares, and duplicate rows from non-idempotent seeds.

1. Check `plax_base` exists. If absent, call `CreateBase` and return (nothing to refresh — base was just created empty; the caller should follow with `SeedBase`).

2. Check if `plax_base_next` already exists. If it does, return error: "'plax_base_next' exists — a previous refresh may have been interrupted. Run 'plax base reset' to clean up."

3. `CREATE DATABASE plax_base_next`
   ⚠ Fresh empty database, NOT a template copy.

4. Open pool to `plax_base_next`.

5. Run migrations against `plax_base_next` (same as CreateBase step 4, targeting `plax_base_next`).

6. Compute schema hash (same as CreateBase step 5).

7. Stamp provenance version:
   Read current version from `plax_base`'s `_plax_provenance`. Increment it. Insert provenance into `plax_base_next` with the new version.
   ⚠ If `plax_base` has no provenance row (should not happen), start at version 1.

8. Run seed command against `plax_base_next` (same command as `bp.Seed.Command`, targeting `plax_base_next`).
   ⚠ Connections are allowed during seeding — `plax_base_next` is not locked yet. The swap is what closes it.

9. Lock `plax_base_next`: `ALTER DATABASE plax_base_next WITH ALLOW_CONNECTIONS false`

10. Close the `plax_base_next` pool.

11. Swap:
    - Terminate all connections to `plax_base`: `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'plax_base' AND pid <> pg_backend_pid()`.
      ⚠ This also kills any in-flight TEMPLATE clones. A clone that fails mid-copy is a recoverable error — Phase 3's `plax up` retries CloneBase.
    - `DROP DATABASE plax_base`
      ⚠ If DROP fails because a clone is using TEMPLATE (Postgres holds a lock during the file copy), the DROP blocks briefly but Postgres does not queue DDL behind ongoing TEMPLATE copies — it returns immediately with an error. In that case:
      ⚠ Retry the DROP with exponential backoff: 100ms, 200ms, 400ms, 800ms, 1.6s, 3.2s, 6.4s (7 attempts, ~12.8s total). Between each attempt, re-check if any sessions are connected to `plax_base` and terminate them.
      ⚠ If all retries fail, leave `plax_base_next` in place and return an error: "refresh: could not swap; plax_base_next is ready but the old base is still in use. Run 'plax base refresh' again or 'plax base status' for details."
    - `ALTER DATABASE plax_base_next RENAME TO plax_base`

### DropInstanceDB

1. Check if database exists: `SELECT 1 FROM pg_database WHERE datname = $1`. If absent, return nil (idempotent).

2. Terminate connections: `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`.
   ⚠ This is required — Postgres refuses `DROP DATABASE` while any connection is active.

3. `DROP DATABASE IF EXISTS <dbName>`

### RunService (Docker)

1. Build container name: `plax-<instance>-<service>` (sanitize: lowercase, replace `_` with `-`).

2. Pull image if not present locally. Use `ImagePull` with `RegistryAuth` empty (assumes public images or local Docker credentials).
   ⚠ If pull fails (network, auth), return wrapped error: "docker: pull <image>: <err>"

3. Build port bindings from `cfg.PortMap`:
   For each `containerPort → hostPort`, create `nat.PortBinding{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPort)}`.
   ⚠ All dedicated services bind to 127.0.0.1 only — they are local to the machine.

4. Assemble container config:
   - `Image`: `cfg.Image`
   - `Cmd`: `cfg.Command` (nil if empty — uses image default)
   - `Env`: format `cfg.Env` as `KEY=VALUE` strings, add `cfg.PortMap` env vars as set by the caller
   - `ExposedPorts`: set from `cfg.PortMap` keys

5. Assemble host config:
   - `PortBindings`: `cfg.PortMap` → host port bindings
   - `NetworkMode`: `container.NetworkMode(cfg.NetworkName)`
   - `RestartPolicy`: `container.RestartPolicy{Name: "no"}`
   - `Binds`: for each volume in `cfg.Volumes`, `"<volumeName>:/data"` (or blueprint-specified mount path)

6. Create named volumes: for each volume name, `docker.VolumeCreate` with name `plax-<instance>-<service>-<vol>`. Idempotent.
   ⚠ Docker's `VolumeCreate` on an existing name is a no-op.

7. `cli.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)`
   ⚠ If a container with the same name already exists (stale), remove it first and retry once.

8. `cli.ContainerStart(ctx, containerID, types.ContainerStartOptions{})`

9. Return container ID.

### StopService / RemoveService / RemoveVolume / CreateNetwork / RemoveNetwork

Straightforward Docker SDK calls. Implementation details deferred to the code; the plan's concern is the API surface (correct signatures, correct error propagation).

### BaseStatus

1. Query `pg_database` for `plax_base` existence and `datallowconn`:

   ```sql
   SELECT datallowconn FROM pg_database WHERE datname = 'plax_base'
   ```

2. If the base exists, open a pool to it, read the provenance row (version, created_at).

3. Check for `plax_base_next`: `SELECT 1 FROM pg_database WHERE datname = 'plax_base_next'`.

4. Return `BaseInfo`.

---

## CLI specification

### `plax base`

`plax base` is the CLI namespace for base database operations. Each subcommand
maps to a `BaseManager` method.

```
plax base create   Create an empty base (migrated, no seed data)
plax base seed     Run the seed command into the base
plax base reset    Drop and recreate the base (migrated only)
plax base refresh  Staged refresh via base_next swap
plax base status   Print base health and provenance info
```

All subcommands:
- Require `plax.json` in the repo root (fail with exit 1 if missing).
- Build the Postgres connection string and pass it to `NewBaseManager`:
  user and password come from the blueprint's logical postgres service `env`
  block (`POSTGRES_USER`, `POSTGRES_PASSWORD`); host and port default to
  `localhost:5432` — a day-one convention, since logical services declare no
  ports (validation rule V6). `--pg-url <dsn>` overrides the entire string.
- Pass the resolved repo root to `NewBaseManager` as `repoRoot`. Use
  `--root <path>` flag to override (defaults to `.`).

Phase 4 also lists `plax base refresh` as a deliverable. The split: Phase 2
ships the `plax base` CLI as the driver's test harness; Phase 4 layers the
config-stamp side effect (registry update) on top of the same command. This
plan is authoritative for the command's behavior.

#### `plax base create`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional) |
| Exit 0 | Base created successfully (or already exists and locked) |
| Exit 1 | Base exists but unlocked, migration command fails, Postgres unreachable |
| Stderr | Progress: "creating plax_base...", "running migrations...", "locking..." |
| Stdout | Nothing on success (success is silent) |
| Idempotent | Yes — no-op if base exists and is locked |

#### `plax base seed`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional) |
| Exit 0 | Seed command completed successfully |
| Exit 1 | Base does not exist, seed command fails |
| Stderr | Progress + ⚠ warning: "SeedBase is not safe while instances exist; use 'plax base refresh' for ongoing updates" |
| Stdout | Nothing on success |
| Idempotent | No — running seed twice may duplicate data depending on the seed script |

#### `plax base reset`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional) |
| Exit 0 | Base dropped and recreated (migrated, empty) |
| Exit 1 | Migration command fails, Postgres unreachable |
| Stderr | "resetting plax_base...", "running migrations..." |
| Stdout | Nothing |
| Destructive | Yes — drops all instance databases that were cloned from this base. Caller must ensure no instances are active. |

#### `plax base refresh`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional) |
| Exit 0 | Refresh completed, new base active |
| Exit 1 | Migration or seed fails, old base still active |
| Exit 2 | Refresh succeeded but swap is deferred (`base_next` exists, old base still active) |
| Stderr | Progress + if exit 2: "plax_base_next is ready but could not swap. Run 'plax base refresh' again when instances are idle." |
| Stdout | Nothing |

#### `plax base status`

| Aspect | Value |
|---|---|
| Flags | `--root <path>` (optional, default `.`), `--pg-url <dsn>` (optional), `--json` |
| Exit 0 | Always (status is informative) |
| Stderr | Nothing in default mode |
| Stdout | Table (default) or JSON (`--json`) |

**Table output (default):**

```
plax_base:
  Exists:          yes
  Locked:          yes
  Provenance:      v3 (seeded 2025-07-15 14:32:01)
  Base next:       no
```

**JSON output (`--json`):**

```json
{
  "exists": true,
  "locked": true,
  "provenance_version": 3,
  "created_at": "2025-07-15T14:32:01Z",
  "has_base_next": false
}
```

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| Postgres unreachable at construction | `pgxpool.New` returns error | Return wrapped error: "postgres: connect: <err>". Exit 1. |
| Blueprint has no logical postgres service | `bp.Services` iteration finds none | Return error: "no logical postgres service in blueprint". Exit 1. |
| `CREATE DATABASE plax_base` fails (e.g. already exists, permissions) | Postgres error | If "already exists" + unlocked → "base exists but is not locked — run 'plax base reset'". Otherwise wrap and return. |
| Migration command exits non-zero | `exec.Cmd.CombinedOutput` → exit code != 0 | Return error: "migrate: command failed: <stderr output>". Drop the partially created database. |
| Seed command exits non-zero | Same detection | Return error: "seed: command failed: <stderr output>". (For RefreshBase, drop `base_next`. For SeedBase, re-lock and return error.) |
| `TEMPLATE` clone fails — source has active connections | Postgres error 55006 | Return error: "clone: base has active connections — is it locked?" |
| `TEMPLATE` clone fails — source doesn't exist | Postgres error "database does not exist" | Return error: "base database does not exist — run 'plax base create'" |
| `DROP DATABASE` fails during refresh swap | Postgres error "cannot drop the currently open database" or "being accessed by other users" | Retry with exponential backoff (see RefreshBase algorithm). If all retries exhausted → deferred swap, exit 2. |
| Docker daemon unreachable | `client.NewClientWithOpts` or first API call fails | Return error: "docker: cannot connect to daemon. Is Docker running?" |
| Docker image pull fails | `ImagePull` returns error | Return wrapped error: "docker: pull <image>: <err>" |
| Docker container name already exists | `ContainerCreate` returns conflict error | Remove existing container, retry once. If retry fails, return error. |
| Docker network already exists | `NetworkCreate` on existing name | No-op — Docker returns the existing network. Not an error. |
| Port already bound on the host | `ContainerStart` fails with "port is already allocated" | Return wrapped error: "docker: port <port> already in use by <container_name_or_pid>" |
| Base exists but not locked (corrupt state) | `CreateBase` detects it | Return error: "base exists but is not locked — run 'plax base reset' to recreate" |
| `base_next` exists from a previous interrupted refresh | `RefreshBase` detects it | Return error: "'plax_base_next' exists — run 'plax base reset' to clean up" |
| Schema hash directory not found | `ComputeSchemaHash`: `os.Stat` returns `ErrNotExist` | Return empty string (valid — not all repos use file-based migrations). No error. |
| Migration command executable not found | `exec.LookPath` fails | Return error: "migrate: <cmd> not found on PATH" |

---

## Tests

### Test prerequisites

Postgres integration tests require a running Postgres instance. Tests accept
a `PLAX_TEST_POSTGRES_URL` environment variable (default:
`postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable`).
Tests are skipped if `PLAX_TEST_POSTGRES_URL` is empty or connection fails.

Docker integration tests require a running Docker daemon. Tests are skipped
if `DOCKER_HOST` is unset and `/var/run/docker.sock` is absent. CI skips
both integration suites (no Postgres, no Docker in standard GitHub Actions).

Consequence: the derivation engine ships with no CI coverage in this phase.
The acceptance criteria below are only considered met when the full
integration suite has been run locally against real Postgres and Docker —
the implementer runs it before merge and pastes the output into the PR.

### Test fixtures

```
pkg/derive/postgres/testdata/
  migrations/
    001_create_users.sql        # minimal: CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT);
  seed.sh                       # minimal: INSERT INTO users (name) VALUES ('seed-user');
```

These are minimal fixtures used for testing the driver logic — not the actual
sample repo's migrations. They live in the `postgres` package so the tests are
self-contained.

The test blueprint is constructed in Go (not a JSON file):

```go
func testBlueprint() *blueprint.Blueprint {
    return &blueprint.Blueprint{
        Seed: blueprint.SeedConfig{
            Migrate: "psql -f testdata/migrations/001_create_users.sql",
            Command: "bash testdata/seed.sh",
            Workdir: ".",
        },
    }
}
```

### Unit tests

**`pkg/derive/postgres/basemanager_test.go`** (integration — requires Postgres):

- `TestCreateBase_Success` — `CreateBase` on fresh Postgres → base exists, is locked, has provenance v1
- `TestCreateBase_Idempotent` — second call is no-op, no error
- `TestCreateBase_ExistsUnlocked` — create base, manually unlock it, call `CreateBase` → error "exists but is not locked"
- `TestSeedBase_Success` — `CreateBase` → `SeedBase` → provenance version incremented, data present
- `TestSeedBase_NoBase` — `SeedBase` without `CreateBase` → error "does not exist"
- `TestSeedBase_CommandFails` — seed command with invalid SQL → error, base re-locked
- `TestResetBase_Success` — `CreateBase` → `SeedBase` → `ResetBase` → base exists with v1, no seed data
- `TestResetBase_NoBase` — `ResetBase` with no base → creates it (delegates to `CreateBase`)
- `TestCloneBase_Success` — `CreateBase` → `CloneBase("test_clone")` → clone exists, has provenance, source="test_clone"
- `TestCloneBase_BaseNotLocked` — manually unlock base → `CloneBase` → error "not locked"
- `TestCloneBase_AlreadyExists` — `CloneBase("dup")` twice → second call error "already exists"
- `TestCloneBase_NoBase` — `CloneBase` without base → error "does not exist"
- `TestRefreshBase_Success` — `CreateBase` → `SeedBase` → `RefreshBase` → version incremented, no `base_next` left
- `TestRefreshBase_NoBase` — `RefreshBase` with no base → delegates to `CreateBase`, returns
- `TestRefreshBase_BaseNextExists` — create `plax_base_next`, then `RefreshBase` → error "'base_next' exists"
- `TestRefreshBase_SeedFails` — `CreateBase` → `RefreshBase` with failing seed command → error, old base intact, `base_next` dropped
- `TestDropInstanceDB_Success` — `CloneBase("drop_test")` → `DropInstanceDB("drop_test")` → database gone
- `TestDropInstanceDB_NoDB` — `DropInstanceDB("nonexistent")` → no-op, no error
- `TestDropInstanceDB_ActiveConnections` — open a connection to a clone from another goroutine → `DropInstanceDB` → succeeds (connections terminated)
- `TestBaseStatus_Exists` — `CreateBase` → `BaseStatus` → `Exists=true, Locked=true, ProvenanceVer=1`
- `TestBaseStatus_NotExists` — `BaseStatus` on empty Postgres → `Exists=false`
- `TestBaseStatus_BaseNext` — create `plax_base_next` → `BaseStatus` → `HasBaseNext=true`

**`pkg/derive/postgres/provenance_test.go`** (integration — requires Postgres):

- `TestCreateProvenance_RoundTrip` — create, then `ReadProvenance` → rows match
- `TestCreateProvenance_Idempotent` — call twice → second call updates the single row (no duplicate)
- `TestReadProvenance_NoTable` — read on fresh database → nil, nil
- `TestComputeSchemaHash_Success` — directory with migration files → returns hex string
- `TestComputeSchemaHash_EmptyDir` — empty directory → empty string
- `TestComputeSchemaHash_NoDir` — nonexistent directory → empty string, no error

**`pkg/derive/docker/driver_test.go`** (integration — requires Docker):

- `TestRunService_Success` — `RunService` with nginx → container running, returns container ID
- `TestRunService_PortBinding` — `RunService` with port 80 mapped → `curl localhost:<port>` returns nginx welcome
- `TestRunService_VolumePersistence` — write file in container volume → stop → remove container → start new container with same volume → file exists
- `TestRunService_NetworkConnectivity` — two containers on same network → can ping by service name
- `TestStopService_Success` — `RunService` → `StopService` → container stopped
- `TestStopService_NotRunning` — `StopService` on nonexistent container → no-op (Docker returns error, driver swallows it)
- `TestRemoveService_Success` — stopped container → `RemoveService` → container gone
- `TestRemoveVolume_Success` — `RemoveVolume` → volume gone
- `TestRemoveVolume_NotExists` — `RemoveVolume` on nonexistent → no-op
- `TestCreateNetwork_Idempotent` — `CreateNetwork` twice → no error
- `TestRemoveNetwork_Success` — `RemoveNetwork` → network gone

### Integration test

`TestEndToEnd_RefreshWhileCloning` — Simulates concurrent `CloneBase` and `RefreshBase`:
1. `CreateBase` → `SeedBase`
2. Start goroutine: loop `CloneBase("clone_N")` 10 times with 50ms sleeps between each
3. While clones are running, call `RefreshBase`
4. RefreshBase retries the DROP during swap, eventually succeeds (clones finish)
5. Verify: all clones have the pre-refresh provenance version, base has post-refresh version

This test verifies the retry logic works in practice.

---

## Acceptance criteria

- [x] `plax base create` creates `plax_base`, runs migrations, stamps provenance v1, locks the base
- [x] `plax base create` is idempotent — second call in same repo exits 0 immediately
- [x] `plax base seed` runs the seed command, increments provenance version
- [x] `plax base seed` prints a warning to stderr about using `refresh` for ongoing updates
- [x] `plax base reset` drops and recreates the base (migrated, empty, v1)
- [x] `plax base refresh` creates `base_next`, seeds it, swaps it, leaves no `base_next` behind
- [x] `plax base refresh` with a failing seed command leaves the old base intact and drops `base_next`
- [x] `plax base status` prints a table with exists/locked/provenance/base_next fields
- [x] `plax base status --json` prints valid JSON matching `BaseInfo`
- [x] `CloneBase` succeeds on a locked base, fails on an unlocked base
- [x] `CloneBase` copies the provenance row and updates the `source` field
- [x] `DropInstanceDB` terminates active connections before dropping
- [x] `RunService` starts a container with the correct image, ports, env, volumes, and network
- [x] `StopService` stops a running container within 10s
- [x] `RemoveService` force-removes a container
- [x] `RemoveVolume` removes a named volume
- [x] `CreateNetwork` / `RemoveNetwork` create and destroy per-instance bridge networks
- [x] `go vet ./...` passes
- [x] `go test -race ./...` passes (unit tests; integration tests skipped without Postgres/Docker)
- [x] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| kong | `github.com/alecthomas/kong` | v1.16.0 | CLI framework (wired in Phase 3, declared here for completeness) |
| pgx | `github.com/jackc/pgx/v5` | v5.7.6 | Postgres driver (connection pool, query execution) |
| docker | `github.com/docker/docker/v28` | v28.0.0 | Docker Engine API client |
| go-cmp | `github.com/google/go-cmp/cmp` | latest | Deep equality in tests |

Standard library:

- `context` — cancellation propagation
- `crypto/sha256` — schema hash computation
- `database/sql` — not used (pgx is the direct driver)
- `encoding/json` — provenance row marshal/unmarshal
- `os/exec` — running migration and seed commands
- `os` — file I/O, `filepath.Join`, `os.Stat` for migration detection
- `testing` — test framework

Existing project dependencies (no change):

- `github.com/apollopower/plax/pkg/blueprint` — reads `SeedConfig`, `ServiceDef`
- `github.com/apollopower/plax/pkg/registry` — not used by Phase 2 directly (Phase 3 wires it)

---

## Backward compatibility note

Phase 2 adds a `migrate` field to `blueprint.SeedConfig`. This is a backward-
compatible JSON change: existing `plax.json` files that omit `migrate` will
fail validation (V11 extended) with a clear error message. `plax init` must
be updated in the same change to emit the `migrate` field. The golden file,
validation rules (V11), and init logic should be updated as a Phase 1 follow-
up before Phase 2 implementation begins.
