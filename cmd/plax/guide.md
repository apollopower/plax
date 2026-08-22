# Plax Guide

Complete operational reference for plax, for coding agents.

This document is bundled with the binary and printed by `plax guide`. It is
static: it describes plax's behavior for this exact version of the binary.
The other source of truth is the repo's `plax.json` (the blueprint), which
declares what an instance of *this* repo needs. Read both, and you have
everything needed to use plax correctly.

---

## What plax does

Plax provisions parallel, isolated development environments from one repo.
Each environment — an **instance** — gets its own git worktree, its own
ports, its own database, its own containers, and its own processes. Nothing
about one instance can collide with another or with the primary checkout.

Plax manages environments. It does not orchestrate agents, and it ships no
UI. Agents use it over the CLI, exactly as a human would.

The unit of work is the instance: `plax up <name>` creates one, work
happens inside it, `plax down <name>` destroys it. In between you can
suspend, resume, list, check drift, verify, run commands, and pass
messages between instances.

Isolation costs money and time. Instances that mutate state (migrations,
data changes, write paths) each need their own database. Read-only work
(same tests, same data) does not. "One instance per agent" is wrong;
"one instance per mutator" is closer.

## The blueprint

Every repo that uses plax has a `plax.json` at its root — the contract
between the repo and the tool. It declares what an instance needs; plax
does what it says. Read the repo's `plax.json` before doing anything else;
it tells you which services, processes, and env variables exist.

Key fields:

- `port_pool` — the port range instances draw from.
- `toolchain` — path to the toolchain file (`.tool-versions`, `mise.toml`,
  ...). Plax records tool versions at instance creation and detects drift
  later.
- `seed` — how the base database is built:
  - `migrate` — command that applies schema migrations
  - `command` — command that loads fixture data
  - `workdir` — directory to run both in
  - `migrations_dir` — where migration files live (default
    `src/db/migrations`)
  - `applied_migrations` — optional `{ "table", "column" }` pointing at the
    migration-tracking table. When set, schema drift compares the
    migrations actually applied to the instance DB against the files on
    disk; when unset, plax compares a clone-time migration-file hash.
- `services` — map of service name to definition. Per service:
  - `isolation` — `logical`, `dedicated`, `shared`, `external`, or `native`
  - `type` — driver type (only `postgres` exists today; required for
    `logical`)
  - `image` — Docker image (required for `dedicated`)
  - `env` — env variables passed to the container
  - `ports` — map of container port to `{ "var": "ENV_VAR_NAME" }`
  - `command` — override the image's default command
  - `databases` — additional databases cloned on `up` (logical Postgres
    only); each has a `name` (giving the template variable
    `{{DB_NAME_<name>}}`) and `from` (only `"base"` supported)
- `processes` — native processes spawned in the worktree:
  - `name`, `command` (supports `{{VAR}}` templating), `workdir`
  - `port_var` — env var receiving the allocated port
  - `default_port` — preferred port when available
  - `depends_on` — processes that must start first
- `env` — how each instance's `.env` is derived:
  - `template` — path to `.env.example`
  - `holes` — map of env var to template string with `{{VAR}}`
    placeholders. Available variables: `DB_NAME` (primary database),
    `DB_NAME_<key>` (per declared database), plus one per allocated port
    var (e.g. `PORT`, `REDIS_PORT`).
  - `scrub` — keys whose real values must never propagate into instances.
    Scrub API keys, OAuth credentials, tokens.

`plax.json` is plain JSON, no comments. `plax init` scaffolds a skeleton
from `docker-compose.yml` and `.env.example`; the skeleton is a starting
point and must be completed (seed commands, processes, isolation,
holes).

Secrets are not holes: API keys and tokens vary per machine, not per
instance. Derivation merges three sources in order — hole values, then the
user's `.env` (real secrets, gitignored), then template defaults. An
instance's derived `.env` lives in its worktree.

## Isolation strategies

- **logical** — one Postgres server, one database per instance, cloned via
  `CREATE DATABASE ... TEMPLATE` (fast, ~100 ms). All instances share the
  host port; only the database name varies. Requires a driver (Postgres).
- **dedicated** — one container per instance on a per-instance network,
  with its own allocated port. For services that cannot separate tenants
  (Redis, Gotenberg).
