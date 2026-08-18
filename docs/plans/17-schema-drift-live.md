# Plan 17 — Schema drift reads live applied migrations

## Objective

Make schema drift a live computation from the instance database: compare the
migrations actually applied to an instance's DB against the migration files at
the worktree's HEAD, instead of comparing a clone-time `SchemaHash` stamp that
is frozen and never updates, so drift correctly clears after an in-worktree
migration.

## Decision recorded from triage 2026-08-14

The triage's Phase 3 (#47) prescribes reading applied-migration identifiers
directly from the instance DB at status time and dropping the frozen stamp
from the comparison. That direction is correct and kept. Two decisions refine
it:

**The applied-migrations tracking is a blueprint contract, not a guess.**
Migrations run via the opaque `seed.migrate` command (`psql -f file.sql` in
the sample) — plax never touches the framework's persistence. Reading applied
identifiers therefore requires an explicit config: the tracking table **and**
the identifier column, plus a defined filename↔identifier mapping. A table
name alone is insufficient, and the sample repo's plain `.sql` + `psql -f`
setup has **no tracking table at all**, so the field is mandatory opt-in and
cannot be inferred.

**Legacy stamp comparison is retained as an unconfigured fallback.** When
`seed.applied_migrations` is absent, `schemaDrift` falls back to the existing
hash-of-filenames comparison. This keeps today's behaviour (no regression) at
the cost of the known limitation — drift may not clear after an in-worktree
migration. The live path is the corrected behaviour, enabled by the config.
The alternative (report `Unknown` whenever unconfigured) would silently
disable schema detection for every repo that has not opted in, which is worse.

Two scope clarifications from review, resolved:

- The triage's "#50 for free" claim is **moot** — #50 shipped (direction-aware
  messages already in `schemaDrift`, status.go:342-365). No dependency.
- Schema drift now measures **DB-vs-worktree agreement only**, decoupled from
  the base. A freshly-cloned instance on a branch behind base shows drift
  ("database has migrations the worktree does not declare") — a true signal
  that already aligns with the `code` dimension.

## Package layout

```
pkg/blueprint/blueprint.go        # SeedConfig gains AppliedMigrations; type + validation
pkg/blueprint/blueprint_test.go   # Validation tests for the new field
pkg/derive/postgres/basemanager.go# New BaseManager.AppliedMigrations reads tracking table
pkg/derive/postgres/basemanager_test.go # AppliedMigrations table-present/absent cases
pkg/status/status.go              # schemaDrift branches on config: live vs legacy; Build decouples schema from provErr when configured; interface
pkg/status/status_test.go         # fakeBM.AppliedMigrations; live schema tests; update schemaDrift helper
docs/plans/index.md               # Add plan 17 to the phase table
```

## Type specifications

`blueprint.SeedConfig` gains one pointer field:

```go
type SeedConfig struct {
    Migrate           string              `json:"migrate"`
    Command           string              `json:"command"`
    Workdir           string              `json:"workdir"`
    MigrationsDir     string              `json:"migrations_dir,omitempty"`
    AppliedMigrations *AppliedMigrations  `json:"applied_migrations,omitempty"`
}

type AppliedMigrations struct {
    Table  string `json:"table"`
    Column string `json:"column"`
}
```

Validation rules:

- `Table` and `Column` are both required when the block is present (reject a
  partially-specified block).
- Both must match `^[A-Za-z_][A-Za-z0-9_]*$` — this doubles as SQL-injection
  defence since the values are interpolated into a `SELECT` query.
- No relation to `MigrationsDir` is enforced at validation; the live path only
  requires both the tracking block and a resolved `MigrationsDir` (which
  already has a default fallback).

`status.BaseManager` interface gains one method:

```go
type BaseManager interface {
    BaseStatus(ctx context.Context) (postgres.BaseInfo, error)
    InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error)
    AppliedMigrations(ctx context.Context, dbName string) ([]string, error)
}
```

The concrete `postgres.BaseManager` reads `bm.bp.Seed.AppliedMigrations`
internally, so the interface stays free of a blueprint dependency in its
signature. `fakeBM` in status_test gains an `applied []string` field and an
`appliedErr error` to satisfy the interface.

## Algorithms

### `postgres.BaseManager.AppliedMigrations` — read the tracking table

Precondition: callers must check `bp.Seed.AppliedMigrations != nil` before
calling this method.

```
exists = InstanceDBExists(dbName); if err return err
if !exists: return nil, nil                 # nothing to probe yet
pool = connect(dsnForDB(dbName)); defer close
q = "SELECT DISTINCT \"<column>\" FROM \"<table>\""
rows = pool.Query(ctx, q)
collect identifiers; sort; return
```

`⚠` Both `Table` and `Column` are double-quoted; blueprint validation already
restricts them to `[A-Za-z0-9_]`, so the interpolation is safe by construction.
A `DISTINCT` collapses frameworks that record a row per run of the same
migration. The returned set is the applied identifiers, e.g. `0001_init`
(sans extension, as most frameworks store it).

### `schemaDrift` — branch on configured tracking

Signature gains `ctx` and the config; `Build` passes
`deps.Blueprint.Seed.AppliedMigrations` and `ctx`:

