# Plax Manual

A guide for engineers who own and operate plax.

---

## 1. What plax does

Plax provisions parallel, isolated development environments from one repo.
Each environment — an instance — gets its own worktree, its own ports, its
own database, its own containers, and its own processes. They do not collide.

The unit of work is the instance. You create one with `plax up`, work
inside it, and destroy it with `plax down`. In between you can suspend it,
resume it, check its health, verify its integrity, send it a message, or
run a command inside it. That is the whole lifecycle.

Plax manages environments. It does not manage agents, windows, panes, or
sessions. Where you put the terminal is your business.

## 2. Installation

```sh
go build -o plax ./cmd/plax
```

Copy the binary somewhere in your `$PATH`. Requires Go 1.26+ and Docker.
For Postgres logical isolation, a reachable Postgres server on
`localhost:5432` (or wherever `--pg-url` points).

## 3. First setup

Every repo that uses plax needs a `plax.json` at its root. This file is the
contract between the repo and the tool. It says what an instance needs;
the tool reads it and does what it says.

### 3.1 Scaffold

```sh
plax init > plax.json
```

This parses `docker-compose.yml` and `.env.example`, then emits a skeleton:
every service present, `dedicated` isolation by default, every env variable
present with its example value, every compose port mapped to a variable.

The skeleton is intentionally incomplete. You must fill in:

- **processes** — which commands start your app and workers
- **seed.command** — how to load fixture data into the base
- **seed.migrate** — how to run schema migrations
- **isolation** — whether each service is `logical`, `dedicated`, `shared`,
  or `native`
- **env.holes** — which variables get per-instance values

### 3.2 The blueprint

A complete `plax.json` for a Next.js + Postgres + Redis repo:

```json
{
  "version": 1,
  "name": "myapp",
  "port_pool": { "start": 3000, "end": 4000 },
  "toolchain": ".tool-versions",
    "seed": {
    "migrate": "bun run db migrate",
    "command": "bun run db fixtures",
    "workdir": ".",
    "migrations_dir": "src/db/migrations"
  },
  "services": {
    "db": {
      "isolation": "logical",
      "type": "postgres",
      "image": "ankane/pgvector:v0.5.0",
      "env": { "POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "postgres" },
      "databases": [{ "name": "test", "from": "base" }]
    },
    "redis": {
      "isolation": "dedicated",
      "image": "redis:7.2",
      "ports": { "6379": { "var": "REDIS_PORT", "default": "6379" } }
    },
    "gotenberg": {
      "isolation": "dedicated",
      "image": "gotenberg/gotenberg:8",
      "command": ["gotenberg", "--api-port=3000", "--api-timeout=90s"],
      "ports": { "3000": { "var": "GOTENBERG_PORT", "default": "3030" } }
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
    },
    "scrub": ["OPENAI_API_KEY", "STRIPE_SECRET_KEY"]
  }
}
```

### 3.3 Blueprint fields

**version** — Always `1`.

**name** — Project name. Used in output and defaults.

**port_pool** — Range of ports available for allocation. Instances draw
from this range. Choose a range that does not collide with anything else
on your machine.

**toolchain** — Path to the toolchain file (`.tool-versions`, `mise.toml`,
etc.). Plax reads it to record which tool versions existed when an instance
was created and to detect drift later.

**seed** — How to build the base database.

- `migrate` — command that applies schema migrations
- `command` — command that loads fixture data
- `workdir` — directory to run both commands in
- `migrations_dir` — where migration files live (default `src/db/migrations`); used
  by drift detection to compare applied migrations against the files on disk

**services** — Map of service name to definition. Each service declares:

- `isolation` — one of `logical`, `dedicated`, `shared`, `native`
- `type` — driver type (only `postgres` exists today; required for
  `logical`)
- `image` — Docker image (required for `dedicated`)
- `env` — environment variables passed to the container
- `ports` — map of container port to `{ "var": "ENV_VAR_NAME" }`.
  Optionally include `"default"` to pre-fill the template.
