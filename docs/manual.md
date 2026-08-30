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

Isolation earns its cost when agents write. Instances that mutate state —
running migrations, changing data, exercising write paths — must not share a
database, so they each get one. For read-only fan-out (running the same test
suite against the same data), isolation is pure overhead. "One instance per
agent" is the wrong heuristic; "one instance per mutator" is closer.

## 2. Installation

**Homebrew (macOS and Linux):**

```sh
brew tap apollopower/plax
brew install plax
```

**Pre-built binaries** — download from the
[latest release](https://github.com/apollopower/plax/releases/latest) and
put the binary somewhere in your `$PATH`.

**Go install:**

```sh
go install github.com/apollopower/plax/cmd/plax@latest
```

**From source** (requires Go 1.26+):

```sh
go build -o plax ./cmd/plax
```

### 2.1 Upgrading

`plax upgrade` updates the binary to the latest release, using the path
that matches how it was installed:

- **Homebrew** — runs `brew upgrade plax`
- **go install** — runs `go install github.com/apollopower/plax/cmd/plax@latest`
- **Direct binary** — downloads the release archive for your platform,
  verifies its checksum, and replaces the binary atomically (a running
  `plax` is replaced safely)

`plax upgrade --check` reports the current and latest versions and the
command an upgrade would run, without changing anything. Exit codes:
`0` current, `1` outdated, `2` version lookup failed. A binary built from
source reports its version as `dev`; `plax upgrade` refuses to update it
unless `--force` is passed.

You also need:

- **Docker** — every instance gets its own Docker network, and `dedicated`
  services run as per-instance containers.
- **Postgres** — for logical isolation, a reachable Postgres server on
  `localhost:5432` (or wherever `--pg-url` points). Plax derives the DSN
  from the blueprint's logical Postgres service; `--pg-url` overrides it.

## 3. Getting started

The five-minute path from a fresh repo to a running instance. The narrative
uses a Next.js + Postgres + Redis app called `myapp`; your repo will differ
in detail but not in shape. Every command runs from the repo root.

### 3.1 Install plax

Install the binary ([Installation](#2-installation)) and verify it runs:

```sh
plax --help
```

Coding agents should start from `plax guide` instead — it prints the
complete operational reference (lifecycle states, drift model,
verification, mailbox IPC) as one markdown document, bundled with the
binary and independent of any repo.

### 3.2 Scaffold the blueprint

Every repo that uses plax needs a `plax.json` at its root. Generate one:

```sh
plax init > plax.json
```

`plax init` parses `docker-compose.yml` and `.env.example` and emits a
skeleton blueprint on stdout — one entry per service, every env variable
with its example value, every compose port mapped to a variable. Notices
and warnings go to stderr, so the redirect only captures the JSON. It also
appends `.plax/` to `.gitignore`, so instance worktrees are not traversed
by root-globbing tooling, and warns about tooling configs (tsconfig, jest,
eslint, …) that glob from the repo root and would traverse every instance.

The skeleton is a starting point, not a finished contract. It cannot know
your migration command, your seed command, or which services need
per-instance state. Fill in the gaps before going further:

- **seed.migrate / seed.command / seed.workdir** — how schema and fixtures
  are loaded into the base
- **processes** — the commands that start your app and workers
- **isolation** — whether each service is `logical`, `dedicated`, `shared`,
  `external`, or `native`
- **env.holes** — which variables get per-instance values

### 3.3 Inspect plax.json

Open `plax.json` and read it. Every field is documented in
[The blueprint](#4-the-blueprint); the ones that matter right now:

```json
{
  "version": 1,
  "name": "myapp",
  "port_pool": { "start": 3000, "end": 4000 },
  "seed": {
    "migrate": "bun run db migrate",
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
    }
  },
  "processes": [
    { "name": "app", "isolation": "native", "command": "bun run dev:app",
      "workdir": ".", "port_var": "PORT", "default_port": 3000 }
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

The `services` block declares what runs in every instance: `db` is
`logical` (one Postgres database per instance, cloned from the base), and
`redis` is `dedicated` (one container and port per instance). The
`env.holes` block is what makes instances differ: `{{DB_NAME}}`, `{{PORT}}`,
and `{{REDIS_PORT}}` are substituted with per-instance values when `.env`
is derived.

### 3.4 Prepare the base

Instances are cloned from a shared base database, so it must exist first.
If your blueprint has a logical Postgres service (as above), run:

```sh
plax base create
plax base seed
```

`base create` creates `plax_base`, applies `seed.migrate`, stamps it with
provenance, and locks it. `base seed` then loads fixture data with
`seed.command`. If your Postgres is not on `localhost:5432`, pass `--pg-url`
to both commands. Repos without a logical Postgres service can skip this
section. The base is covered in depth in [The base](#5-the-base).

### 3.5 Start an instance

```sh
plax up i1
```

One command does the whole lifecycle:

```
creating branch and worktree...
creating network plax-i1-net...
allocating ports...
deriving .env...
cloning database plax_i1...
starting redis...
starting app...

instance i1 up
  worktree:  .plax/worktrees/i1
  branch:    plax/i1
  database:  plax_i1 (psql -d plax_i1)
  ports:     PORT=3301 REDIS_PORT=26380
  logs:      .plax/logs/i1/
  scratch:   .plax/worktrees/i1/scratch/

verification: 6 check(s) passed
```

Your app is now running in `.plax/worktrees/i1`, on its own ports, against
its own database. Nothing about it can collide with the primary checkout —
that is the point. `plax up` verifies what it started before reporting
success; the check count varies with the blueprint.

### 3.6 Work inside the instance

```sh
plax ls
plax status i1
plax exec i1 -- bun run test
plax attach i1
```

- `plax ls` — list all instances, their state, and live health.
- `plax status i1` — the six-dimension drift report: code, schema, data,
  host, config, and live health.
- `plax exec i1 -- <cmd>` — run one command in the instance's worktree with
  its `.env` loaded, non-interactively.
- `plax attach i1` — an interactive shell in the same context. This is
  where you live while working in the instance.

### 3.7 Add a second instance

Parallelism is the feature, so create another:

```sh
plax up i2
```

`i2` gets its own branch, worktree, ports, container, and database
(`plax_i2`). Both instances run at once, writing to separate state. This is
how multiple agents work the same repo without colliding.

### 3.8 Tear down

```sh
plax down i1
```

Kills the processes, removes the container and network, drops the database,
and deletes the worktree and branch. Every step tolerates missing resources,
so teardown works even when Docker or Postgres is down.

That is the whole loop: `init` once, then `up`, work, `down` as often as you
like. The rest of this manual documents the surface in full.

## 4. The blueprint

Every repo that uses plax needs a `plax.json` at its root. This file is the
contract between the repo and the tool. It says what an instance needs;
the tool reads it and does what it says.

### 4.1 Scaffold

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
  `external`, or `native`
- **env.holes** — which variables get per-instance values

### 4.2 The blueprint

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

### 4.3 Blueprint fields

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
- `applied_migrations` — optional `{ "table", "column" }` pointing at the
  framework's migration-tracking table (e.g. `pgmigrations`). When set,
  schema drift reads the migrations actually applied to the instance
  database, so drift correctly clears after an in-worktree migration.
  When absent, plax falls back to comparing the clone-time migration-file
  hash. Repos whose migration runner does not track applied migrations in
  a table (plain `psql -f file.sql`, for example) have no tracking table
  and should leave this unset.

**services** — Map of service name to definition. Each service declares:

- `isolation` — one of `logical`, `dedicated`, `shared`, `external`, `native`
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
  Keys that exist only in your `.env` (neither template nor holes) are
  dropped entirely — a legitimate way to keep local secrets local.
  `plax doctor` warns about these so the difference is visible. Use
  scrub to block dangerous secrets (API keys, credentials) from
  propagating into instances. `plax verify` checks that scrubbed keys
  do not leak through, including under a different key name.

### 4.4 Isolation strategies

**logical** — One server, one database per instance. Postgres clones via
`CREATE DATABASE ... TEMPLATE`. Cheap (about 100 ms). All instances share
the same host port; only the database name varies. Requires a driver
(currently only Postgres).

**dedicated** — One container per instance. Each gets its own allocated
port, its own Docker container on a per-instance network. Good for
services that cannot separate tenants (Redis, Gotenberg).

**shared** — Accepted in the schema but not yet implemented. A `shared`
service will be silently skipped during `plax up`. Use `dedicated`
instead.

**external** — Accepted in the schema but not yet implemented. Same
behavior as `shared` today: silently skipped. For dependencies that
cannot be cloned (SaaS blob storage, third-party APIs).

**native** — Not a container at all. The process runs directly on the
host, in the instance's worktree, with the derived `.env` loaded.
Used for the app's own dev server and workers.

### 4.5 Secrets

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

## 5. The base

The base is a reference database that instances are cloned from. It lives
on your Postgres server as `plax_base`. It exists to be copied.

### 5.1 Creating the base

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

### 5.2 Seeding the base

```sh
plax base seed --pg-url "postgres://..."
```

Runs `seed.command` against the base, then locks it again. Do this once
after `base reset`, and again whenever fixtures change.

### 5.3 Refreshing the base

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

### 5.4 Checking the base

```sh
plax base status --pg-url "postgres://..."
```

Prints whether the base exists, whether it is locked, its provenance
version, and whether a `plax_base_next` is staging.

## 6. Working with instances

### 6.1 Create

```sh
plax up myfeature --pg-url "postgres://..."
```

Start from a specific branch, PR, tag, or commit with `--ref`:

```sh
plax up --ref feature/fixes myfeature
plax up --ref pr/42 myfeature
plax up --ref 42 myfeature       # bare integer also treated as PR number
plax up --ref abc1234 myfeature
```

The base must exist before the first `up` (see [The base](#5-the-base)).
This does everything:

1. Creates branch `plax/myfeature` and worktree `.plax/worktrees/myfeature`
2. Creates Docker network `plax-myfeature-net`
3. Allocates ports from the pool
4. Derives `.env` from template + your `.env` + hole values
5. Clones `plax_base` to `plax_myfeature`
6. Applies pending migrations in the worktree with the instance's derived
   environment — the base's applied set is not assumed current, so the
   instance never boots on a stale schema
7. Starts dedicated containers (Redis, Gotenberg, etc.)
8. Starts native processes (app, workers)
9. Records everything in the registry

If any step fails, all side effects are rolled back in reverse order.

### 6.1.1 Skipping steps

```sh
plax up --skip migrate myfeature   # skip the migration step
plax up --skip verify myfeature    # skip the explicit verification phase
plax up --skip migrate,verify myfeature
```

`--skip` accepts exactly `migrate` and `verify`, comma-separated or as
repeated flags; unknown or empty names fail before any side effect.

- `--skip migrate` leaves the instance on the cloned base schema — use it
  when the base is known current and migrations are slow.
- `--skip verify` still runs the immediate settle check (a workload that
  exits right after start fails `up`); it only omits the later verification
  phase and says so on stderr.

When the blueprint configures `seed.applied_migrations`, the migration step
reports the measured number of newly applied identifiers (per database when
more than one); without it, the step reports completion without a count —
plax never parses a migration tool's stdout.

### 6.2 List

```sh
plax ls
```

```
NAME       STATE      BRANCH               MAIL  PORTS                    HEALTH     CREATED
myfeature  running    plax/myfeature       0     3000 3001                healthy    2m ago
otherwork  suspended  plax/otherwork       3     3002 3003                unhealthy  1h ago
```

For running instances the `HEALTH` column is a live probe of process
liveness and port reachability at call time — not a stored snapshot. A
suspended instance shows its last stored value (`—` if never verified).
Use `--json` for structured output.

### 6.3 Check health

```sh
plax status myfeature --pg-url "postgres://..."
```

Six dimensions:

- **code** — commits ahead/behind the base branch
- **schema** — whether migration files match the database. With
  `seed.applied_migrations` configured, this compares the migrations
  actually applied to the instance's database against the files at the
  worktree's HEAD, so an in-worktree migration clears the drift. Without
  it, plax falls back to comparing the clone-time migration-file hash
- **data** — whether the instance was built from the current base version
- **host** — whether tool versions have changed since creation
- **config** — whether blueprint inputs (compose, env template, toolchain)
  have changed since last `plax up`
- **health** — a live probe, computed at call time: for a running instance,
  plax dials every allocated port (dedicated services and process
  `port_var`s) and checks every native process is alive. A suspended
  instance reports `unknown` — there is no runtime to probe

Each dimension is `ok`, `drift`, or `unknown`. Reading a report never
writes to the registry.

### 6.4 Suspend and resume

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

### 6.5 Run commands inside an instance

```sh
plax exec myfeature -- bun run test
plax attach myfeature
```

`exec` runs a single command with the instance's environment loaded and
the worktree as the working directory. `attach` opens an interactive shell
in the same context. `attach` prints drift warnings to stderr if the
instance has moved, and reminds you about unread mailbox messages.

### 6.6 Verify an instance

```sh
plax verify myfeature --pg-url "postgres://..."
```

Runs a battery of checks and updates the instance's health state in the
registry:

- **env-completeness** — every key in the template and user `.env` is
  present in the derived `.env`
- **env-unresolved-holes** — no `{{VAR}}` placeholders remain unsubstituted
- **env-scrubbed-leaks** — compares parsed values: no scrubbed key's real
  value appears in the derived `.env`, even under a different key name
- **dependency-isolation** — the worktree shares the parent's `node_modules`
  only if the dependency manifests (package.json, lockfiles) match; a
  manifest mismatch means the instance runs the parent's dependencies,
  not this branch's
- **tcp-reachability** — every allocated port has a listener (dedicated
  services and process `port_var`s)
- **process-liveness** — every declared native process is alive
- **db-existence** — every declared database exists
- **db-provenance** — every database has its provenance table (catches
  externally dropped-and-recreated databases)

The `check` field in `--json` output is the stable identifier for scripting.

Verification also runs automatically on `plax up` and `plax resume`, before
either reports success. The static env checks run first and fail fast: a
broken `.env` rolls the whole `up` back, leaving nothing behind. After the
workloads settle, process liveness and the database checks run. The TCP
probe is deliberately left out of `up`/`resume` — a freshly started app
legitimately takes seconds to bind, and `up` must not block on readiness.
Run `plax verify` for the full probe, including ports.

On a runtime-check failure the instance stays up for debugging and is
marked `unhealthy`; the exit code is 1. A suspended instance skips runtime
checks with a note. With Postgres unreachable, DB checks are skipped with a
note.

`--json` is supported. Use `--pg-url` to override the DSN.

### 6.7 Destroy

```sh
plax down myfeature --pg-url "postgres://..."
```

Kills processes, removes containers, drops the database, removes the
network, removes the worktree, deletes the branch, removes the registry
entry, removes the mailbox. Every step tolerates missing resources — a
stopped Docker daemon or dead Postgres will not prevent teardown of
everything else.

### 6.8 Regenerate .env files

```sh
plax rederive
```

After changing `plax.json` or `.env.example`, this re-derives the `.env`
file for every registered instance. Non-hole values from both the instance's
existing `.env` and the user's `.env` are preserved; hole values are
recomputed from the current blueprint and registry. Prints a diff per
instance and a reminder to restart.

### 6.9 Inspect the instance database

The cloned database is the investigation surface for anything an instance
does to Postgres. `plax up` prints it in the summary with a ready-to-use
hint:

```
instance i1 up
  worktree:  .plax/worktrees/i1
  branch:    plax/i1
  database:  plax_i1 (psql -d plax_i1)
  ports:     PORT=3301 REDIS_PORT=26380
  logs:      .plax/logs/i1/
  scratch:   .plax/worktrees/i1/scratch/
```

The instance database is structurally accurate — same schema and data as
the base at clone time. It is statistically naive: query plans depend on
`ANALYZE` statistics, table and index sizes, and data volume, none of which
an instance guarantees. Plax does not run `ANALYZE` on clones; a fresh clone
inherits the base's statistics. Diagnose plan-dependent, volume-dependent,
or statistics-dependent queries against production, not the instance.

## 7. Mailbox

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

Use the mailbox for routine, low-stakes coordination. Direct agent-to-agent
handoff propagates errors as efficiently as findings: a claim travels as
fast as a bug report. Route high-stakes claims — anything that needs
adjudicating against ground truth agents cannot reach — through the hub
(the user), who can weigh evidence the instances cannot see.

`ls` shows unread count and health. `attach` prints a notice if messages
are waiting.

## 8. Doctor

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

## 9. Output conventions

Records go to stdout. Human chatter goes to stderr. A pipe never has to
strip out a banner.

```sh
plax ls | awk '$2 == "suspended" { print $1 }' | xargs -n1 plax down
```

Commands that support `--json`: `ls`, `status`, `verify`, `doctor`, `send`,
`recv`, `base status`.

## 10. Files and directories

Plax stores its state inside the repo under `.plax/`:

```
.plax/
  registry.json          instance records, blueprint stamp
  worktrees/<name>/      git worktrees (each with a scratch/ directory)
  logs/<name>/           process logs (app.log, workers.log, ...)
  mail/<name>/           mailbox directories
```

The registry is a single JSON file. It records: branch, worktree path,
allocated ports, database name, container IDs, process group IDs,
creation time, provenance (base version, toolchain hash, tool versions),
state (`running` or `suspended`), and the last verification outcome
(health, verified-at). Writes are serialized with a file lock.

### 10.1 Ignore entries

Instance worktrees live inside the repo at `.plax/worktrees/<name>`. Any
tool that globs from the repo root therefore traverses every instance —
discovering N copies of the codebase. `plax init` adds `.plax/` to your
`.gitignore` automatically, but you must ignore the worktree directory in
each root-globbing tool yourself, otherwise a test run, typecheck, or
format-write from the primary checkout silently operates on every instance.

The complete ignore list:

- **Version control** — `.gitignore`: `.plax/`
- **Type checker** — `tsconfig.json`: add `.plax` to the `"exclude"` array
- **Test runner** — jest, vitest, or Playwright config: add
  `.plax/worktrees/**` to the ignore list (`testPathIgnorePatterns`,
  `exclude`, or `testIgnore` respectively)
- **Linter/formatter** — ESLint or Prettier config: add `.plax/**` to the
  ignore list

`plax init` prints a warning naming any detected config file that does not
already reference `.plax`.

## 11. Environment variables

- `PLAX_INSTANCE` — read by `send` as the default `--from` value.
  Set it yourself (e.g., in your shell profile or tmux config) if you
  want `send` to know which instance is writing.
- `SHELL` — used by `attach` to find your shell

## 12. Known limitations

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
