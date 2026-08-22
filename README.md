# plax

[![GitHub release](https://img.shields.io/github/v/release/apollopower/plax)](https://github.com/apollopower/plax/releases/latest)

Run many copies of your repo's development environment at once.
Each copy gets its own ports, its own database, its own services.
Built for coding agents working in parallel.

---

Git worktrees give each agent its own checkout of the source.
That is not enough. Two agents running the app still bind port 3000
and still write to the same database. Worktrees isolate code.
Plax isolates the parts that run.

## How it works

One file — `plax.json` — checked into your repo declares what an
instance needs: services, isolation strategy, env template, seed
command, toolchain. `plax up <name>` builds a new instance from
that declaration. `plax down <name>` throws it away.
Nothing drifts, because nothing survives that was not built from
the declaration.

## Commands

    plax init                  scaffold plax.json from the repo
    plax up <name>             build and start an instance
    plax up --ref <ref> <name> build from a branch, PR, tag, or commit
    plax down <name>           destroy it
    plax ls                    list instances
    plax status <name>         drift report: code, schema, data, host, config, health
    plax verify <name>         run verification checks, update health
    plax suspend <name>        stop workloads, keep state
    plax resume <name>         start again, print drift report
    plax exec <name> -- <cmd>  run a command inside an instance
    plax attach <name>         open a shell wired to the instance
    plax doctor                check blueprint, registry, machine, and base
    plax base create           create empty base (migrated, no seed)
    plax base seed             seed the base database
    plax base reset            drop and recreate base (schema only)
    plax base refresh          re-seed base through staged swap
    plax base status           base health and provenance
    plax rederive              regenerate .env files after blueprint changes
    plax send <name>           write a message to an instance's mailbox
    plax recv <name>           read and remove messages (--all, --count)

Records go to stdout. Human chatter goes to stderr.
`--json` is available on `ls`, `status`, `verify`, `doctor`, `send`,
`recv`, and `base status`.

## Install

**Homebrew (macOS and Linux):**

```sh
brew tap apollopower/plax
brew install plax
```

**Pre-built binaries:**

Download from the [latest release](https://github.com/apollopower/plax/releases/latest).

**Go install:**

```sh
go install github.com/apollopower/plax/cmd/plax@latest
```

Requires Go 1.26+.

## Documentation

- [Manual](docs/manual.md) — full guide; start with the [Getting Started walkthrough](docs/manual.md#3-getting-started) (install → `plax init` → `plax up` → work → `plax down`)
- [Design doc](docs/design.md) — architecture, isolation strategies, derivation engine
- [Implementation plans](docs/plans/index.md) — phased roadmap with detailed specs

## License

MIT