- `command` — override the image's default command
- `databases` — additional databases to clone on `up`, for logical
  Postgres services only. Each entry has a `name` (the key used in
  template variables, e.g. `"test"` gives `{{DB_NAME_test}}`) and `from`
  (the clone origin — currently only `"base"` is supported). Each named
  database is cloned, migrated, and dropped with the instance.

**processes** — Array of native processes to spawn. Each declares:

- `name` — identifier
- `isolation` — always `native`
- `command` — shell command to run (supports `{{VAR}}` templating)
- `workdir` — relative to the worktree root
- `port_var` — env var that receives this process's allocated port
- `default_port` — preferred port (used if available)
- `depends_on` — other processes that must start first

**env** — How `.env` is derived.

- `template` — path to `.env.example` (or equivalent)
- `holes` — map of env var name to template string. Template strings
  use `{{VAR}}` placeholders. Available variables: `DB_NAME` (primary
  database), `DB_NAME_<key>` (for each declared database, e.g.
  `DB_NAME_test`), plus one per allocated port var (e.g., `PORT`,
  `REDIS_PORT`, `GOTENBERG_PORT`).
- `scrub` — list of env var keys to strip from derived `.env` files.
  These must be present in the template or declared as holes. Use this
  to block dangerous secrets (API keys, credentials) from propagating
  into instances. `plax verify` checks that scrubbed keys do not leak
  through.

### 3.4 Isolation strategies

**logical** — One server, one database per instance. Postgres clones via
`CREATE DATABASE ... TEMPLATE`. Cheap (about 100 ms). All instances share
the same host port; only the database name varies. Requires a driver
(currently only Postgres).

**dedicated** — One container per instance. Each gets its own allocated
port, its own Docker container on a per-instance network. Good for
services that cannot separate tenants (Redis, Gotenberg).

**shared** — Accepted in the schema but not yet implemented. A `shared`
service will be silently skipped during `plax up`. Use `dedicated` or
`external` instead.

**external** — Accepted in the schema but not yet implemented. Same
behavior as `shared` today: silently skipped. For dependencies that
cannot be cloned (SaaS blob storage, third-party APIs).

**native** — Not a container at all. The process runs directly on the
host, in the instance's worktree, with the derived `.env` loaded.
Used for the app's own dev server and workers.

### 3.5 Secrets

Secrets are not holes. API keys, OAuth credentials, and tokens are the
same across every instance — they vary per machine, not per instance.

Derivation merges three sources in order:

1. **Hole keys** — get per-instance values (database name, ports, URLs)
2. **User's `.env`** — real secrets override the template (gitignored,
   never committed)
3. **Template defaults** — everything else falls through

This means `.env.example` can keep placeholder values. Your own `.env`
in the repo root supplies the real secrets. Each instance's `.env` is
written into its worktree.

## 4. The base

The base is a reference database that instances are cloned from. It lives
on your Postgres server as `plax_base`. It exists to be copied.

### 4.1 Creating the base

```sh
plax base create --pg-url "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
```

Creates `plax_base`, runs migrations, stamps provenance (v1), and locks it
(`ALLOW_CONNECTIONS false`). Nothing ever connects to the base directly.
It exists to be copied.

If the base already exists and is locked, `create` is a no-op.

To start over:

```sh
plax base reset --pg-url "postgres://..."
```

Drops both `plax_base` and any staged `plax_base_next`, then creates a
fresh base. Also the recovery path for an interrupted refresh.

If `--pg-url` is omitted, plax derives the DSN from the blueprint's
logical Postgres service definition.

### 4.2 Seeding the base

```sh
plax base seed --pg-url "postgres://..."
```

Runs `seed.command` against the base, then locks it again. Do this once
after `base reset`, and again whenever fixtures change.

### 4.3 Refreshing the base

