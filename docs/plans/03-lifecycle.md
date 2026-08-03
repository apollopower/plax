# Phase 3 — Instance Lifecycle

The first phase where a user can create, use, and destroy instances. This
wires Phase 1 (blueprint, registry, ports) and Phase 2 (derivation) into
concrete CLI commands.

---

## 3.1 `plax up <name>`

The main orchestration command. Given an instance name (e.g. `i1`):

1. **Read blueprint** — `plax.json` from repo root. Fail if missing or
   invalid.
2. **Check base** — Verify `plax_base` exists and is locked. If absent,
   suggest `plax base reset` first.
3. **Create branch** — `git branch plax/<name>` at current HEAD (if not
   already existing).
4. **Create worktree** — `git worktree add .plax/worktrees/<name>
   plax/<name>`
5. **Allocate ports** — One per port-bearing `dedicated` service and native
   process in the blueprint. `logical` services are skipped — they share the
   host's existing port. Probe OS for availability. Register in registry.
6. **Derive .env** — Read `.env.example` from worktree root. For each hole
   declared in blueprint, substitute `{{VAR}}` with the allocated port or
   computed value. Copy all non-hole variables verbatim. Write result to
   `<worktree>/.env`.
7. **Clone database** — `CREATE DATABASE plax_<name> TEMPLATE plax_base`
8. **Start dedicated containers** — Loop over services with
   `isolation: "dedicated"`. Call Docker driver for each with allocated
   ports.
9. **Start native processes** — Loop over `processes` in blueprint. For each:
   - Build the environment: merge the host's `os.Environ()` with the derived
     `.env` variables and the allocated port (set as the env var named by
     `port_var`, e.g. `PORT=3001`)
   - If the `command` string contains `{{VAR}}` template holes, substitute
     them with the allocated port value
   - Spawn the command in the worktree directory with the merged environment
   - Record the PID

   For the eai app process (`port_var: "PORT"`, `command: "bun run dev:app"`),
   this means `PORT=3001` is set in the environment before spawning. `next dev`
   reads `PORT` from the environment and listens on the allocated port
   automatically.
10. **Write registry** — Save instance record with all ports, container IDs,
    PIDs, DB name, provenance version, and state=`"running"`.

**Failure modes:**
- Port allocation fails → roll back all previous steps, report which port
- Docker fails → stop any started containers, drop DB, remove worktree,
  report error
- Process spawn fails → same rollback

Rollback on `up` failure is essential. A half-created instance is worse than
none.

### Branch strategy detail

`plax up <name>` creates `plax/<name>` branching from the current commit.
If the branch already exists (resume scenario), it checks out the existing
worktree instead — but `up` on an existing instance is an error; use
`resume` for that.

The branch naming ensures `git branch` lists all Plax-managed branches
together and avoids collision with user branches.

## 3.2 `plax down <name>`

1. Stop native processes (SIGTERM, wait 5s, SIGKILL)
2. Stop and remove dedicated containers
3. Remove container volumes
4. Drop instance database: `DROP DATABASE IF EXISTS plax_<name>`
5. Remove Docker network
6. Release all ports in registry
7. Remove worktree: `git worktree remove .plax/worktrees/<name>`
8. Delete branch: `git branch -D plax/<name>` (only if merged or forced)
9. Remove registry entry

## 3.3 `plax ls`

Read registry. Print table:

```
NAME   STATE     BRANCH             PORTS                               CREATED
i1     running   plax/feature-auth  3001 6380 3031                       5m ago
i2     running   plax/feature-fix   3002 6381 3032                       2m ago
```

Fields: name, state, branch, allocated ports, age, unread message count.

`--json` flag prints full registry as JSON for scripting.

## 3.4 `plax attach <name>`

Spawns an interactive shell with:
- `WORKDIR` = instance worktree path
- `ENV` = instance's derived `.env` loaded (as actual env vars, not a file)
- Prints drift notice if any (delegated to Phase 4)

Under the hood: `exec.Command(shell, "--login")` with `os.Environ()` merged
with parsed `.env`. Shell discovery: `$SHELL` env var, then
`/bin/bash`, `/bin/sh`.

## 3.5 `plax exec <name> -- <cmd> [args...]`

Same environment setup as `attach`, but runs the given command non-interactively.

- Stdout/stderr are inherited from the parent process
- Exit code is propagated
- `--` separates the instance name from the command

Example: `plax exec i1 -- bun test`

## 3.6 `.env` derivation details

The derivation reads the blueprint's `env.template` file (`.env.example`)
and the `env.holes` map:

1. Start with the template file as the base
2. For each hole key (e.g. `DATABASE_URL`), find the corresponding line in
   the template (by key prefix) and replace its value with the rendered hole
   template
3. `{{DB_NAME}}` resolves to `plax_<name>` (always)
4. All other `{{VAR}}` holes resolve to their allocated port value or the
   env var set on the process (e.g. `{{PORT}}` → 3001, `{{REDIS_PORT}}` →
   6380)
5. Copy all non-hole lines verbatim (preserves comments and ordering)
6. Write to `<worktree>/.env`

A variable that appears in `env.holes` but is absent from the template file
is appended at the end of the file.

---

## Deliverables

- [ ] `plax up <name>` — full orchestration with rollback
- [ ] `plax down <name>` — full teardown
- [ ] `plax ls` — table + JSON output
- [ ] `plax attach <name>` — interactive shell in instance env
- [ ] `plax exec <name> -- <cmd>` — one-shot command in instance env
- [ ] `.env` derivation engine (hole substitution)
- [ ] Git worktree + branch management
- [ ] Process supervision (spawn, track PID, signal on teardown)
- [ ] Process port injection: set `PORT` env var + `{{VAR}}` template
      substitution in command strings
- [ ] Two running instances of eai on the same machine, non-colliding
