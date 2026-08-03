# Phase 4 — State Management and Drift Detection

Instances that can be paused, resumed, inspected for drift, and repaired when
the blueprint or base changes. This phase adds durability and confidence to
the development loop.

---

## 4.1 `plax suspend <name>` and `plax resume <name>`

### Suspend

1. **Stop native processes** — SIGTERM to tracked PIDs, wait 5s, SIGKILL
2. **Stop dedicated containers** — `docker stop` (preserves volumes)
3. **Update registry** — state → `"suspended"`, clear PIDs, keep containers
   (stopped) and ports

Ports are *reserved by record only*. Nothing is bound. The port allocation is
kept in the registry but the OS-level probe on resume will verify it's still
free.

### Resume

1. **Check ports** — For each allocated port, probe `net.Listen`. If taken,
   print error with pid/command of the occupant. Exit with code 1. Do not
   move to a different port.
2. **Restart dedicated containers** — `docker start` (reuses existing volumes
   and container config)
3. **Restart native processes** — Same as `up` step 9
4. **Update registry** — state → `"running"`, record new PIDs
5. **Print drift report** — Call `plax status` and display to user

Resume is never silent. The drift report prints on every resume so the user
knows what changed while the instance slept.

## 4.2 `plax status <name>` — Drift report

Five comparisons, printed as a table:

| Dimension | From | Against | Method |
|---|---|---|---|
| Code | Instance branch (`plax/<name>`) | `HEAD` of main | `git rev-list --left-right --count main...plax/<name>` |
| Schema | Applied migrations in instance DB | Migration files in worktree | Compare `_migrations` table (Orchid ORM's) filenames against `src/db/migrations/*.ts` |
| Data | Provenance row in instance DB | Provenance row in `plax_base` | `SELECT version FROM _plax_provenance` on both |
| Host | Toolchain recorded at creation | Machine's current tools | Compare recorded `node@X bun@Y` against `node --version` + `bun --version` |
| Config | Blueprint config stamp in registry | Current compose + .env.example | SHA-256 of `docker-compose.yml` + `.env.example` + `.tool-versions` |

Output:

```
i1 — drift report
  Code:    ahead 3, behind 0        (up to date with main)
  Schema:  up to date               (migrations match)
  Data:    built from base v3       (base is now v5 — stale)
  Host:    built on node@22.19.0    (machine has node@22.19.0 — ok)
  Config:  blueprint matches repo   (compose, .env.example unchanged)
```

`--json` flag for machine-readable output.

## 4.3 `plax doctor`

Full validation pass across four areas:

### Blueprint vs repo
- Every declared service exists in `docker-compose.yml` (by image name)
- Every hole variable has a corresponding line in `.env.example`
- No `shared` service has a writable volume
- No `logical` service is missing `type: "postgres"` (day one)
- Emit warnings for services in compose that aren't in blueprint
- Recalculate config stamp and compare to registry's stamp

### Blueprint vs registry
- Every port in registry's port_allocations corresponds to a declared
  service/process in the blueprint
- No dangling instance entries (worktree missing, DB missing)

### Repo vs machine
- Parse `.tool-versions`, check each tool is installed and matches version
  (`node --version`, `bun --version`)
- Check Docker daemon is reachable
- Check Postgres is reachable (`psql ping`)

### Base health
- `plax_base` exists
- `plax_base` refuses connections (`ALLOW_CONNECTIONS false`)
- Provenance table is present and has a row
- `plax_base_next` does not exist (or if it does, a swap was deferred)

## 4.4 `plax rederive [--all]`

Regenerate `.env` files after the blueprint's env template changes.

Without `--all`: rederive `.env` for every running/suspended instance.
With `--all`: also include instances that exist in registry.

Algorithm:
1. Load current blueprint
2. For each instance in registry:
   a. Read existing `.env` from worktree
   b. Re-apply hole substitution (preserves allocated ports and DB name)
   c. Preserve variables that are NOT in the holes map (e.g. secrets)
   d. Write new `.env`
3. Print diff for each changed file

## 4.5 `plax base refresh`

Calls Phase 2's staged refresh. On success, updates registry's config stamp
(provenance version for all instances that were built from the old base is
now stale — `plax status` will report it).

## 4.6 Config stamp

On every Plax command (not just `doctor`), cheaply check:

- Does `.plax/registry.json` exist?
- If yes, compare SHA-256 of `docker-compose.yml`, `.env.example`,
  `.tool-versions` against the stored `blueprint_stamp`.
- If mismatch, print a one-line notice: `⚠ blueprint inputs changed — run
  'plax doctor' for details`

This is fast enough (3 file reads + 3 SHA-256 hashes) to run on every
command without noticeable delay.

---

## Deliverables

- [ ] `plax suspend <name>` — stop processes + containers, keep state
- [ ] `plax resume <name>` — port check, restart, drift report
- [ ] `plax status <name>` — 5-dimension drift report (table + JSON)
- [ ] `plax doctor` — 4-area validation pass
- [ ] `plax rederive [--all]` — rebuild .env files
- [ ] `plax base refresh` — staged refresh (wires Phase 2)
- [ ] Config stamp check on every command
