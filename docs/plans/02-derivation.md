# Phase 2 — Derivation Engine

The core of Plax. This phase builds the drivers that create, clone, seed,
reset, and refresh resource instances. Two drivers day one: Postgres
(logical) and Docker (dedicated).

---

## 2.1 Postgres driver — base lifecycle

Package: `pkg/derive/postgres/`

### `BaseManager`

Connects to a shared Postgres server (same `POSTGRES_USER`/`POSTGRES_PASSWORD`
as the blueprint declares; read from the host's running postgres container or
a local socket).

State machine:

```
absent → empty (schema only) → seeded (schema + fixtures) → locked (template)
                                                                  ↓
                                                            cloned for instances
```

**`CreateBase()`** — Creates a database named `plax_base` (or blueprint's
configured base name).

1. `CREATE DATABASE plax_base`
2. Run migrations: `bun run db migrate` (in worktree root pointed at
   `plax_base`)
3. Stamp provenance: create `_plax_provenance` table, insert version=1 row
4. Set `ALTER DATABASE plax_base WITH ALLOW_CONNECTIONS false`
5. Base is now in "empty" state — schema present, no data, no connections

**`SeedBase()`** — Runs the repo's seed command against the base.

1. Temporarily allow connections: `ALTER DATABASE plax_base WITH
   ALLOW_CONNECTIONS true`
2. Run `bun run db fixtures` (from blueprint's `seed_command`)
3. Re-lock: `ALTER DATABASE plax_base WITH ALLOW_CONNECTIONS false`
4. Increment provenance version

**`ResetBase()`** — Drops and recreates the base with schema only.

1. Drop `plax_base` if exists (disconnect all first)
2. `CREATE DATABASE plax_base`
3. Run migrations
4. Stamp provenance version=1
5. Lock

**`CloneBase(target_db string) error`** — Template-clone for a new instance.

1. Assert base is locked (refuses connections)
2. `CREATE DATABASE <target_db> TEMPLATE plax_base`
3. Target inherits provenance table with base_version stamp

**`RefreshBase()`** — Staged refresh via `base_next` swap (avoids race with
ongoing clones).

1. `CREATE DATABASE plax_base_next TEMPLATE plax_base`
   (This copies the current base including its schema and data.)
2. Temporarily allow connections on `plax_base_next`
3. Run `bun run db fixtures` against `plax_base_next`
4. Increment provenance on `plax_base_next`
5. Lock `plax_base_next`
6. Rename: drop `plax_base` (if no clones are in flight — see note below),
   rename `plax_base_next` to `plax_base`
7. Drop the old `plax_base`

Note on step 6: `DROP DATABASE` fails if any clone is in progress. If it
fails, retry after a short backoff. Clone operations take ~100ms, so this
window is small. If it consistently fails, the refresh marks `plax_base_next`
for deferred swap and notifies via `plax status`.

**`DropInstanceDB(name string)`** — `DROP DATABASE IF EXISTS <name>`

### Provenance table

Created inside every database (base and every clone):

```sql
CREATE TABLE _plax_provenance (
  version       INTEGER NOT NULL,
  source        TEXT NOT NULL DEFAULT 'base',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  seed_command  TEXT,
  schema_hash   TEXT          -- optional: hash of migration filenames
);
```

Single-row table. Every clone inherits it. `plax status` compares the
clone's `version` against the current base's `version` to detect data drift.

## 2.2 Docker driver — dedicated services

Package: `pkg/derive/docker/`

Uses the Docker SDK to manage containers for services with `isolation:
"dedicated"`.

**`RunService(ctx, service *blueprint.ServiceDef, ports map[string]int) (containerID string, error)`**

1. Determine container name: `plax-<instance>-<service>` (e.g.
   `plax-i1-redis`)
2. Pull image if not present (`imagePullPolicy: "missing"`)
3. Configure port mapping: map host port (from allocator) to container port
4. Set environment variables from blueprint's `env` map
5. Configure volumes with per-instance names:
   `plax-<instance>-<service>-<volume>` (e.g. `plax-i1-redis-data`)
6. Set restart policy: `"no"` (Plax manages lifecycle)
7. `docker.ContainerCreate` + `docker.ContainerStart`
8. Return container ID

**`StopService(containerID string)`** — `docker.ContainerStop` with 10s timeout

**`RemoveService(containerID string)`** — `docker.ContainerRemove` with force

**`RemoveVolume(volumeName string)`** — `docker.VolumeRemove`

## 2.3 Docker network

All dedicated containers for one instance share a Docker network:

`plax-<instance>-net`.

Created on `up`, removed on `down`. Containers discover each other by
service name. Not strictly necessary for localhost-mapped ports, but good
practice and prepares for `shared` services that talk internally.

## 2.4 Base Docker service

The blueprint's Postgres service (the shared one that runs the base) is
managed separately: it's the single Postgres container that hosts all
logical databases. `plax base seed` and `plax base reset` assume it's
already running (e.g. from `docker compose up -d` or the user's existing
setup).

A future `plax doctor` check verifies it's reachable.

---

## Deliverables

- [ ] Postgres driver: `CreateBase`, `SeedBase`, `ResetBase`, `CloneBase`,
      `RefreshBase`, `DropInstanceDB`
- [ ] Provenance table creation and stamping
- [ ] Staged refresh with `base_next` swap
- [ ] Docker driver: `RunService`, `StopService`, `RemoveService`,
      `RemoveVolume`
- [ ] Docker network create/remove per instance
- [ ] `plax base` subcommands: `seed`, `reset`, `refresh`, `status`
