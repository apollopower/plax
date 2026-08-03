# Phase 1 — Blueprint, Registry, and Init

The goal of Phase 1 is the data layer every other phase depends on: the
blueprint schema (what Plax reads), the registry (what Plax writes), and
`plax init` (the bridge from your repo to the blueprint). No instances run
yet.

---

## 1.1 Go module layout

```
cmd/plax/main.go              # CLI entry point, wires subcommands
pkg/
  blueprint/
    blueprint.go              # Blueprint struct + JSON tags
    validate.go               # Validation rules
    init.go                   # plax init: compose + .env.example → blueprint
  registry/
    registry.go               # Registry struct, Open/Close/Save
    types.go                  # Instance record, port allocation
  portpool/
    pool.go                   # PortAllocator: next free port in range
```

## 1.2 Blueprint Go types

Define the full blueprint schema as Go structs with JSON tags. All fields are
documented in the type system rather than comments.

Key types: `Blueprint`, `ServiceDef`, `ProcessDef`, `EnvHoles`, `ServiceIsolation`,
`ProcessIsolation`.

`ServiceIsolation` is a typed string enum: `"logical"`, `"dedicated"`,
`"shared"`, `"external"`, `"native"`.

Validation rules in `validate.go`:
- Services must have unique names
- `logical` services must declare `type` (only `"postgres"` supported day one)
- Port vars must be unique across services and processes
- Holes must not collide with env vars that carry static secrets
- `port_pool` range must be valid (start < end, > 1024)

## 1.3 `plax init` — compose parser

`plax init` is an offline command with no dependencies beyond the filesystem.

Inputs (read from repo root):
- `docker-compose.yml` — parses services, images, ports, env, volumes, commands
- `.env.example` — parses variables, comments (explanatory), default values

Algorithm:
1. Parse compose YAML — extract each service: image, ports (with `${VAR}` or
   numeric), environment (static vars), volumes, command
2. For each service, emit a `ServiceDef`:
   - If it has `volumes:` → marks it as stateful (seeds `isolation` as
     `"logical"` for postgres, `"dedicated"` otherwise)
   - If no volumes → marks it as stateless (seeds `"shared"`, but always
     emits a warning: "confirm isolation strategy for <service>")
   - Ports with `${VAR}` → map to `"var"` field in blueprint
   - Ports with bare numbers → suggest a var name
3. Parse `.env.example` — for each variable whose value matches
   `localhost:\d+` or contains `localhost:`, flag it as a likely hole
4. Emit JSON to stdout (or `> plax.json`)

The output is a skeleton, not a final blueprint. The only field the user must
manually verify is each service's `isolation` strategy. Everything else is
mechanically derived.

## 1.4 Registry

A JSON file at `<repo-root>/.plax/registry.json`.

```json
{
  "version": 1,
  "blueprint_stamp": {
    "compose_hash": "sha256:a3f1...",
    "env_example_hash": "sha256:9c2d...",
    "toolchain_hash": "sha256:4b7e..."
  },
  "instances": {
    "i1": {
      "id": "i1",
      "branch": "plax/feature-auth",
      "worktree_path": ".plax/worktrees/i1",
      "created_at": "2026-08-03T10:00:00Z",
      "state": "running",
      "ports": {
        "APP_PORT": 3001,
        "PGPORT": 5433,
        "REDIS_PORT": 6380,
        "GOTENBERG_PORT": 3031
      },
      "db_name": "plax_i1",
      "container_ids": {
        "redis": "abc123...",
        "gotenberg": "def456..."
      },
      "pids": {
        "app": 12345,
        "workers": 12346
      },
      "provenance": {
        "base_version": 3,
        "toolchain": "node@22.19.0 bun@1.3.11"
      }
    }
  },
  "port_allocations": {
    "3001": { "instance": "i1", "service": "app" },
    "5433": { "instance": "i1", "service": "db" },
    "6380": { "instance": "i1", "service": "redis" },
    "3031": { "instance": "i1", "service": "gotenberg" }
  }
}
```

API:
- `Open(path) *Registry` — reads file, or returns empty if not found
- `(*Registry).Save() error` — atomically writes to JSON
- `(*Registry).AddInstance(id, record) error`
- `(*Registry).RemoveInstance(id)`
- `(*Registry).GetInstance(id) *InstanceRecord`

## 1.5 Port allocator

`PortAllocator` wraps the registry's port allocations.

- `Allocate(service string) (port int, error)` — scans `[start, end]`, checks
  each port against (a) registry allocations and (b) `net.Listen("tcp",
  "127.0.0.1:PORT")` probe (actually free on the OS). First free port wins.
- `Release(port int)` — removes from allocations map
- `Reserve(name, service string, port int)` — for resume: re-claim a specific
  port if still free; error if taken by another pid

The TCP probe prevents silent collision with non-Plax processes.

---

## Deliverables

- [ ] Go module with `kong` CLI wired to `plax init` subcommand
- [ ] Blueprint struct + JSON schema
- [ ] `plax init` parses eai's compose + `.env.example`, emits valid blueprint
- [ ] Registry: read/write `.plax/registry.json`
- [ ] Port allocator with OS-level availability check
- [ ] `.plax/.gitignore` with `*` (keeps worktrees + registry out of git)
