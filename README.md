# Plax

Run many copies of your repo's development environment at once. Each copy
gets its own ports, database, and services. Built for coding agents working
in parallel.

---

Git worktrees give each agent its own checkout of the source. That is not
enough. Two agents running the app still bind port 3000 and still write to
the same database. Plax isolates the parts that run.

**Status — early development.** The blueprint schema, registry, and `plax init` parser are being built first. See the phases below for what comes next.

---

## How it works

One file — `plax.json` — checked into your repo declares everything an
instance needs: services, isolation strategy, env template with per-instance
holes. `plax up <name>` materializes a new instance from that declaration
in seconds, `plax down <name>` throws it away. Nothing drifts.

Blueprint example (for a Next.js + Postgres + Redis repo):

```json
{
  "version": 1,
  "name": "eai",
  "port_pool": { "start": 3000, "end": 4000 },
  "seed": {
    "command": "bun run db fixtures",
    "workdir": "."
  },
  "services": {
    "db": {
      "isolation": "logical",
      "type": "postgres",
      "image": "ankane/pgvector:v0.5.0",
      "env": { "POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "postgres" }
    },
    "redis": {
      "isolation": "dedicated",
      "image": "redis:7.2",
      "ports": { "6379": { "var": "REDIS_PORT" } }
    },
    "gotenberg": {
      "isolation": "dedicated",
      "image": "gotenberg/gotenberg:8",
      "command": ["gotenberg", "--api-port=3000", "--api-timeout=90s"],
      "ports": { "3000": { "var": "GOTENBERG_PORT" } }
    }
  },
  "processes": [
    {
      "name": "app",
      "isolation": "native",
      "command": "bun run dev:app",
      "workdir": ".",
      "port_var": "PORT",
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
      "DATABASE_URL": "postgres://postgres:postgres@localhost:5432/{{DB_NAME}}",
      "WORKER_REDIS_URL": "redis://localhost:{{REDIS_PORT}}/0",
      "NEXTAUTH_URL": "http://localhost:{{PORT}}"
    }
  }
}
```

`plax init` scaffolds this skeleton from your `docker-compose.yml` and
`.env.example`. You verify the isolation strategies and fill in the holes.

---

## Planned commands

| Command | What it does |
|---|---|
| `plax init` | Scaffold a blueprint from the repo |
| `plax up <name>` | Build an instance from the blueprint |
| `plax down <name>` | Destroy it |
| `plax ls` | List instances |
| `plax status <name>` | Drift report (code, schema, data, host, config) |
| `plax suspend <name>` | Stop processes and containers, keep state |
| `plax resume <name>` | Start again, print drift report |
| `plax exec <name> -- <cmd>` | Run a command inside an instance |
| `plax attach <name>` | Open a shell wired to the instance |
| `plax doctor` | Check blueprint, registry, and machine |
| `plax base seed` | Seed the reference database |
| `plax base reset` | Reset the reference database (schema only) |
| `plax base refresh` | Re-seed through staged swap |
| `plax rederive` | Regenerate .env files after blueprint changes |
| `plax send <name>` | Write a message to an instance |
| `plax recv <name>` | Read and remove messages |

---

## Build

```sh
go build -o plax ./cmd/plax
```

Requires Go 1.26+.

---

## Documentation

- [Design doc](docs/design.md) — architecture, isolation strategies, derivation engine
- [Implementation plans](docs/plans/index.md) — phased roadmap with detailed specs

---

## License

MIT