```sh
plax base refresh --pg-url "postgres://..."
```

Creates a fresh `plax_base_next` from scratch (empty, migrated, seeded,
locked), then swaps it into place via rename. Instance creation never
waits on a refresh and never sees a partial base.

If instances are connected to the old base, the swap is deferred:
the new base stays staged as `plax_base_next` and the command exits
with code 2. Re-run `plax base refresh` after the instances are down
to complete the swap.

If a previous refresh was interrupted mid-staging, `refresh` detects
the incomplete `plax_base_next` and tells you to run `plax base reset`
to clean up.

### 4.4 Checking the base

```sh
plax base status --pg-url "postgres://..."
```

Prints whether the base exists, whether it is locked, its provenance
version, and whether a `plax_base_next` is staging.

## 5. Working with instances

### 5.1 Create

```sh
plax up myfeature --pg-url "postgres://..."
```

Start from a specific branch, PR, tag, or commit with `--ref`:

```sh
plax up --ref feature/fixes myfeature
plax up --ref pr/42 myfeature
plax up --ref abc1234 myfeature
```

This does everything:

1. Creates branch `plax/myfeature` and worktree `.plax/worktrees/myfeature`
2. Creates Docker network `plax-myfeature-net`
3. Allocates ports from the pool
4. Derives `.env` from template + your `.env` + hole values
5. Clones `plax_base` to `plax_myfeature`
6. Starts dedicated containers (Redis, Gotenberg, etc.)
7. Starts native processes (app, workers)
8. Records everything in the registry

If any step fails, all side effects are rolled back in reverse order.

### 5.2 List

```sh
plax ls
```

```
NAME       STATE      BRANCH               MAIL  PORTS                    HEALTH     CREATED
myfeature  running    plax/myfeature       0     3000 3001                healthy    2m ago
otherwork  suspended  plax/otherwork       3     3002 3003                unhealthy  1h ago
```

Use `--json` for structured output.

### 5.3 Check health

```sh
plax status myfeature --pg-url "postgres://..."
```

Six dimensions:

- **code** — commits ahead/behind the base branch
- **schema** — whether migration files match the database
- **data** — whether the instance was built from the current base version
- **host** — whether tool versions have changed since creation
- **config** — whether blueprint inputs (compose, env template, toolchain)
  have changed since last `plax up`
- **health** — whether the last `plax verify` run passed all checks

Each dimension is `ok`, `drift`, or `unknown`.

### 5.4 Suspend and resume

```sh
plax suspend myfeature
plax resume myfeature
```

Suspend stops all processes and containers. The worktree, database, and
registry entry are preserved. Ports are kept by record but not bound —
anything on the machine can take them while the instance sleeps.

Resume starts everything again and prints the drift report. If a port
the instance held is now taken by something else, resume fails and names
the offender. Free the port and retry, or `down` and `up` to reallocate.

### 5.5 Run commands inside an instance

```sh
plax exec myfeature -- bun run test
plax attach myfeature
```

`exec` runs a single command with the instance's environment loaded and
the worktree as the working directory. `attach` opens an interactive shell
in the same context. Both print drift warnings to stderr if the instance
has moved.

### 5.6 Verify an instance

```sh
plax verify myfeature --pg-url "postgres://..."
```

Runs a battery of checks and updates the instance's health state in the
registry:

- **env-completeness** — every key in the template and user `.env` is
  present in the derived `.env`
- **env-unresolved-holes** — no `{{VAR}}` placeholders remain unsubstituted
- **env-scrubbed-leaks** — no value from the user's `.env` leaks into the
  derived `.env` for keys listed in `env.scrub`
- **tcp** — every allocated port has a listener
- **process** — every declared native process is alive
- **db** — the instance database is reachable

On `plax up`, verification runs automatically before the command reports
success. On failure, the instance stays up for debugging and is marked
`unhealthy`. A suspended instance skips runtime checks.