```
resolve migrationsDir (default src/db/migrations)
names, err = SchemaFilesAtRef(repoRoot, ref, migrationsDir)   # worktree HEAD, else base
if err or empty: Unknown (as today)
ref resolved as today (worktree HEAD preferred, base fallback)

if am == nil:
    return legacyStampDrift(...)   # unchanged existing hash-vs-hash logic
if bm == nil: Unknown "postgres unreachable"
applied, aerr = bm.AppliedMigrations(ctx, rec.DBName)
if aerr:    Unknown "applied migrations: <err>"
if applied == nil: Unknown "no applied migrations recorded — run migrations in the instance"

fileIDs = { TrimSuffix(name, filepath.Ext(name)) for name in names }
branchOnly = fileIDs ∖ applied
dbOnly     = applied ∖ fileIDs

switch:
  len(branchOnly)==0 && len(dbOnly)==0 → OK
       detail "migrations match worktree HEAD" if usingWorktreeHead else "migrations match <base>"
  len(branchOnly)>0 && len(dbOnly)==0 → Drift
       "worktree declares <describeMigrations(branchOnly)> the database does not have — re-migrate the instance to apply them"
  len(branchOnly)==0 && len(dbOnly)>0 → Drift
       "database has <describeMigrations(dbOnly)> the worktree does not declare — rebase this branch or rebuild the instance"
  else → Drift (divergent)
       "migration histories have diverged — the database has <describeMigrations(dbOnly)> this branch lacks, and the branch declares <describeMigrations(branchOnly)> the database lacks. Rebuild the instance ('plax down' + 'plax up')"
```

`⚠` `filepath.Ext` strips the framework suffix (`.sql`, `.js`), mapping the
stored `0001_init` to the file `0001_init.sql`. Applied identifiers already
extension-less are unchanged. Filenames whose base minus extension collides
across different extensions are indistinguishable — a framework-persistence
constraint, noted, not handled.

`⚠` The legacy path is preserved verbatim (the existing git-set direction
branching at status.go:342-365) so unconfigured repos see no behaviour change.
The `prov`/`prov.SchemaHash` arguments remain needed only for that path.

`⚠` In the live path (when `applied_migrations` is configured), `schemaDrift`
reads from the framework's tracking table, not from `_plax_provenance`. `Build`
must therefore be restructured so that `schemaDrift` is called regardless of
the `InstanceProvenance` result — the existing `provErr` gate couples schema
to the provenance fetch, which is unnecessary when live tracking is active.
`dataDrift` remains gated on `provErr` as before since it reads provenance
version numbers.

## CLI specification

No command-syntax or flag changes. `plax status <name>`'s `schema` row (and
any `--json` output) reflects live applied migrations for instances whose
blueprint declares `seed.applied_migrations`; otherwise it behaves as today.
No writes to the registry.

## Error handling

| Failure | Behavior |
|---|---|
| `seed.applied_migrations` absent | legacy stamp comparison (unchanged) |
| `Table`/`Column` invalid or partial | blueprint validation rejects; `plax init`/load error |
| `bm` nil (postgres unreachable) | schema `Unknown`, `"postgres unreachable"` |
| `AppliedMigrations` query errors | schema `Unknown`, detail names the error |
| tracking table not yet created | `AppliedMigrations` returns nil → schema `Unknown`, `"no applied migrations recorded — run migrations in the instance"` |
| applied == worktree files | schema `OK`, `"migrations match worktree HEAD"` |
| worktree declares more | schema `Drift`, re-migrate advice, names migrations |
| DB has more | schema `Drift`, rebase/rebuild advice, names migrations |
| both have extras | schema `Drift`, divergent, names both sets |
| `InstanceProvenance` fails, live tracking active | schema computed from live tracking table; data `Unknown` |

## Tests

- `TestBlueprintValidation_AppliedMigrationsPartial` — block with only
  `table` or only `column` rejected.
- `TestBlueprintValidation_AppliedMigrationsBadIdent` — `table`/`column`
  containing `-`, space, or `"` rejected.
- `TestPostgres_AppliedMigrations_TablePresent` — a real tracking table
  returns the sorted, deduped identifier set (integration, `t.Skip` unless
  `PLAX_TEST_POSTGRES_URL`).
- `TestPostgres_AppliedMigrations_TableAbsent` — no tracking table returns
  nil, nil.
- `TestStatus_SchemaLive_MatchesWorktreeHead` — `applied` equals the branch
  file IDs → `OK`.
- `TestStatus_SchemaLive_BranchAhead` — DB has only `0001`, branch files add
  `0002_add_users.sql` → `Drift` with re-migrate advice naming `0002_add_users`.
- `TestStatus_SchemaLive_DbAhead` — DB applied includes a migration the branch
  files lack → `Drift` with rebase/rebuild advice.
- `TestStatus_SchemaLive_Diverged` — `applied` and file IDs each have an extra
  → `Drift`, names both.
- `TestStatus_SchemaLive_NoAppliedRows` — `applied == nil` → `Unknown`,
  "no applied migrations recorded".
- `TestStatus_Build_SchemaLiveDespiteProvFailure` — `InstanceProvenance`
  returns a connection error but `applied_migrations` is configured → schema
  is `OK` (computed from live tracking), data is `Unknown`.
- Existing `TestStatus_SchemaDriftBaseAhead`/`BranchAhead`/`Diverged` updated
  for the new `schemaDrift` signature via the `schemaDriftResult` helper, and
  still pass through the legacy fallback (`applied_migrations == nil`).

## Acceptance criteria

- `plax status <name>` on an instance whose blueprint sets
  `seed.applied_migrations` reports schema from the live applied-migration
  set, not the frozen `SchemaHash` stamp.
- Migrating an instance DB in-worktree and re-running `plax status` returns
  the `schema` dimension to `ok` when the applied set matches worktree HEAD.
- Removing `seed.applied_migrations` from a blueprint restores today's stamp
  comparison unchanged.
- `plax status` performs no registry writes on the schema path.
- Migration identifiers stored with a `.sql`/`.js` extension stripped compare
  correctly against their files.

## Dependencies

No new Go imports — `context`, `sort`, `path/filepath`, `strings`, and
`github.com/jackc/pgx/v5` (for the tracking-table query) are all already used
by the touched packages.
