# Plax — Implementation Plan

Run many copies of one repo's development environment at once, on your own
machine. Each copy gets its own ports, its own database, its own services.
Built for coding agents working in parallel.

---

## Target repo

**Empwr.ai (eai)** — `~/Work/repos/eai`

- Next.js (Pages Router) + tRPC + React + Tailwind + Postgres + Redis + BullMQ + Gotenberg
- Toolchain: Node 22.19.0, Bun 1.3.11 (`.tool-versions`)
- Docker services: `db` (pgvector Postgres), `redis` (7.2), `gotenberg` (document conversion)
- Dev start: `bun run dev` → `next dev` + `workers`
- Database seeded via `bun run db fixtures`

---

## Blueprint format

JSON, checked into the repo root as `plax.json`. No comments.

```jsonc
{
  "version": 1,
  "name": "eai",
  "port_pool": { "start": 3000, "end": 4000 },
  "toolchain": ".tool-versions",
  "services": {
    "db": {
      "isolation": "logical",
      "type": "postgres",
      "image": "ankane/pgvector:v0.5.0",
      "env": { "POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "postgres" },
      "ports": { "5432": { "var": "PGPORT" } }
    },
    "redis": {
      "isolation": "dedicated",
      "image": "redis:7.2",
      "ports": { "6379": { "var": "REDIS_PORT" } }
    },
    "gotenberg": {
      "isolation": "dedicated",
      "image": "gotenberg/gotenberg:8",
      "command": ["gotenberg", "--api-port=3000",
                   "--api-timeout=90s", "--libreoffice-restart-after=10"],
      "ports": { "3000": { "var": "GOTENBERG_PORT" } }
    }
  },
  "processes": [
    {
      "name": "app",
      "isolation": "native",
      "command": "bun run dev:app",
      "workdir": ".",
      "port_var": "APP_PORT",
      "default_port": 3000
    },
    {
      "name": "workers",
      "isolation": "native",
      "command": "bun run dev:workers",
      "workdir": ".",
      "depends_on": ["app"]
    }
  ],
  "env": {
    "template": ".env.example",
    "holes": {
      "DATABASE_URL": "postgres://postgres:postgres@localhost:{{PGPORT}}/{{DB_NAME}}",
      "DATABASE_TEST_URL": "postgres://postgres:postgres@localhost:{{PGPORT}}/{{DB_NAME}}_test",
      "WORKER_REDIS_URL": "redis://localhost:{{REDIS_PORT}}/0",
      "WORKER_REDIS_TEST_URL": "redis://localhost:{{REDIS_PORT}}/1",
      "CACHE_REDIS_URL": "redis://localhost:{{REDIS_PORT}}/2",
      "CACHE_REDIS_TEST_URL": "redis://localhost:{{REDIS_PORT}}/3",
      "NEXTAUTH_URL": "http://localhost:{{APP_PORT}}",
      "NEXT_PUBLIC_SITE_URL": "http://localhost:{{APP_PORT}}",
      "GOTENBERG_URL": "http://localhost:{{GOTENBERG_PORT}}",
      "MCP_ISSUER_URL": "http://localhost:{{APP_PORT}}"
    }
  }
}
```

---

## Design decisions

| Question | Decision |
|---|---|
| Blueprint format | JSON (no comments) |
| Registry location | Per-repo, `.plax/registry.json` |
| Worktree management | `git worktree add` directly |
| Branch strategy | `plax/<name>` off current HEAD |
| Postgres isolation | Logical (template-clone `CREATE DATABASE ... TEMPLATE`) |
| Redis isolation | Dedicated (one container per instance) |
| Gotenberg isolation | Dedicated (one container per instance) |
| App processes | Native (run in worktree with derived `.env`) |
| Base seed | `bun run db fixtures` for eai; configurable per repo |
| Base reset | Drop and recreate base DB with schema only, no data |

---

## Phases

| # | File | What it delivers |
|---|---|---|
| 1 | [`01-blueprint.md`](01-blueprint.md) | Blueprint schema, `plax init` parser, registry, port allocator |
| 2 | [`02-derivation.md`](02-derivation.md) | Postgres driver (base, clone, refresh), Docker driver, volume management |
| 3 | [`03-lifecycle.md`](03-lifecycle.md) | `up/down/ls/attach/exec` — first runnable instances |
| 4 | [`04-state.md`](04-state.md) | `suspend/resume/status/doctor/rederive/base` — drift detection, state management |
| 5 | [`05-ipc.md`](05-ipc.md) | `send/recv` — instance mailbox for agent IPC |

---

## Key Go dependencies (tentative)

| Need | Likely library |
|---|---|
| CLI framework | `alecthomas/kong` |
| Docker SDK | `docker/docker/client` |
| Postgres driver | `pgx` |
| YAML parsing | `goccy/go-yaml` (for compose input to `init`) |
