# Plax — Design Doc

Plax runs many copies of one repo's development environment at the same time, on your
own machine. Each copy gets its own ports, its own database, its own services. Each is
quick to create and meant to be thrown away.

It exists because coding agents now work in parallel, and parallel agents collide.

Git worktrees already give each agent its own checkout of the source. That is not
enough. Two agents running the app still bind port 3000 and still write to the same
database. Worktrees isolate code. Plax isolates the parts that run.

---

## 1. Problem

Say you want three agents working on the same repo at once. Today you copy the repo
three times and hand-edit each `.env`: change the port, change the database name,
change the Redis index. It works for about a week. Then:

- **Drift.** Add an environment variable and you have to add it to all three copies.
  Rotate a secret and you rotate it three times. Miss one and it breaks later,
  somewhere unrelated.
- **Permanence.** Copies are expensive to make, so you keep them around whether you
  need them or not.
- **Collision.** Tools that create worktrees copy `.env` verbatim. Every new worktree
  points at the same database until someone remembers to change it.
- **Tests have the same disease.** Parallel test workers share one test database and
  truncate it between runs, so they are not really running in parallel.

The copies are the problem. A copy has to be maintained: it starts correct and drifts.

The alternative is to declare the environment once, in a file in the repo, and build
each instance from that declaration when you need it. Change the declaration and the
next instance is correct. Nothing drifts, because nothing survives that was not built
from the declaration.

Call that building step **derivation**. It is the idea the rest of this document is
about.

## 2. Architecture

Three parts. A file that declares, a thing that runs, and a terminal pointed at it.
They stay separate.

```
                    attach (terminal, tmux pane, agent session)
        ┌──────────────┐      ┌──────────────┐
        │   window     │      │   window     │            (no window —
        └──────┬───────┘      └──────┬───────┘             headless is fine)
               │                     │                          │
               ▼                     ▼                          ▼
        ┌─────────────────┐   ┌─────────────────┐   ┌─────────────────┐
        │  instance i1    │   │  instance i2    │   │  instance i3    │
        │  worktree       │   │  worktree       │   │  worktree       │
        │  derived .env   │   │  derived .env   │   │  derived .env   │
        │  db_i1  q_i1    │   │  db_i2  q_i2    │   │  db_i3  q_i3    │
        │  processes      │   │  processes      │   │  processes      │
        └────────▲────────┘   └────────▲────────┘   └────────▲────────┘
                 │    materialize      │                     │
        ┌────────┴──────────────────────┴─────────────────────┴────────┐
        │  blueprint (checked-in file)                                 │
        │  resources + isolation strategy · env template · base sources│
        └──────────────────────────────────────────────────────────────┘
```

### 2.1 Blueprint — the declaration

The blueprint is one file, checked into the repo. It says:

- which resources an instance needs — database, Redis, the app's own processes
- how each one is isolated
- a template for `.env`, with holes for the port, the database name, and endpoints
- where the starting data comes from
- the commands that start the app

The runtime reads this file and does what it says. It does not guess.

**Writing the blueprint is the hard part. Running it is not.** Writing one means
reading the repo's compose file, finding every environment variable the app uses, and
picking an isolation strategy for each service. That is a good job for an AI agent —
an API call, or whatever agent CLI is at hand. No model ships inside the tool;
bundling weights for this would be maintenance cost for nothing.

It is not a one-time job. The blueprint has to be rechecked whenever the repo's
services or configuration change, so expect to re-run the author as a checker. That is
fine — the output is always a file a human reads and approves before it lands.

The runtime itself never calls a model. Two `plax up` runs from the same blueprint
have to produce instances that match each other, and a model in that path could make
them differ. It would also put a network call and a few seconds of latency on a
command people run all day.

### 2.2 Instance — the thing that runs

An instance is a worktree plus everything the blueprint asks for: a derived `.env`,
its own database, its own or shared services, and its processes. Nobody has to be
watching it. Headless agent work is the normal case, not a special one.

A registry — a single JSON or SQLite file — tracks each instance: id, worktree path,
branch, allocated ports, resource endpoints, pids, agent session ids, and whether it
is running or suspended.

Each resource in the blueprint picks the cheapest isolation that still prevents
collision:

| Strategy | Use when | Example |
|---|---|---|
| `logical` | The service has a good built-in way to separate tenants | Postgres: one server, one database per instance, cloned in about 100 ms |
| `dedicated` | The service separates tenants badly or not at all | Redis: one small container each. Database indexes stop at 16, and queue names collide inside an index |
| `shared` | The service holds no state | A rendering or conversion service: one container serves everyone |
| `external` | The dependency cannot be cloned | Blob storage in SaaS: run a local emulator, or share it on purpose and say so |
| `native` | The instance's own app processes | Dev server and workers, run directly in the worktree |

Limits worth stating up front:

- `logical` needs a driver written per service. Postgres has the template trick. Most
  things do not.
