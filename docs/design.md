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
        │  resources + isolation strategy · env template · the base    │
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

**Secrets are not holes.** API keys, OAuth credentials, and tokens are the same
across every instance — they vary per machine, not per instance. The blueprint
declares the holes; the user's own `.env` (gitignored, never committed) supplies
the secrets. Derivation merges three sources: holes get per-instance values, keys
present in the user's `.env` get real secrets, and everything else falls through
to the template's defaults. This means `.env.example` can keep its placeholder
values (`"openai-api-key"`, empty strings) without breaking instances — the user's
`.env` overrides them.

**Writing the blueprint is the hard part. Running it is not.** Most of that work is
mechanical. `plax init` parses the compose file for the service list and `.env.example`
for the variable list, then emits a skeleton: every service present, `dedicated` by
default, every variable present with its example value. That is a parser, and it runs
offline.

The skeleton is intentionally incomplete. `init` emits hardcoded defaults for processes
(`app` + `workers` with `bun` commands) and a placeholder seed command, because no
heuristic can reliably determine which `package.json` script is the dev server, which
is the worker, and which is the test runner. The repo's own structure — monorepo layout,
turbo or nx config, Procfile, Makefile — carries that information, but extracting it
requires judgment a parser does not have.

An AI agent does. An agent with full repo access can read `package.json`, `turbo.json`,
`Procfile`, `Makefile`, and the scripts directory simultaneously, then fill in the gaps.
`init` gives it a structured starting point and the agent finishes the job:

```
plax init --root . > plax.json
# Agent reads the JSON, inspects repo, edits:
#   - processes: add/rename processes, set correct commands
#   - seed: fill in the actual seed command
#   - toolchain: set to .node-version or mise.toml
#   - isolation: fix any misclassified services
```

What is left is judgment — which services hold state, and which values have to vary per
instance. An AI agent is good at that, and it is the intended path: an API call, or
whatever agent CLI is at hand. No model ships inside the tool; bundling weights for this
would be maintenance cost for nothing. Nor is a model required. Some of the judgment
reduces to rules a parser can apply, and `doctor` has to encode those rules anyway: a
`volumes:` entry means the service holds state, so it cannot be `shared`; a value
carrying a port or a hostname probably needs a hole. Failing all that, a person writes
the file. It is one file, for one repo, by whoever already knows which services hold
state.

It is not a one-time job. The blueprint has to be rechecked whenever the repo's
services or configuration change, so expect to re-run the author as a checker. What
notices a recheck is due is the config stamp in §3, not somebody remembering. The output
is always a file a human reads and approves before it lands.

The runtime itself never calls a model. Two `plax up` runs from the same blueprint
have to produce instances that match each other, and a model in that path could make
them differ. It would also put a network call and a few seconds of latency on a
command people run all day.

### 2.2 Instance — the thing that runs

An instance is a worktree plus everything the blueprint asks for: a derived `.env`, its
own database, its services, and its processes. Nobody has to be watching it. Headless
agent work is the normal case, not a special one.

A registry — a single JSON or SQLite file — tracks each instance: id, worktree path,
branch, allocated ports, resource endpoints, pids, agent session ids, and whether it
is running or suspended.

The registry records ports. It does not own them; the machine does. So allocation
draws from the range the blueprint declares and then asks the OS whether the port is
actually free, before committing it.

A suspended instance keeps its ports. The port is written into its `.env`, and from
there into bookmarks, browser storage, and OAuth redirect URIs held by somebody else;
moving it breaks all of them. But it keeps them by record only. Nothing is bound while
the instance sleeps, so anything on the machine can take one. §4 says what happens
then.

Each resource in the blueprint picks the cheapest isolation that still prevents
collision:

| Strategy | Use when | Example |
|---|---|---|
| `logical` | The service has a good built-in way to separate tenants | Postgres: one server, one database per instance, cloned in about 100 ms. All instances share the same host port; only the database name varies. |
| `dedicated` | The service separates tenants badly or not at all | Redis: one small container each. Database indexes stop at 16, and queue names collide inside an index |
| `shared` | The service holds no state | A rendering or conversion service: one container serves everyone |
| `external` | The dependency cannot be cloned | Blob storage in SaaS: run a local emulator, or share it on purpose and say so |
| `native` | The instance's own app processes | Dev server and workers, run directly in the worktree |

Limits worth stating up front:

- `logical` needs a driver written per service. Postgres has the template trick. Most
  things do not.
