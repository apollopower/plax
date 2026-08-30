# Plan 21 — Instance migrations during `plax up`

## Objective

Apply the blueprint's `seed.migrate` command to each newly cloned instance
before workloads start, with an explicit skip option and rollback on failure
(issue #75; triaged in `docs/plans/triage/2026-08-26.md`).

## Package layout

```
pkg/instance/command.go          # run a shell command in an instance worktree
pkg/instance/command_test.go     # derived-env, workdir, output, and failure tests
pkg/instance/up.go                # migrate phase and validated skip-step wiring
pkg/instance/up_test.go           # migration ordering and rollback scenarios
cmd/plax/main.go                  # UpCmd --skip parsing and output
cmd/plax/main_test.go             # CLI skip syntax and unknown-step failures
cmd/plax/e2e_test.go              # real migration against cloned instance databases
cmd/plax/guide.md                 # up command and skip-step reference
docs/manual.md                    # up behavior, migration, and skip documentation
docs/plans/index.md               # phase table entry for plan 21
docs/plans/triage/2026-08-26.md   # link plan 21 to issue #75 triage
```

The instance command helper owns process execution in an instance. It does
not own migration policy or database lifecycle. `up` decides when the
configured command runs; the Postgres base manager continues to own base
creation, refresh, cloning, and base-only seeding.

## Type specifications

### `pkg/instance`

```go
// CommandResult describes one completed instance command.
type CommandResult struct {
    Output string
}

// RunCommand executes command through the user's shell in the instance
// worktree (workdir relative to worktreePath, matching seed.workdir
// semantics) with the instance environment. It returns captured output on
// success and includes command output in errors on failure.
func RunCommand(ctx context.Context, worktreePath string, workdir string, ports map[string]int, command string) (CommandResult, error)
```

`RunCommand` uses `env.LoadInstanceEnv` to merge the host environment with
the instance's derived `.env` and allocated ports. It sets `Cmd.Dir` to
`worktreePath/seed.workdir` — the base path and process spawn both honor
`seed.workdir`, and migration tooling may live in a subdirectory — and runs
`sh -c command`, matching the blueprint's existing seed command model. The
result captures combined output so a migration tool's own summary can be
shown on failure, but output is not parsed for an applied count.

Two environment guards, both learned from the base path (plan 06 F3):

- A host `DATABASE_URL` must not redirect an instance command to a
  non-instance database: when the derived `.env` does not define
  `DATABASE_URL`, the variable is stripped from the command environment.
  The migration then fails loudly if the repository's tooling needs it,
  rather than silently targeting the wrong database.
- A blueprint without `env.template` has no derived `.env`; the migration
  step fails with a message naming `env.template` as the cause, never the
  misleading "was the instance created with 'plax up'?" text.

```go
// UpOptions controls optional provisioning steps.
type UpOptions struct {
    Skip map[string]bool
}
```

`UpOptions` is internal to lifecycle orchestration initially. Its accepted
names are defined centrally as `migrate` and `verify`; callers cannot add
arbitrary names through the CLI.

### Migration count

No new persisted type is required. When `seed.applied_migrations` is
configured, `up` may read the live applied set before and after the command
using the existing `BaseManager.AppliedMigrations` method and report the
set-difference count. When it is not configured, the output reports the step
without a count. Command stdout is never treated as a stable protocol.

## Algorithms

### Skip parsing and validation

```
plax up [--skip migrate,verify] i1

1. Split each --skip value on commas and trim whitespace.
2. Reject an empty name.
3. Reject any name not in the fixed set {migrate, verify}.
   ⚠ Validate all names before creating a branch, network, port, database,
   or other side effect; a typo must not silently run a skipped step.
4. Store the result as a set so repeated names are harmless.
```

The CLI may accept repeated flags as well as comma-separated names if Kong's
slice handling makes that natural, but both forms must resolve to the same
set and documentation must show one canonical form.

### Instance command execution

```
RunCommand(ctx, worktreePath, workdir, ports, command)

1. Reject an empty command before starting a process.
2. Fail clearly when the worktree has no derived `.env` (blueprint without
   `env.template`), naming the template as the cause.
3. Load worktree `.env` and merge it over the host environment.
4. Strip `DATABASE_URL` from the merged environment when the derived `.env`
   does not define it, so a stray host value cannot redirect the command.
5. Layer allocated port variables over the merged environment.
6. Create `sh -c command` with `Cmd.Dir = worktreePath/seed.workdir`.
7. Connect stdout and stderr to the caller's stderr for visible `up` output
   while also collecting bounded output for diagnostics.
8. Run with ctx so cancellation terminates the command.
9. Return a wrapped error containing the command output on non-zero exit.
   ⚠ Never run this helper with the base manager's repository root or base
   DSN; doing so can migrate the base instead of the instance.
```

The exact output-capture limit must be fixed in implementation so a broken
migration cannot consume unbounded memory. The complete command output need
not be retained when it has already streamed to stderr.

### Migration phase in `up`

```
up i1

1. Validate blueprint, name, existing instance, base status, and skip names.
2. Create branch/worktree, scratch, mailbox, network, ports, and derived .env.
3. Clone every logical database from base.
4. If `migrate` is not skipped:
   a. Print `applying migrations...` to stderr.
   b. If configured, snapshot applied migrations for the primary instance
      databases using the live-set API.
   c. Run `seed.migrate` exactly once in the instance worktree's
      `seed.workdir` with the instance's derived environment.
   d. If configured, read the live sets again and report the count of newly
      applied identifiers. Otherwise report successful completion without a
      fabricated count.
5. Start dedicated containers and native processes.
6. Perform the existing settle check.
7. Register the instance and run verification unless `verify` is skipped.
8. Report success.
```

Migration runs once because a repository migration command may intentionally
operate on both the primary and test databases through multiple derived env
variables. Do not loop over `dbNames` and invoke the command once per DB;
that could repeat migrations or produce incorrect cross-database behavior.

The command runs after all clones and before workloads. This ensures an app
that reads schema at startup sees the migrated instance, and ensures a
migration failure is handled by the existing pre-success rollback path.

**⚠ `BaseManager.runMigrate` is not suitable here.** It runs in the base
repository workdir and overrides only `DATABASE_URL` for a base-manager DB
name. Reusing it can target the base and misses instance-specific variables
such as test or additional database URLs. `RunCommand` must be the instance
path.

### Migration count

```
if seed.applied_migrations is configured:
    before := live applied identifiers for every declared logical database
    run seed.migrate once
    after := live applied identifiers for every declared logical database
    count[db] := number of identifiers in after[db] - before[db], per database
    total := sum(count[db] for every declared database)
    print the deterministic total (and per-database detail when >1)
else:
    print migration success without a count
```

⚠ If the command succeeds but the after-read fails, `up` must fail rather than
claim a count that was not measured. Whether the databases have already been
migrated does not change the command's success contract; rollback drops the
clones. A configured migration table that cannot be queried is a provisioning
failure, not permission to fall back to parsing stdout.

The total is a sum of per-database applications, not a union of migration
identifiers. If migration `001` is applied to both the primary and test
databases, it contributes two to the total because two database changes were
made. Per-database counts make that interpretation visible.

### Failure and rollback

```
if RunCommand fails:
    return an error naming the instance migration step
    execute normal up cleanup in reverse order
    do not write a registry entry or report a healthy instance
```

The migration phase is before registry creation, so the existing cleanup stack
already drops cloned databases, releases ports, removes services/network,
removes the mailbox, and removes the worktree. No destructive `db drop` or
`db reset` command is ever invoked; only the configured `seed.migrate`
command runs.

### Verification skip

When `verify` is skipped, `up` still performs the settle check that detects
immediate process/container exit. It omits the later `verify.RunVerify` call
and must say so in stderr. This does not mark the instance healthy by
verification; it only suppresses the explicit verification phase.

## CLI specification

### `plax up`

```
plax up [--skip <step>[,<step>...]] <name>
```

- `--skip migrate` creates databases from the base schema without applying
  the instance migration command.
- `--skip verify` starts the workloads and performs the settle check without
  running the later verification phase.
- Multiple names are comma-separated; repeated `--skip` flags may also be
  accepted if Kong provides a clean equivalent.
- Valid names are exactly `migrate` and `verify`.
- Unknown or empty names fail with non-zero exit before side effects.
- Migration output and step status go to stderr. Records and machine data do
  not change in this phase.
- Existing `plax up <name>` behavior changes by design: migration is enabled
  by default.

### Output

Default successful path when count metadata is available:

```
cloning database plax_i1...
applying migrations...
migrations: 10 applied
starting redis...
starting app...
verification: 7 check(s) passed
```

When count metadata is unavailable:

```
applying migrations...
migrations: complete (applied count unavailable)
```

The exact wording should remain deterministic and should not imply that a
zero count was measured when `applied_migrations` is absent.

## Error handling

| Failure | Expected behavior |
|---|---|
| Unknown `--skip` step | Reject before side effects → list valid names |
| Empty `--skip` item | Reject before side effects → identify the empty step |
| Empty `seed.migrate` | Blueprint validation already rejects it → `up` does not bypass validation |
| No derived `.env` (no `env.template`) | Fail migration phase naming the missing template → rollback clones and all resources |
| Instance `.env` missing or malformed | Fail migration phase → rollback clones and all resources |
| Host exports `DATABASE_URL`, derived `.env` does not | Strip the host value from the migration environment → the command fails loudly if it needs one, never targets a non-instance database |
| Migration command exits non-zero | Include command output → rollback and return non-zero |
| Migration command canceled | Return context error → kill the whole process group so orphans cannot block rollback; rollback uses the uncanceled cleanup context |
| Live migration count read fails | Fail rather than fabricate count → rollback |
| Migration command succeeds with zero new identifiers | Report measured `0 applied` → continue |
| `applied_migrations` is absent | Run migration → report completion without a count |
| Multiple logical databases declared | Run command once with all derived URLs → count per DB if measurable |
| `--skip migrate` | Do not execute `seed.migrate` → continue from cloned base schema |
| `--skip verify` | Omit `verify.RunVerify` → retain settle check and state that verification was skipped |
| Workload fails after successful migration | Existing workload rollback behavior → migration is not rerun during cleanup |
| Migration command attempts `db drop`/`db reset` | Plax cannot reliably inspect command intent → repository command contract documents that only migration is safe; any command failure rolls back clones |

## Tests

### Unit tests

- `TestRunCommand_UsesInstanceEnvAndWorktree` — command sees derived `.env`,
  allocated ports, and instance working directory rather than base values.
- `TestRunCommand_HonorsWorkdirSubdirectory` — `seed.workdir` is joined
  onto the worktree path, matching base and process-spawn semantics.
- `TestRunCommand_StreamsOutputAndIncludesFailureOutput` — output is visible
  and non-zero exit errors retain useful diagnostics.
- `TestRunCommand_CancellationStopsProcess` — context cancellation kills the
  whole process group, so a forking migration cannot block `Wait` on pipes
  held by orphaned children.
- `TestRunCommand_StripsHostDATABASE_URL_WhenNotDerived` — a host
  `DATABASE_URL` never reaches the command when the derived `.env` does not
  define it.
- `TestRunCommand_KeepsDerivedDATABASE_URL` — the derived value wins over
  the host value.
- `TestRunCommand_MissingEnv_FailsWithClearMessage` — a blueprint without
  `env.template` fails naming the template, not with a generic message.
- `TestParseSkip_AcceptsCommaSeparatedNames` — valid names become a set.
- `TestParseSkip_RejectsUnknownName` — typo fails validation.
- `TestParseSkip_RejectsEmptyName` — malformed comma lists fail.
- `TestMigrationCount_DiffersAppliedSets` — count is calculated from live
  before/after sets, including multiple databases.
- `TestMigrationCount_UnconfiguredHasNoFabricatedCount` — absent metadata
  does not produce `0` or parse command output.

### Lifecycle tests

- `TestInstance_Up_MigratesBeforeWorkloads` — migration command runs after
  clone and before container/process start.
- `TestInstance_Up_MigrationRunsOnceForMultipleDatabases` — one command sees
  all derived database URLs.
- `TestInstance_Up_MigrationRunsInSeedWorkdir` — the command's working
  directory is `worktreePath/seed.workdir`.
- `TestInstance_Up_MigrationCountReported_WhenConfigured` — the before/after
  live-set reads and the measured count report are exercised through `Up`
  with a stateful fake.
- `TestInstance_Up_MigrationFailureRollsBack` — failed migration removes
  clones, worktree, network, ports, mailbox, and leaves no registry record.
- `TestInstance_Up_SkipMigrateDoesNotRunCommand` — skipped migration reaches
  workload startup without invoking the command.
- `TestInstance_Up_SkipVerifyOmitsVerification` — settle check remains while
  `verify.RunVerify` is not called.
- `TestInstance_Up_UnknownSkipHasNoSideEffects` — validation precedes branch
  and resource creation.

### End-to-end tests (`cmd/plax/e2e_test.go`)

The real-Git/Postgres scenario follows the repository convention of calling
`t.Skip` when `PLAX_TEST_POSTGRES_URL` is unset. It should create a fixture
with a migration that is absent from the base, run real `plax up`, verify the
migration is present in the instance database and absent from the base, then
run `plax up --skip migrate` for a second instance and verify the migration is
absent there. The scenario should also cover a failing migration and confirm
that no instance remains registered.

## Acceptance criteria

- [x] Plain `plax up i1` runs `seed.migrate` exactly once after all instance
  databases are cloned and before any workload starts.
- [x] The migration command runs in the instance worktree with its derived
  environment and all derived database URLs; the base database is untouched.
- [x] A migration failure returns non-zero, includes useful command output,
  removes all resources created by `up`, and leaves no healthy registry entry.
- [x] `plax up --skip migrate i1` does not run the migration command and leaves
  the instance on the cloned base schema.
- [x] `plax up --skip verify i1` omits the explicit verification phase while
  retaining the immediate workload settle check and clearly reports the
  skipped verification.
- [x] `--skip` accepts the documented names, rejects unknown or empty names
  before side effects, and has deterministic behavior for repeated names.
- [x] When `seed.applied_migrations` is configured, output reports the measured
  number of newly applied identifiers; when absent, output does not invent a
  count or parse framework-specific stdout.
- [x] Multiple logical databases receive one migration command invocation, with
  count detail measured per database when metadata permits.
- [x] Guide, manual, plan index, and the 2026-08-26 triage snapshot document the
  changed default and skip behavior.

## Dependencies

No new external Go modules are required.

- Standard library: `context`, `errors`, `fmt`, `io`, `os`, `os/exec`,
  `path/filepath`, `sort`, `strings`, and `time` for command execution,
  output handling, skip parsing, and deterministic reporting.
- Existing internal packages: `pkg/derive/env` for instance environment
  loading, `pkg/derive/postgres` through the existing `BaseManager` interface
  for live applied-migration reads, `pkg/verify`, `pkg/registry`, and
  `pkg/worktree`.
- Existing CLI dependency: `github.com/alecthomas/kong v1.16.0`.

The implementation must not invoke base-oriented migration plumbing for an
instance, add a migration-output parser, add a destructive reset/drop path,
or introduce a daemon.
