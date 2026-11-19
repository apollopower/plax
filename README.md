# plax

[![GitHub release](https://img.shields.io/github/v/release/apollopower/plax)](https://github.com/apollopower/plax/releases/latest)

Plax is a command-line tool for running several copies of an application's
development environment. Each instance has its own Git worktree and ports.
`plax.json` defines how its databases and services are isolated.

Use it to test branches side by side or give parallel coding agents separate
environments. Git worktrees separate source files, but applications still need
different ports and databases to run independently.

## How it works

Check a `plax.json` file into your repository to configure services, application
processes, ports, environment variables, database setup, and toolchain tracking.
Services can be isolated per instance or shared, depending on the configuration.

`plax up <name>` creates a worktree, allocates ports, prepares the configured
services and databases, writes the instance's environment file, and starts its
processes. `plax down <name>` stops the instance and removes its resources.
You can also suspend an instance and keep its state for later.

Plax manages development environments, not agents or terminal sessions. It can
be used interactively or from scripts.

## Commands

    plax init                  scaffold plax.json from the repo
    plax guide                 print the bundled operational reference
    plax up <name>             build and start an instance
    plax up --ref <ref> <name>  build from a branch, PR, tag, or commit
    plax down <name>           stop and remove an instance
    plax ls                    list instances
    plax status <name>         drift report: code, schema, data, host, config, health
    plax verify <name>         run verification checks, update health
    plax suspend <name>        stop workloads, keep state
    plax resume <name>         start again, print drift report
    plax exec <name> -- <cmd>   run a command inside an instance
    plax attach <name>         open a shell with the instance's environment
    plax doctor                check blueprint, registry, machine, and base
    plax base create           create empty base (migrated, no seed)
    plax base seed             seed the base database
    plax base reset            drop and recreate base (schema only)
    plax base refresh          re-seed base through staged swap
    plax base status           base health and provenance
    plax rederive              regenerate .env files after blueprint changes
    plax send <name>           write a message to an instance's mailbox
    plax recv <name>           read and remove messages (--all, --count)
    plax upgrade               update plax to the latest release (--check to report)

Command results go to stdout; progress messages and diagnostics go to stderr.
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

- [Manual](docs/manual.md) — setup, configuration, and command reference. Start with [Getting started](docs/manual.md#3-getting-started).
- `plax guide` — operational reference bundled with the binary, available without a repository checkout
- [Design doc](docs/design.md) — architecture, isolation strategies, derivation engine
- [Implementation plans](docs/plans/index.md) — development plans and implementation status

## License

MIT