- The blueprint templates one file today, `.env`, so derivation only reaches apps
  configured through the environment. A port in `vite.config.ts`, a database name in
  `config/database.yml`, a compile-time `config/dev.exs` — all out of reach. The limit
  is the hardcoded filename, not the mechanism: an env template is a special case of
  rendering a template into the worktree, and widening it to arbitrary files is a small
  change the day a repo needs one. Either way the blueprint is a contract the repo
  agrees to, not something discovered automatically.
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

**Day one is three strategies, not one.** The tempting first version is one compose
project per instance, every service `dedicated`, plus env derivation and the registry.
It is mostly plumbing and it isolates correctly. It is also not this tool: a dedicated
Postgres per instance means boot, migrate, and seed on every `plax up` — tens of seconds
against 100 ms — and it defers §3, which this document calls the core. An instance slow
to create is an instance you keep, and instances you keep are the copies §1 is about.

So day one is `logical` for Postgres, `native` for the app's own processes, `dedicated`
for the rest. `logical` buys cheap creation and brings the base, the provenance row, and
the drift report with it. `logical` services stay on one host port — all instances share
5432, each with its own database name — so they do not consume from the port pool.
`native` is the difference §7.2 exists to test. `shared` and `external` wait: `shared` is
an optimization and the one strategy that fails quietly, so it arrives when a specific
service hurts, with its `doctor` check in the same change; `external` is an escape hatch,
written the first time a dependency refuses to be cloned. Measurements decide those two.
They do not decide the first three.

### 2.3 Window — not something we build

A window is an operation, not an object: point a terminal, a tmux pane, or an agent
session at an instance. Two terminals on one instance is fine. Zero terminals on a
headless instance is fine.

Plax ships no UI beyond `ls`, `attach`, and `exec`. Terminal multiplexing is a solved
problem with good tools in it — tmux, Zellij, and newer session managers. The right
position is to be the thing they attach *to*.

## 3. The derivation engine

This is the core of the tool. It does three things:

1. seed a reference copy of a resource — the **base**
2. build a fresh copy of the base, on demand
3. stamp every copy so you can tell later how far it has moved

The interface is the same for every resource type. Each type needs its own driver:

- **databases** — template-clone
- **Redis** — write an RDB snapshot, mount it into a fresh container
- **file trees**, for state that is not in git (uploads, caches) — copy-on-write clone
  where the filesystem supports it (`reflink` on btrfs and XFS), `tar` otherwise

Provenance stamps and staged refresh work the same way for all of them.

Databases come first. The other drivers get written when something actually needs
them.

**The base** is the reference dataset instances are built from, seeded however the team
already seeds — fixtures, a filtered pull from a replica, a dump. There is one, and
everything is built from it.

The base is a local artifact. Everything else in this design is a declaration in git;
the base is a database on your disk. Whether it matches a teammate's depends on how it
is seeded. From fixtures in the repo, it does. From a filtered pull off a replica, it
does not — your pull and his run on different days and get different rows. Both are
called v5, and the provenance row records a version rather than a checksum, so nothing
catches it. That is §6's boundary seen from below.

**Building a copy.** One mechanism:

- `CREATE DATABASE new TEMPLATE base` — Postgres copies the files directly. About
  100 ms. It requires that nothing is connected to `base` while it runs. That is why
  nothing ever connects to the base: not the app, not a migration, not a stray `psql`.
  It exists to be copied. A violation is not a corruption, it is an intermittent
  `plax up` failure blamed on the wrong thing, so the rule is enforced by the database
  rather than by discipline: `ALTER DATABASE base WITH ALLOW_CONNECTIONS false`.
  Connections are refused; copying still works. It is how `template0` protects itself.

`pg_dump` and `pg_restore` are slower but cross container boundaries. They get written
the day the base lives somewhere the instance does not.

**Refreshing the base.** Re-seeding the base while someone is creating an instance is
a race: the template mechanism takes a lock, and a copy taken mid-refresh would see
half-written data. So a refresh creates a fresh `base_next` from scratch (empty,
migrated, then seeded), and renames it into place when it finishes. Copying the old base
via TEMPLATE and then re-seeding would carry forward data the seed script no longer
declares, and duplicate rows if the seed is not idempotent. A fresh creation avoids both.
`base_next` accepts connections while it is being seeded — it has to — and the swap is
what closes it: rename, then set the flag. Creating an instance never waits on a refresh
and never sees a partial base.

**Provenance and drift.** The base carries a one-row table *inside the database
itself*: source name, version, when it was refreshed. Because it lives inside the
database, every copy inherits it for free, whether or not the registry knows anything
about it.

