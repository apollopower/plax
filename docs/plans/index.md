# Plax — Implementation Plan

Run many copies of one repo's development environment at once, on your own
machine. Each copy gets its own ports, its own database, its own services.
Built for coding agents working in parallel.

---

## Target repo

**Example repo** — `~/Work/repos/example`

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
  "name": "sample",
  "port_pool": { "start": 3000, "end": 4000 },
  "toolchain": ".tool-versions",
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
      "DATABASE_TEST_URL": "postgres://postgres:postgres@localhost:5432/{{DB_NAME}}_test",
      "WORKER_REDIS_URL": "redis://localhost:{{REDIS_PORT}}/0",
      "WORKER_REDIS_TEST_URL": "redis://localhost:{{REDIS_PORT}}/1",
      "CACHE_REDIS_URL": "redis://localhost:{{REDIS_PORT}}/2",
      "CACHE_REDIS_TEST_URL": "redis://localhost:{{REDIS_PORT}}/3",
      "NEXTAUTH_URL": "http://localhost:{{PORT}}",
      "NEXT_PUBLIC_SITE_URL": "http://localhost:{{PORT}}",
      "GOTENBERG_URL": "http://localhost:{{GOTENBERG_PORT}}",
      "MCP_ISSUER_URL": "http://localhost:{{PORT}}"
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
| Base seed | `bun run db fixtures` for the sample; configurable per repo |
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
| 6 | [`06-correctness.md`](06-correctness.md) | Fix user-facing correctness bugs: init name, compose ports, env override, SQL injection, doctor exit code, isolation validation |
| 7 | [`07-robustness.md`](07-robustness.md) | Harden against failure modes: process survivors, mailbox atomicity, Docker errdefs, DSN parsing, pin validation, registry versioning |
| 8 | [`08-architecture.md`](08-architecture.md) | CSP concurrency, registry file locking, thin CLI shell, atomic env writes |
| 9 | [`09-polish.md`](09-polish.md) | Doc comments, dead code removal, stderr layering, test hygiene, naming conventions |
| 10 | [`10-status-worktree-head.md`](10-status-worktree-head.md) | Fix status drift: measure worktree HEAD instead of recorded branch |
| 11 | [`11-multi-database.md`](11-multi-database.md) | Multiple databases per logical Postgres service: clone, migrate, drop, and detect orphans |
| 12 | [`12-up-ref-flag.md`](12-up-ref-flag.md) | `plax up --ref` flag: build instances from branches, PRs, tags, or commits |
| 13 | [`13-user-env-keys-not-dropped.md`](13-user-env-keys-not-dropped.md) | User `.env` keys absent from template are no longer dropped; `doctor` warns about them |
| 14 | [`14-env-scrub.md`](14-env-scrub.md) | Blueprint `env.scrub` blocks dangerous secrets from propagating from user `.env` into instances |
| 15 | [`15-verify.md`](15-verify.md) | Instance verification on `up`/`resume`/`rederive` + `plax verify` subcommand — env, TCP, process, DB checks, and base seed row count |
| 16 | [`16-health-live.md`](16-health-live.md) | `ls`/`status` run live health checks (incl. process ports) instead of reading stale `rec.Health` |
| 17 | [`17-schema-drift-live.md`](17-schema-drift-live.md) | Schema drift reads live applied migrations from the instance DB instead of a frozen clone-time stamp |
| 18 | [`18-isolation-hardening.md`](18-isolation-hardening.md) | `scratch/` per worktree (ignored, reported, removed) + `dependency-isolation` check for shared-tree manifest mismatch |
| 19 | [`19-upgrade.md`](19-upgrade.md) | `plax upgrade`: install-method detection (brew / go install / direct), atomic binary replacement |
| 22 | [`22-work-records.md`](22-work-records.md) | Durable per-instance work records and stacked instance lineage |

---

## Triage

Periodic issue-triage snapshots live in [`triage/`](triage/), named by date
(`2026-08-14.md`). These are not implementation plans — they catalogue the
active issue landscape at a point in time and propose a remediation roadmap.

---

## Plan file conventions

Every phase plan must contain these sections in order:

| # | Section | Purpose |
|---|---|---|
| 1 | **Objective** | One sentence — what this phase delivers |
| 2 | **Package layout** | File tree with one-line purpose per file |
| 3 | **Type specifications** | Every Go struct with JSON tags, field docs, and validation rules |
| 4 | **Algorithms** | Step-by-step pseudocode. Edge cases called out inline with `⚠` markers |
| 5 | **CLI specification** | Exact command syntax, flags, args, exit codes, stdout/stderr behavior |
| 6 | **Error handling** | Table of failure modes → expected behavior (error message, exit code, rollback) |
| 7 | **Tests** | Unit tests (named functions + what they verify), integration tests (full scenario), test fixtures needed |
| 8 | **Acceptance criteria** | Verifiable checklist of outcomes. Items must be specific ("running `X` produces output matching `Y`") |
| 9 | **Dependencies** | Go imports with exact module paths, pinned to known versions |

Rules:

- Use `⚠` to mark edge cases inside algorithms
- Use `→` to show cause → effect in error tables
- Struct fields list their Go type and JSON tag inline: `Name string \`json:"name"\``
- Test entries name specific functions: `TestBluePrintValidation_LogicalServiceMustHaveType`
- Acceptance criteria are imperative: "`plax init` in a repo root produces valid JSON on stdout, exits 0"

---

## Key Go dependencies (tentative)

| Need | Likely library |
|---|---|
| CLI framework | `alecthomas/kong` |
| Docker SDK | `docker/docker/client` |
| Postgres driver | `pgx` |
| YAML parsing | `goccy/go-yaml` (for compose input to `init`) |