- **shared** — accepted in the schema, not implemented. Silently skipped
  during `plax up`.
- **external** — same as `shared` today: silently skipped. For
  dependencies that cannot be cloned (SaaS, third-party APIs).
- **native** — not a container: the process runs on the host inside the
  instance's worktree with the derived `.env` loaded. Used for the app's
  dev server and workers.

## The base

Logical Postgres instances are cloned from a shared reference database,
`plax_base`. The base is built once and copied; nothing connects to it
directly.

- `plax base create` — create `plax_base`, apply `seed.migrate`, stamp
  provenance, lock it. No-op if already created and locked.
- `plax base seed` — run `seed.command` against the base. Do this once
  after `create`/`reset`, and again whenever fixtures change.
- `plax base reset` — drop and recreate the base (schema only, no data).
  Also the recovery path for an interrupted refresh.
- `plax base refresh` — staged refresh: build `plax_base_next` from
  scratch, then swap it in via rename. If instances are still connected to
  the old base, the swap is deferred and the command exits with code 2;
  re-run after the instances are down to complete the swap.
- `plax base status` — whether the base exists, is locked, its provenance
  version, and whether a refresh is staged.

`base create`/`seed`/`reset`/`refresh` require `seed.migrate`,
`seed.command`, and `seed.workdir` in the blueprint. Omit `--pg-url` and
plax derives the DSN from the blueprint's logical Postgres service;
otherwise pass `--pg-url`.

The base must exist before the first `plax up`.

## Command reference

All commands take `--root <dir>` (default `.`, auto-discovered by walking
up to the `plax.json`). Commands that touch Postgres take `--pg-url`.
`plax --version` prints the binary version. `plax --help` prints command
usage.

| Command | What it does |
|---|---|
| `plax init` | Scaffold a `plax.json` skeleton from `docker-compose.yml` and `.env.example` (stdout) |
| `plax base create/seed/reset/refresh/status` | Manage the base database (see above) |
| `plax up <name>` | Create and start an instance (the whole lifecycle) |
| `plax down <name>` | Destroy an instance: processes, containers, DB, network, worktree, branch, registry entry, mailbox |
| `plax ls` | List instances with state, live health, unread mail, ports |
| `plax attach <name>` | Open an interactive shell in the instance's environment |
| `plax exec <name> -- <cmd>` | Run one command in the instance's environment |
| `plax suspend <name>` | Stop workloads, keep all state |
| `plax resume <name>` | Start a suspended instance again, print drift report |
| `plax status <name>` | Six-dimension drift report (read-only) |
| `plax verify <name>` | Run the full verification battery, update health state |
| `plax doctor` | Validate blueprint, registry, machine, and base health |
| `plax rederive` | Regenerate `.env` files for all instances from the current blueprint |
| `plax send <name>` | Write a message to an instance's mailbox |
| `plax recv <name>` | Read and remove messages (`--all`, `--count N`) |

## Lifecycle states

Every instance is in exactly one state, recorded in the registry:

- **running** — worktree exists, ports allocated and bound, containers and
  processes running, database in place.
- **suspended** — workloads (containers, processes) stopped. The worktree,
  database, ports (by record), and registry entry are preserved. Ports are
  not bound while suspended — anything on the machine can take them.

`plax up` creates a running instance and verifies what it started before
reporting success. `plax down` removes everything, tolerating missing
resources (a stopped Docker daemon or dead Postgres will not prevent
teardown of everything else). `plax up` rolls back all side effects in
reverse order if any step fails.

`resume` restarts a suspended instance and prints the drift report. If a
port the instance held is now taken, resume fails and names the offender;
free the port and retry, or `down` and `up` to reallocate.

## Working with instances

### Acquire

```sh
plax up <name>            # from current HEAD
plax up --ref <ref> <name>  # from a branch, PR, tag, or commit
```

`--ref` accepts a branch name, `pr/42`, a bare integer (treated as a PR
number), or a commit SHA. `plax up` branches `plax/<name>`, creates the
worktree, network, ports, derived `.env`, cloned database(s), containers,
and processes, then verifies. The summary prints the worktree path,
branch, database name, ports, logs dir, and scratch dir — capture it, it
is everything needed to work inside the instance.

### Inspect

