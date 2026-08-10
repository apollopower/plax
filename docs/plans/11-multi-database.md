# Plan 11 — Multiple databases per logical service

## Objective

Let a logical Postgres service declare additional database clones per
instance (e.g. `<name>_test`), so a repo whose test suite needs a
second database can run tests inside a plax instance without
out-of-band tooling. Each declared database is created on `up`,
migrated (if the base was), and dropped on `down`. Orphaned databases
on the Postgres server with no registry entry are surfaced by `doctor`.

---

## Package layout

```
pkg/blueprint/
  blueprint.go              # Add Databases field to ServiceDef
  validate.go               # Validate database declarations (no duplicate names)
pkg/registry/
  registry.go               # DBName string → DBNames map[string]string
pkg/instance/
  up.go                     # Clone multiple databases; values map gets per-DB vars
  down.go                   # Drop multiple databases from record
  resume.go                 # Restore per-DB values from record
  rederive.go               # Same
  instance_test.go          # Update fake BM and all tests for multi-DB
pkg/doctor/
  doctor.go                 # Check all DBNames in record; new orphan scan
pkg/derive/postgres/
  basemanager.go            # Add ListPlaxDatabases method for orphan scan
cmd/plax/
  e2e_test.go               # Add test-DB scenario: DATABASE_URL and DATABASE_TEST_URL
docs/plans/
  index.md                  # Add plan 11 to phase table
```

---

## Type specifications

### `pkg/blueprint/blueprint.go`

```go
type ServiceDef struct {
    Isolation ServiceIsolation   `json:"isolation"`
    Type      string             `json:"type,omitempty"`
    Image     string             `json:"image"`
    Env       map[string]string  `json:"env,omitempty"`
    Ports     map[string]PortDef `json:"ports,omitempty"`
    Command   []string           `json:"command,omitempty"`
    Databases []DatabaseDef      `json:"databases,omitempty"`
}

type DatabaseDef struct {
    Name string `json:"name"`        // key used in template vars: {{DB_NAME_<key>}}
    From string `json:"from"`        // "base" | "migrations"
}
```

- `From: "base"` — clone the fully seeded `plax_base` via TEMPLATE (today's
  behaviour).
- `From: "migrations"` — clone from the same template (getting schema +
  migrations), then skip the seed step so the database is empty of
  application data. ⚠ The "migrations" origin reuses the TEMPLATE mechanism
  from the live base (it has the applied-migrations table); the only
  difference is that it is deliberately not seeded.
- If `Databases` is nil or empty, behaviour is unchanged (one database named
  `plax_<name>` with key `""` / bare `DB_NAME`).

### `pkg/registry/registry.go`

```go
type InstanceRecord struct {
    // ... existing fields unchanged ...
    DBName  string            `json:"db_name,omitempty"`
    DBNames map[string]string `json:"db_names,omitempty"`   // NEW: key → physical DB name
}
```

`DBName` (singular) is **deprecated** in this plan — it is populated by `Up`
for backward compatibility with existing on-disk registries, but `Down` and
`doctor` prefer `DBNames`. A migration step on registry open populates
`DBNames` from `DBName` if the map is nil but the string is set, keeping old
registries readable.

A logical service with a single database (the default, pre-11) uses the
empty-string key:

```json
{
  "db_name": "plax_i1",
  "db_names": {"": "plax_i1"}
}
```

Additional databases use their declaration key:

```json
{
  "db_names": {"": "plax_i1", "test": "plax_i1_test"}
}
```

### `pkg/derive/postgres/basemanager.go` (additions)

```go
// ListPlaxDatabases returns every database name on the connected server that
// starts with the well-known "plax_" prefix (excluding "plax_base" and
// "plax_base_next"). Used by doctor to detect orphans.
func (bm *BaseManager) ListPlaxDatabases(ctx context.Context) ([]string, error)
```

No changes to `CloneBase` or `DropInstanceDB` — they already accept an
arbitrary `targetDB` / `dbName` string.

---

## Algorithms

### Database name construction

In `Up`, database names are built from the service's `Databases` slice:

```go
dbNames := map[string]string{}
for _, dbDef := range logicalSvc.Databases {
    key := dbDef.Name
    physicalName := "plax_" + name
    if key != "" {
        physicalName += "_" + key
    }
    dbNames[key] = physicalName
}
// Backward compatible: if no databases declared, do the same but with key "".
```