That makes drift five cheap comparisons, printed by `status` and on every resume:

- **Code** — instance branch against main: commits ahead and behind.
- **Schema** — the applied-migrations table in the instance database against the
  migration files on main: a set difference.
- **Data** — the instance's provenance row against the base's current version:
  *"built from base v3; base is now v5."*
- **Host** — the toolchain hash recorded when the instance was created against the
  machine now: *"built against Node 20.11; machine now has 20.14."* Instances created
  days apart can otherwise diverge without anyone noticing, and them matching each
  other is the whole guarantee (§6).
- **Config** — the blueprint's inputs when it was approved against the repo now. The
  same trick as the provenance row, aimed one level up: approving a blueprint records
  the hashes of what it was written from — the compose file, `.env.example`, the
  toolchain file. Comparing them is three file hashes, cheap enough to run on every
  command rather than only in `doctor`: *"blueprint approved against compose@a3f1;
  compose is now 9c2d."* This is what notices that a recheck is due (§2.1). Its stamp
  lives in the registry, not inside a database, because what drifted is the repo rather
  than a resource.

Deliberately not included: telling you which rows changed. That is expensive, and a
staleness report should not pretend to be a data diff.

## 4. Shutting down and coming back

You should be able to stop a session and pick it up later with the configuration and
the state intact. That splits into three parts with very different costs.

- **Config** is the blueprint. It is a file in git. Free.
- **History** — agent transcripts, scrollback — is mostly not ours. Agent tools save
  their own sessions and scrollback belongs to the terminal. The registry keeps
  pointers: branch, agent session ids, what was attached.
- **Resource state** — what the database and the untracked files hold — is the real
  substance, and it belongs to the derivation engine.

**Suspend and resume** is the whole mechanism. Stop the processes, stop the containers.
Volumes stay where they are. The registry remembers the rest. Resume starts everything
again and prints the drift report; resuming is never silent.

Resume can fail where `up` cannot: a port the instance held may now be somebody else's.
It stops and names what took it, pid and command. It does not quietly move to a free
port, because the old one is written into places this machine does not control. Free the
port and resume, or build a new instance. The tool does not choose for you.

There is no archive. Freezing an instance's state into a named blob would put a second
kind of copy in the tool, and §1 is about what copies cost — an archive is opaque, it
is local, and it rots against a base that keeps moving. When state inside an instance
turns out to be worth keeping, promote it: into a fixture in git, or into the base. Then
it is reproducible and everyone has it. Anything short of that, `down` throws away on
purpose.

Work in flight is not touched. Half-finished jobs sitting in a queue are neither
drained nor frozen: the containers stop, the queue's volume keeps what was in it, and
resume lets the workers pick it up again. Redelivery of jobs claimed but never
acknowledged is the queue's own business, and its visibility timeout is better tested
than anything written here. Draining would make suspend unbounded — a job takes a
minute, or fails, or enqueues three more — and freezing means serializing state into a
blob, which is the thing this section just declined to build.

That works because the queue sits on a volume that outlives the container. Declare that
service `shared`, or run a queue that only lives in memory, and suspend drops the work
without saying so. §2.2 again: complete and still wrong.

State that depends on wall-clock time — delayed jobs that fire late, timestamps that
are now wrong — is accepted the same way. The agent working inside the instance deals
with it. The tool does not try to.

## 5. Commands

**Plax** is the working name for the project and the CLI. No relation to tmux's own
commands. The operations are the design:

```
plax init               scaffold a blueprint by parsing the repo, offline
plax up <name>          build an instance from the blueprint
plax down <name>        destroy it: drop the database, release the ports, remove the worktree
plax ls                 what exists, and its state
plax status <name>      drift report: code, schema, data, host, config
plax suspend <name>     stop processes and containers, keep the state
plax resume <name>      start it again, print the drift report
plax exec <name> -- cmd run a command inside the instance's environment
plax attach <name>      open a shell wired to the instance
plax doctor             check blueprint against repo, registry, machine, and base
plax base refresh       re-seed the base through the staging swap
plax rederive [--all]   regenerate .env files after the template changes
plax send <name>        write a message into an instance's mailbox
plax recv <name>        read and remove messages from its own mailbox
```

`attach` injects an environment; it does not manage windows. It spawns a shell, or a
command, with the instance's derived `.env` loaded and the worktree as the working
directory. Where that shell lives is up to you: a bare terminal, `tmux new-window
'plax attach i2'`, a Zellij tab, or an agent through `plax exec i2 -- claude`. Same as
ssh sessions, which live wherever you put them.