- Derivation only works if the app is configured through environment variables. The
  blueprint is a contract the repo agrees to, not something discovered automatically.
- Cloned resources can point at shared ones. Clone the database but share blob
  storage and your rows reference files every instance can overwrite. The blueprint
  has to state that trade, not hide it.
- **A blueprint can be complete and still be wrong.** If someone adds a write path to
  a service the blueprint calls `shared`, everything still materializes and the
  instances quietly collide. Missing things fail loudly; wrong strategies fail
  quietly. `plax doctor` should hunt for contradictions of that kind — a `shared`
  service that has a writable volume, say — not just for things that are absent.
- The blueprint covers runtime resources only. It assumes the machine already has the
  right language and tool versions; that is what `mise.toml` or `.tool-versions` is
  for. See §6, *isolated from each other, not sealed off from the machine*.

**Build the dumb version first.** One compose project per instance, every service
dedicated, plus env derivation and the registry. That is mostly plumbing and it
isolates correctly on day one. Then run ten instances and watch where it actually
hurts — memory from ten copies of a heavy service, instances taking too long to
create — and fix those, one resource at a time. Share the stateless services. Move the
database to template-cloning when creation speed or data size demands it. Let
measurements decide how fancy each resource gets.

### 2.3 Window — not something we build

A window is an operation, not an object: point a terminal, a tmux pane, or an agent
session at an instance. Two terminals on one instance is fine. Zero terminals on a
headless instance is fine.

Plax ships no UI beyond `ls`, `attach`, and `exec`. Terminal multiplexing is a solved
problem with good tools in it — tmux, Zellij, and newer session managers. The right
position is to be the thing they attach *to*.

## 3. The derivation engine

This is the core of the tool. It does two things:

1. capture the state of a resource and give it a name — that is a **source**
2. build a fresh copy of any source, on demand

The interface is the same for every resource type. Each type needs its own driver:

- **databases** — template-clone, or dump and restore
- **Redis** — write an RDB snapshot, mount it into a fresh container
- **file trees**, for state that is not in git (uploads, caches) — copy-on-write clone
  where the filesystem supports it (`reflink` on btrfs and XFS), `tar` otherwise

Named sources, provenance stamps, and staged refresh work the same way for all of
them. One thing does not carry across: **cross-resource consistency**. An archive is a
set of per-resource snapshots taken at roughly the same moment. It is not one
consistent point in time, and the tool says so instead of implying otherwise.

Databases come first. The other drivers get written when something actually needs
them.

**Sources.** The main one is the **base**: the reference dataset that instances are
built from, seeded however the team already seeds — fixtures, a filtered pull from a
replica, a dump. The others are archived instances (§4) and test fixtures.

**Building a copy.** Two mechanisms, picked by size and by where the source lives:

- `CREATE DATABASE new TEMPLATE base` — Postgres copies the files directly. About
  100 ms. It requires that nothing is connected to `base` while it runs. That is why
  nothing ever connects to the base: not the app, not a migration, not a stray `psql`.
  It exists to be copied.
- `pg_dump` then `pg_restore` — slower, but it works between separate containers and
  it works for archives.

**Refreshing the base.** Re-seeding the base while someone is creating an instance is
a race: the template mechanism takes a lock, and a copy taken mid-refresh would see
half-written data. So a refresh writes into a second database, `base_next`, and renames
it into place when it finishes. Creating an instance never waits on a refresh and
never sees a partial base.

**Provenance and drift.** Every source carries a one-row table *inside the database
itself*: source name, version, when it was refreshed. Because it lives inside the
database, every copy inherits it for free, through either mechanism, whether or not
the registry knows anything about it.

That makes drift four cheap comparisons, printed by `status` and on every resume:

- **Code** — instance branch against main: commits ahead and behind.
- **Schema** — the applied-migrations table in the instance database against the
  migration files on main: a set difference.
- **Data** — the instance's provenance row against the base's current version:
  *"built from base v3; base is now v5."*
- **Host** — the toolchain hash recorded when the instance was created against the
  machine now: *"built against Node 20.11; machine now has 20.14."* Instances created
  days apart can otherwise diverge without anyone noticing, and them matching each
  other is the whole guarantee (§6).

Deliberately not included: telling you which rows changed. That is expensive, and a
staleness report should not pretend to be a data diff.

**One engine, three users.** A new instance is built from the base. A restored
instance is built from its own archive. A test worker is built a throwaway database of
its own. The third is worth calling out: it replaces "share one test database and
truncate between tests" with real parallel isolation, and it falls out of machinery
built for something else.

## 4. Shutting down and coming back

You should be able to stop a session and pick it up later with the configuration and
the state intact. That splits into three parts with very different costs.

- **Config** is the blueprint. It is a file in git. Free.
- **History** — agent transcripts, scrollback — is mostly not ours. Agent tools save
  their own sessions and scrollback belongs to the terminal. The registry keeps
  pointers: branch, agent session ids, what was attached.