### Up changes

Current step 5 (`Clone database`, line 148 in `up.go`) becomes a loop over
`dbNames`. Hole values are populated for each key:

```go
for key, physicalName := range dbNames {
    if key == "" {
        values["DB_NAME"] = physicalName
    } else {
        values["DB_NAME_"+key] = physicalName
    }
}
```

`Render` already handles `{{DB_NAME_test}}` with no changes — the regex is
`\{\{(\w+)\}\}`.

⚠ Only logical services with `type: "postgres"` support `databases`. A
logical service of a different type (or one not yet implemented) that
declares `databases` should fail validation.

⚠ The `values["DB_NAME"]` assignment in step 3 (build values map, line 127
in `up.go`) is replaced with the loop above. The hardcoded `"plax_" + name`
for the single-database case is still needed as a fallback for blueprints
without `databases`.

Registry record:

```go
deps.Registry.AddInstance(name, registry.InstanceRecord{
    // ...
    DBName:  dbNames[""],  // backward compat
    DBNames: dbNames,
    // ...
})
```

### Down changes

Current step 4 (`Drop instance database`, line 66 in `down.go`) becomes a
loop over `rec.DBNames`. If `DBNames` is nil (old record), fall back to
`rec.DBName`:

```go
names := rec.DBNames
if names == nil {
    if rec.DBName != "" {
        names = map[string]string{"": rec.DBName}
    }
}
for key, dbName := range names {
    fmt.Fprintf(os.Stderr, "dropping database %s...\n", dbName)
    if err := deps.BM.DropInstanceDB(ctx, dbName); err != nil {
        fmt.Fprintf(os.Stderr, "warning: drop database %s: %v\n", dbName, err)
    }
}
```

⚠ Dropping is order-independent (no foreign keys between instance
   databases), so iteration order doesn't matter.

### Resume changes

Line 83 of `resume.go` — `values := map[string]string{"DB_NAME": rec.DBName}`
becomes populated from `rec.DBNames` with the same loop as Up.

### Rederive changes

Line 49 of `rederive.go` — same change.

### Doctor changes (orphan scan)

After `runBlueprintVsRegistry`, add a new phase `runOrphanDatabases`:

1. Call `bm.ListPlaxDatabases(ctx)` — get every `plax_*` database on the
   server.
2. Exclude well-known names: `plax_base`, `plax_base_next`.
3. Build a set of declared databases from `registry.Instances[*].DBNames`.
4. Any server database not in the declared set is orphaned:
   ```
   `plax_orphan_test` is an unreferenced database — run
   `psql -c "DROP DATABASE plax_orphan_test"` to remove it
   ```

⚠ ListPlaxDatabases queries `pg_database` directly. It skips databases
   where `datallowconn = false` — a locked orphan should still be reported
   (it consumes disk), so the query includes them but notes the lock.

⚠ If `DBNames` is nil in a record (pre-migration), `doctor` populates it
   from `rec.DBName` when building the declared set. It does not persist
   the migration — only `Up` writes new records.

---

## CLI specification

No new CLI commands or flags. Existing commands change behaviour as follows:

| Command | Change |
|---|---|
| `plax up <name>` | Creates all databases declared in the blueprint. Summary prints all names (e.g. `database: plax_i1 plax_i1_test`). |
| `plax down <name>` | Drops all databases in the record. |
| `plax doctor` | New check `Orphan databases` reports server-side `plax_*` databases with no registry entry. |