`plax ls` shows state, live health, and unread mail. For running
instances the health column is a live probe (processes alive, ports
reachable) at call time, not a stored snapshot; suspended instances show
their last stored value. `--json` gives structured output including
worktree paths and port allocations.

### Work inside

- `plax exec <name> -- <cmd> [args...]` — run one command with the
  instance's `.env` loaded, working directory = instance worktree.
- `plax attach <name>` — interactive shell in the same context. Prints
  drift warnings and unread-mail notices to stderr.

### Destroy

`plax down <name>` tears everything down. Use it when an instance's work
is done or merged.

### Regenerate env

After `plax.json` or `.env.example` changes, `plax rederive` re-derives
every instance's `.env` from the current blueprint. Non-hole values from
the instance's existing `.env` and the user's `.env` are preserved; hole
values are recomputed. It prints a diff per instance and reminds you to
restart workloads. Use `rederive` for env-only changes; schema and data
changes need `down` + `up` against a refreshed base.

## Drift model

Instances go stale. `plax status <name>` reports six dimensions, each
`ok`, `drift`, or `unknown`:

- **code** — commits ahead/behind the base branch. Drift means the
  instance's branch is behind; sync it with git.
- **schema** — migration files vs the database. With
  `seed.applied_migrations` configured: migrations applied to the
  instance DB vs files at the worktree HEAD — an in-worktree migration
  clears the drift. Without it: clone-time migration-file hash vs
  worktree files.
- **data** — the instance's provenance version vs the current base
  version. Stale means the base was rebuilt since the instance was cloned.
- **host** — tool versions recorded at creation vs the machine's current
  versions (toolchain file).
- **config** — blueprint inputs (docker-compose.yml, env template,
  toolchain file) vs the stamp recorded at last `plax up`.
- **health** — live probe at call time: every allocated port reachable
  (dedicated services and process `port_var`s) and every native process
  alive. Suspended instances report `unknown`.

Reading `status` never writes the registry. Remediation by dimension:

| Dimension | Fix |
|---|---|
| code | `git pull`/merge the base into the instance branch |
| schema | re-migrate the instance DB (`plax exec <name> -- <migrate cmd>`) |
| data | `plax down` + `plax up` after `plax base refresh` |
| host | `plax down` + `plax up` to rebuild with current tool versions |
| config | `plax rederive` for env-only changes; `down` + `up` otherwise |
| health | investigate; `plax verify <name>` for the full battery |

`plax attach` prints a note to stderr when code, host, or config drift is
detected. `plax up`/`resume`/`ls`/`attach` print a stamp notice when
blueprint inputs changed since the registry's recorded stamp.

## Verify an instance

`plax verify <name>` runs a battery of checks and updates the instance's
health state in the registry (`healthy`/`unhealthy`):

- **env-completeness** — every key in the template and user `.env` is
  present in the derived `.env`
- **env-unresolved-holes** — no `{{VAR}}` placeholders remain
  unsubstituted
- **env-scrubbed-leaks** — no scrubbed key's real value appears in the
  derived `.env`, even under a different key name
- **dependency-isolation** — the worktree shares the parent's
  `node_modules` only if dependency manifests match; a mismatch means the
  instance runs the parent's dependencies
- **tcp-reachability** — every allocated port has a listener (explicit
  `verify` only, with a bounded poll)
- **process-liveness** — every declared native process is alive
- **db-existence** — every declared database exists
- **db-provenance** — every database has its provenance table (catches
  externally dropped-and-recreated databases)

The `check` field in `--json` output is the stable identifier for
scripting.

Verification also runs automatically on `plax up` and `plax resume`
before either reports success — minus the TCP probe, which is skipped
there because a freshly started app legitimately takes seconds to bind.
Static env checks run first and fail fast: a broken `.env` rolls `up`
back, leaving nothing behind. On a runtime-check failure the instance
stays up for debugging, is marked `unhealthy`, and the exit code is 1. A
suspended instance skips runtime checks with a note.

## Mailbox

Instances can pass messages to each other through a file-based mailbox at
`.plax/mail/<name>/`. No daemon, no broker.

```sh
plax send <name> --from <sender> "message body"
plax recv <name>           # read and remove the oldest message
plax recv <name> --all     # drain the mailbox
plax recv <name> --count 3
```

Semantics:

- `send` requires a body. `--from` identifies the sender; it defaults to
  the `PLAX_INSTANCE` env var — set it in the instance's environment so
  agents know who wrote what.
- Messages survive suspend: delivery is buffered, not a rendezvous. A
  message to a suspended instance waits on disk until the receiver wakes.
- `recv` reads and removes. `ls` shows unread counts; `attach` prints a
  notice when messages are waiting.
- Use the mailbox for routine, low-stakes coordination (heads-ups, status,
  rebase requests). Route high-stakes claims through the user, who can
  weigh evidence instances cannot see — a claim travels as fast as a bug
  report, and handoff propagates errors as efficiently as findings.

## Doctor

`plax doctor` validates the whole system in four areas, each check
`ok`, `warn`, or `fail`; exit code 1 if any check fails, 0 otherwise.

- **blueprint-vs-repo** — does the blueprint parse and validate? Do
  declared services exist in the compose file? Have blueprint inputs
  changed since the last `up`?
- **blueprint-vs-registry** — do live instances have their worktree,
  branch, database, and containers? Are allocated ports held by unknown
  instances or undeclared services?
- **repo-vs-machine** — does the machine satisfy the toolchain file? Is
  Docker reachable? Is Postgres reachable?
- **base** — does the base exist? Is it locked? What is its provenance?
  Is a refresh staged?

## Output conventions

Records go to stdout; human chatter goes to stderr. A pipe never has to
strip a banner. Records are also safe to parse from an agent: run with
`--json` where available and parse stdout.

`--json` is supported on: `ls`, `status`, `verify`, `doctor`, `send`,
`recv`, `base status`.

## Environment variables

- `PLAX_INSTANCE` — read by `send` as the default `--from`. Set it in an
  instance's environment so mailbox messages are attributed.
- `SHELL` — used by `attach` to find the shell.

## Files and directories

Plax keeps all state inside the repo under `.plax/`:

```
.plax/
  registry.json          instance records, blueprint stamp, ports
  worktrees/<name>/      git worktrees (each with a scratch/ directory)
  logs/<name>/           process logs (app.log, workers.log, ...)
  mail/<name>/           mailbox directories
```

The registry is a single JSON file: branch, worktree path, allocated
ports, database name(s), container IDs, process group IDs, creation time,
provenance (base version, toolchain hash, tool versions), state
(`running`/`suspended`), and last verification outcome. Writes are
serialized with a file lock.

Instance worktrees live inside the repo, so root-globbing tooling (test
runners, linters, type checkers) must ignore `.plax/` or it will traverse
every instance. `plax init` adds `.plax/` to `.gitignore` automatically;
other tools need their own ignore entries.

## Known limitations

- **Postgres only.** Logical isolation requires a driver; only Postgres
  has one. Redis and other services use `dedicated`.
- **`.env` only.** Derivation templates one file. A port in
  `vite.config.ts` or a database name in `config/database.yml` is out of
  reach.
- **Linux/macOS.** Process management uses process groups. Windows is not
  supported.
- **No archive.** There is no way to freeze an instance into a named
  blob. If state is worth keeping, promote it into the base or a fixture.

## Decision rules for agents

1. **Before doing anything**, read the repo's `plax.json` — it declares
   what an instance of this repo needs.
2. **To acquire an environment**: if the repo has a logical Postgres
   service and no base yet, `plax base create` then `plax base seed`
   (once). Then `plax up --ref <ref> <name>`. Capture the summary: it
   gives the worktree path, ports, and database.
3. **To trust an environment before using it**: `plax status <name>`
   (read-only drift report) and `plax verify <name>` (full battery,
   updates health). Drift on schema/data/config means rebuild, not
   workaround.
4. **To work**: `plax exec <name> -- <cmd>` for single commands,
   `plax attach <name>` for interactive sessions. The instance's `.env`
   is loaded and the working directory is its worktree.
5. **To coordinate with other instances**: `plax send`/`plax recv` —
   buffered, survives suspend. Attribute messages with `PLAX_INSTANCE`.
6. **To finish**: `plax down <name>` tears everything down, tolerating
   missing resources.
7. **When the blueprint changes** (plax.json, .env.example, compose,
   toolchain): `plax rederive` for env-only changes; `plax down` + `up`
   for schema or data changes, after `plax base refresh`.