- **Resource state** — what the database and the untracked files hold — is the real
  substance, and it belongs to the derivation engine.

Two tiers:

1. **Suspend and resume** — cheap, build it early. Stop the processes, stop the
   containers. Volumes stay where they are. The registry remembers the rest. Resume
   starts everything again and prints the drift report; resuming is never silent.
2. **Archive and restore** — later, and real work. Serialize a destroyed instance into
   a named source, then rebuild it whenever. This is the derivation engine pointed at
   a different job, not new machinery.

State that depends on wall-clock time — delayed jobs that fire late, timestamps that
are now wrong — is accepted as a fact of life. The agent working inside the instance
deals with it. The tool does not try to.

## 5. Commands

**Plax** is the working name for the project and the CLI. No relation to tmux's own
commands. The operations are the design:

```
plax up <name>          build an instance from the blueprint
plax down <name>        destroy it: drop the database, release the ports, remove the worktree
plax ls                 what exists, and its state
plax status <name>      drift report: code, schema, data, host
plax suspend <name>     stop processes and containers, keep the state
plax resume <name>      start it again, print the drift report
plax exec <name> -- cmd run a command inside the instance's environment
plax attach <name>      open a shell wired to the instance
plax doctor             check the blueprint against the repo, the registry, and the machine
plax base refresh       re-seed the base through the staging swap
plax rederive [--all]   regenerate .env files after the template changes
plax archive <name>     snapshot a destroyed instance into a named source
```

`attach` injects an environment; it does not manage windows. It spawns a shell, or a
command, with the instance's derived `.env` loaded and the worktree as the working
directory. Where that shell lives is up to you: a bare terminal, `tmux new-window
'plax attach i2'`, a Zellij tab, or an agent through `plax exec i2 -- claude`. Same as
ssh sessions, which live wherever you put them.

`exec` is the collaboration primitive, on purpose. "Agent B, go review agent A's work"
is `plax exec a1` from B's session. When instances are cheap, collaboration falls out
of the substrate and no message bus is needed.

`doctor` checks three things:

- **blueprint against repo** — are the declared services in the compose file? Does
  every hole in the env template have a matching variable in `.env.example`? Does any
  declared strategy contradict the service it describes?
- **blueprint against registry** — is a live instance holding a port or a database the
  current blueprint no longer allocates?
- **repo against machine** — does this machine satisfy the repo's `mise.toml` or
  `.tool-versions`?

**Output is meant to be parsed.** `ls` and `status` print one record per line with
stable fields, so ordinary shell tools work on them:

```
plax ls | awk '$2 == "suspended" { print $1 }' | xargs -n1 plax down
```

Both also take `--json` for anything with real structure, like the drift report.
Human-readable chatter goes to stderr and records go to stdout, so a pipe never has to
strip out a banner.

## 6. Decisions

**Isolated from each other, not sealed off from the machine.** Plax guarantees that
instances do not collide and that they all build from one state source, on one
machine. It does not make an instance airtight: one built on your laptop is not
guaranteed to match one built on someone else's. The machine's toolchain is the shared
reference — pinned per repo by `mise.toml` or `.tool-versions` — so instances match
each other and match the environment you work in yourself. That is the property
parallel agents need: agreement with each other and with you. Making an environment
airtight across machines is a real problem and someone else's product (devcontainers,
Nix).

**Collision isolation first.** Stopping instances from stepping on each other comes
first. Containment — restricting what an agent is allowed to touch — is a later
deepening of the project, deferred on purpose.

**Dumb runtime.** No model calls anywhere in the build path. AI writes the blueprint,
and rechecks it when the repo changes, and always produces a file a human reviews.

**No view layer.** Plax never owns windows, panes, layouts, or scrollback. `attach`
spawns a shell with the environment loaded; you put it wherever you already work.

**"Base," not "golden,"** for the reference source.

**Time-dependent state on resume belongs to the agent inside the instance,** not to
the tool.

## 7. Open questions

1. What is the smallest blueprint schema that can express a real repo? Resist inventing
   generality until a second repo demands it.
2. Does the closest existing tool already cover enough of this pain? Dagger's
   container-use gives each agent a branch and a container over MCP. Worth a day of
   real use before writing much code. The bet here is that deriving a *native, running*
   stack — a real database with real seed data, processes running directly on the
   machine, the same environment the human works in — is a different design center.
   That bet should be tested, not assumed.
3. Does `exec` carry the whole multi-agent collaboration story? Look at what `exec`
   actually is: agent B stepping into agent A's instance, not sending it a message.
   Two agents inside one instance share that instance's database and ports — the
   collision problem again, in the one place the tool created to avoid it. That is
   fine for reading and reviewing, which is most of what "review agent A's work"
   means. It is not fine for two agents writing at the same time. The first time that
   case shows up for real is when something with real channels — instances passing
   messages to each other instead of entering each other — earns its place. Not
   before.
4. What should suspend do about half-finished work sitting in queues: drain it, freeze
   it, or ignore it?