Commands with no change: `ls`, `attach`, `exec`, `suspend`, `resume`, `status`, `send`, `recv`, `base *`, `rederive`.

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| Database declaration with no logical postgres service | `ValidateStructural` — `databases` on a non-logical or non-postgres service | `service "<name>" declares databases but is not a logical postgres service`. Exit 1. |
| Duplicate database key | `ValidateStructural` — duplicate `Name` in `Databases` slice | `service "<name>": duplicate database key "<key>"`. Exit 1. |
| CloneBase fails for suffix DB (e.g. `_test`) | Postgres error | Wrapped error. Rollback drops all databases created so far. |
| DropInstanceDB fails for one of the databases | Postgres error | Logged to stderr with `warning:`, execution continues (same as today's single-DB behaviour). |
| Hole template references unknown `{{DB_NAME_*}}` | `Render` — var not in values map | Same as today: `template references unknown variable {{DB_NAME_xyz}}`. Rollback. |
| Old registry record has `db_name` but no `db_names` | `DBNames` is nil, `DBName` is set | Down and doctor fall back to `DBName`. Up always writes both. |
| `From: "migrations"` on a base without provenance | Clone → provenance query fails | `database "<name>": created from "migrations" but base has no provenance row — run 'plax base reset'`. The empty db still exists because `CREATE DATABASE ... TEMPLATE` succeeded, but the provenance update (which records a `source` field) failed. Rollback drops the empty db. |

---

## Tests

### Unit tests

**`pkg/blueprint/validate_test.go`:**

- `TestValidateStructural_DatabaseOnNonPostgres` — `databases` on a logical service with `type: "redis"` → error.
- `TestValidateStructural_DuplicateDatabaseKey` — two databases with `Name: "test"` → error.
- `TestValidateStructural_ValidDatabaseDeclarations` — two databases with distinct names → no error.

**`pkg/instance/instance_test.go`:**

- `TestUp_MultipleDatabases` — Blueprint with `databases: [{name:"test", from:"base"}]` → `DBNames` has two entries, both cloned, both dropped on cleanup.
- `TestDown_MultipleDatabases` — Record with two DB names → both dropped.
- `TestDown_OldRecord_NoDBNames` — Record with only `DBName` set → single database dropped via fallback.
- `TestUp_BackwardCompatible` — Blueprint with no `databases` → `DBNames` has only the `""` key, everything works as before.

**`pkg/doctor/doctor_test.go`:**

- `TestDoctor_OrphanDatabase` — `ListPlaxDatabases` returns a name not in any record → orphan warning.
- `TestDoctor_NoOrphans` — All server databases have registry entries → pass.
- `TestDoctor_OldRecord_Fallback` — Record with only `DBName` → correct set comparison.

### End-to-end test

`TestEndToEnd_TwoInstancesWithTestDB` — a variant of the existing e2e that
declares a `_test` database and verifies:

- Both `plax_i1` and `plax_i1_test` exist on Postgres after `up`.
- `DATABASE_TEST_URL` resolves to `postgres://.../plax_i1_test` inside the
  instance.
- `down i1` removes both databases.
- `doctor` after `down` reports no orphaned databases.
- Repeat with `i2` running concurrently — all four databases are distinct.

---

## Acceptance criteria

- [ ] `plax up i1` with a blueprint declaring `databases: [{name:"test", from:"base"}]` creates both `plax_i1` and `plax_i1_test`
- [ ] `plax down i1` drops both databases
- [ ] `{{DB_NAME_test}}` in a hole template resolves correctly at derivation time
- [ ] A blueprint with no `databases` field behaves identically to before this plan
- [ ] `plax doctor` reports each `plax_*` database on the server that has no registry entry
- [ ] An old registry file with only `db_name` (no `db_names`) is handled correctly by `down` and `doctor`
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (both with and without `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| No new external dependencies. | | | |

All work uses existing project packages:

- `github.com/apollopower/plax/pkg/blueprint`
- `github.com/apollopower/plax/pkg/registry`
- `github.com/apollopower/plax/pkg/instance`
- `github.com/apollopower/plax/pkg/derive/postgres`
- `github.com/apollopower/plax/pkg/derive/env`
- `github.com/apollopower/plax/pkg/doctor`
- `github.com/apollopower/plax/pkg/worktree`

Standard library additions: none.

---

## Deferred items

| Item | Deferred to | Reason |
|---|---|---|
| `From: "migrations"` origin (empty clone without seed data) | Later plan | The narrow case of an empty, post-migration clone is useful but not the immediate blocker. The `_test` databases can use `From: "base"` — they get the seed data, which is harmless for most test suites. Track separately. |
| Per-database seed commands | When a repo needs different seed data per database | The simplest case (seeded main DB + empty test DB) is covered by `From: "migrations"` when it lands. Different data per DB = different base refs = refactor of the base model. |
| Non-Postgres logical services with multiple databases | When a second logical driver lands | Only Postgres has a logical driver today. |

For the immediate fix, `databases` supports a single origin: `"base"`. The
`from` field is parsed but only `"base"` is accepted. This unblocks test
suites without the larger provenance work that `"migrations"` requires.