`--json` is supported. Use `--pg-url` to override the DSN.

### 5.7 Destroy

```sh
plax down myfeature --pg-url "postgres://..."
```

Kills processes, removes containers, drops the database, removes the
network, removes the worktree, deletes the branch, removes the registry
entry, removes the mailbox. Every step tolerates missing resources — a
stopped Docker daemon or dead Postgres will not prevent teardown of
everything else.

### 5.8 Regenerate .env files

```sh
plax rederive
```

After changing `plax.json` or `.env.example`, this re-derives the `.env`
file for every registered instance. Non-hole values from both the instance's
existing `.env` and the user's `.env` are preserved; hole values are
recomputed from the current blueprint and registry. Prints a diff per
instance and a reminder to restart.

## 6. Mailbox

Instances can send each other messages. The mailbox is a directory:
`.plax/mail/<name>/`. `send` writes a JSON file; `recv` reads and removes.
No daemon, no locking, no broker.

```sh
plax send otherwork --from myfeature "schema changed, rebase before resume"
plax recv otherwork
plax recv otherwork --all
plax recv otherwork --count 3
```

Messages survive suspend. Delivery is buffered, not a rendezvous — a
message to a suspended instance waits on disk until the receiver wakes.

`ls` shows unread count and health. `attach` prints a notice if messages
are waiting.

## 7. Doctor

```sh
plax doctor --pg-url "postgres://..."
```

Four areas:

- **blueprint-vs-repo** — does the blueprint parse and validate? Are all
  declared services in the compose file? Have blueprint inputs (compose,
  env template, toolchain) changed since last `plax up`?
- **blueprint-vs-registry** — do all live instances have their worktree,
  branch, database, and containers? Are any allocated ports held by
  unknown instances or undeclared services?
- **repo-vs-machine** — does the machine satisfy the toolchain file?
  Is Docker reachable? Is Postgres reachable?
- **base** — does the base exist? Is it locked? What is its provenance?
  Is a staged `plax_base_next` waiting?

Each check reports `ok`, `warn`, or `fail`. Exit code is 0 if no check
fails, 1 if any do. Use `--json` for structured output.

## 8. Output conventions

Records go to stdout. Human chatter goes to stderr. A pipe never has to
strip out a banner.

```sh
plax ls | awk '$2 == "suspended" { print $1 }' | xargs -n1 plax down
```

Commands that support `--json`: `ls`, `status`, `verify`, `doctor`, `send`,
`recv`, `base status`.

## 9. Files and directories

Plax stores its state inside the repo under `.plax/`:

```
.plax/
  registry.json          instance records, blueprint stamp
  worktrees/<name>/      git worktrees
  logs/<name>/           process logs (app.log, workers.log, ...)
  mail/<name>/           mailbox directories
```

The registry is a single JSON file. It records: branch, worktree path,
allocated ports, database name, container IDs, process group IDs,
creation time, provenance (base version, toolchain hash, tool versions),
and state (`running` or `suspended`).

Add `.plax/` to your `.gitignore`.

## 10. Environment variables

- `PLAX_INSTANCE` — read by `send` as the default `--from` value.
  Set it yourself (e.g., in your shell profile or tmux config) if you
  want `send` to know which instance is writing.
- `SHELL` — used by `attach` to find your shell

## 11. Known limitations

- **Postgres only.** Logical isolation requires a driver. Only Postgres
  has one. Redis and other services use `dedicated`.
- **`.env` only.** Derivation templates one file. A port in
  `vite.config.ts` or a database name in `config/database.yml` is out
  of reach.
- **Linux/macOS.** Process management uses process groups (`setpgid`).
  Windows is not supported.
- **No archive.** There is no way to freeze an instance's state into a
  named blob. If state is worth keeping, promote it into the base or
  into a fixture.
- **One repo, one registry.** Two concurrent `plax up` commands on the
  same repo can race on the registry file. This is a known issue
  planned for Phase 8.