`exec` runs a command in one instance: your own. It is not a collaboration primitive.
Agents collaborate over channels, and there are two.

The first is git, and it is already there. Every instance is a branch. Agent B reviews
agent A by fetching A's branch into B's own worktree and reading it there. The commits
are copied, not shared, and nothing of A's is touched. That covers most of what "review
agent A's work" means, for no new machinery.

The second is a mailbox: one directory per instance. `send` writes a file into it,
`recv` reads and removes. This is Unix mail, and cron's spool, and the printer queue —
no daemon, and no locking, because a write is a create and creates do not collide. It
survives suspend, since the message waits on disk until the receiver wakes. Delivery is
buffered rather than a rendezvous: a blocking send would hang forever on an instance
that is never coming back.

Nothing pushes. `ls` prints a count and `attach` says how many are waiting, the same
way drift is reported.

`doctor` checks four things:

- **blueprint against repo** — are the declared services in the compose file? Does
  every hole in the env template have a matching variable in `.env.example`? Does any
  declared strategy contradict the service it describes? The cheap version of this
  question — the config stamp from §3 — already runs on every command; `doctor` is the
  full pass that says what actually changed.
- **blueprint against registry** — is a live instance holding a port or a database the
  current blueprint no longer allocates?
- **repo against machine** — does this machine satisfy the repo's `mise.toml` or
  `.tool-versions`?
- **base against its own rule** — does the base still refuse connections? The flag can
  be cleared by hand, and the failure it causes surfaces somewhere else entirely.

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

**Collision isolation, not containment.** Stopping instances from stepping on each
other is the job. Restricting what an agent may touch is not. Processes run natively,
so Plax holds nothing back; the exposure is the one you already have when you run an
agent on your own machine, and Plax neither adds to it nor takes it away.

Containment is still available — put the whole tool inside a container. Plax does not
need to know, and the blast radius becomes that container instead of your home
directory. Three things to know before trusting it. Services declared `dedicated` want
a container runtime inside the container, and the easy way, mounting the host's Docker
socket, is equivalent to handing over root on the host; use rootless nesting, or run
those services natively inside the sandbox. A filesystem boundary is not a credential
boundary: a mounted SSH key or cloud token is reachable from inside, and force-pushing
to production needs no files at all. And the parity above holds only if you work inside
that container too — otherwise your editor is on one side and the processes are on the
other, which is the arrangement §7.2 argues against.

**Dumb runtime.** No model calls anywhere in the build path, and none required
anywhere else either. `plax init` scaffolds a blueprint offline by parsing the repo. AI
fills in the judgment calls and rechecks them when the repo changes, faster than a
person would — but it is an accelerant, not a dependency, and it always produces a file
a human reviews.

**No view layer.** Plax never owns windows, panes, layouts, or scrollback. `attach`
spawns a shell with the environment loaded; you put it wherever you already work.

**Instances pass messages; they do not enter each other.** Two agents inside one
instance share its database and its ports — the collision problem, in the one place
built to prevent it. So collaboration is git for code and a mailbox for messages, and
`exec` stays what it is: a way to run a command in an environment.

**"Base," not "golden,"** for the reference source.

**In-flight work on resume belongs to the agent inside the instance,** not to the tool.
Queued jobs and stale timestamps alike.

## 7. Open questions

1. What is the smallest blueprint schema that can express a real repo? Resist inventing
   generality until a second repo demands it.
2. Does the closest existing tool already cover enough of this pain? Dagger's
   container-use gives each agent a git worktree and a Dagger container over MCP, and
   commits the work to its own branch. What it does not give the agent is your data.
   The bet here is that seed data and native processes are worth more than sandboxing.
   Half of that is settled: the agents this is for have to run the app against real
   rows, not just write code and run unit tests. The other half is not, and a day of
   real use would settle it — put a seeded database behind container-use, then measure
   how long one environment takes to create and what ten cost in memory. If those
   numbers come out fine, this tool does not need to exist.
3. What goes in a message? The mailbox is a mechanism, not a protocol. Prose, or
   something structured with a subject and a reply-to? Wait for two agents that
   actually need to talk, and expect the first honest answer to be prose.
4. What would bring archives back? State an instance produced that is expensive to
   reproduce and genuinely does not belong in the base — a data condition caught in the
   act, say, that nobody knows how to recreate. Nothing has demanded it yet. If
   something does, note that it also brings back named sources, `pg_dump`/`pg_restore`,
   and the caveat that a multi-resource snapshot is not one point in time.
