# Plan 23 — Database deadlines and interruption

## Objective

Bound Plax-owned Postgres work and failure cleanup, make migration/seed scripts reliably interruptible, and report timeouts without hiding incomplete teardown ([#82](https://github.com/apollopower/plax/issues/82)).

**Status: draft for maintainer review; no implementation yet.**

Agreed policy:

| Boundary | Policy |
|---|---|
| Initial Postgres connection | 10 seconds |
| Database-only operation/phase | 60 seconds shared across its database calls |
| Each database rollback/recovery attempt | 10 seconds, independent of the failed/canceled operation |
| Forward database drop during `down` | 60 seconds per database; not a rollback attempt |
| External migration/seed command | No automatic timeout; SIGINT/SIGTERM stops its process group |
| Whole CLI command | No overall deadline |
| Configuration | Fixed defaults; no new flag, environment variable, or blueprint field |
| Refresh swap | Retry database-in-use errors only; other failures are not deferred swaps |
| `down` | Continue teardown after database timeout/interruption, but return non-zero |
| Signals during `down` | First SIGINT/SIGTERM cancels DB work; after acknowledgement, a further signal forces termination |

The integration details below are proposals for this plan's review. This plan
does not fix shared-test-server starvation (#83), add infrastructure to CI,
change Rails support, or authorize a push, PR, or release.

## Package layout

```text
pkg/derive/postgres/
  timeout.go                  # fixed policy, private duration injection, error wrapping
  timeout_test.go             # phase budgets, parent cancellation, real stalled TCP connection
  basemanager.go              # phase boundaries, bounded recovery, swap error classification
  basemanager_test.go         # database locks, cleanup, swap retries, clone timing, regressions
  command_test.go             # base migration/seed process-group interruption
  provenance.go               # bound standalone provenance calls; preserve parent phase deadlines
pkg/instance/
  up.go                       # immediate clone rollback ownership; private cleanup-duration seam
  down.go                     # continue teardown while retaining timeout/cancellation errors
  instance_test.go            # context-aware lifecycle fakes and teardown assertions
  up_test.go                  # partial multi-DB failure and interrupted migration rollback
  command.go                  # retain instance process-group cancellation; reject canceled starts
  command_test.go             # child termination and no implicit script deadline
pkg/doctor/
  doctor.go                   # retain connection cause and mark timed-out DB checks failed
  doctor_test.go              # timeout severity and connection diagnostics
pkg/status/
  status.go                   # return cancellation errors; retain degraded timeout diagnostics
  status_test.go              # cancellation propagation and timeout/degraded-report diagnostics
cmd/plax/
  main.go                     # signal contexts, down signal escalation, caller error propagation
  main_test.go                # callers, signal escalation, and infrastructure-free TCP regression
  e2e_test.go                 # real-binary SIGINT and cleanup regression scenarios
  guide.md                    # deadlines, script interruption, incomplete-down recovery
  guide_test.go               # bundled guide contract where assertions are needed
docs/
  manual.md                   # policy, failure semantics, recovery instructions
  plans/index.md              # plan 23 inventory entry
```

No new domain package is needed. Review README command/help claims for this
change, but do not rewrite its narrative or fold the separate README-accuracy
task into this issue. Update factual reference text only if it is affected.

No internal `pkg/verify` change is expected: `CheckDatabases` already turns
query errors into failed checks. The CLI must stop converting initialization
timeout/cancellation into a nil backend followed by healthy verification.

## Type specifications

### Private Postgres policy

```go
type timeouts struct {
    connect   time.Duration // constructor/ping budget; default 10s
    operation time.Duration // one database phase; default 60s
    cleanup   time.Duration // one recovery attempt; default 10s
}
```

Add an unexported `timeouts timeouts` field to `BaseManager`. Keep the public
`NewBaseManager` signature unchanged; delegate to a private constructor taking
explicit durations so package tests can use milliseconds. Reject non-positive
durations in that constructor. Do not use mutable package globals or a
test-only production environment variable.

These are private execution settings, not persisted data; no JSON tags or
blueprint/registry schema changes apply. Existing exported report structs and
their JSON shapes remain unchanged.

Expose only the fixed `postgres.DefaultCleanupTimeout` duration constant needed
by lifecycle orchestration. Its default is 10 seconds. Keep the exported
`instance.Up` signature unchanged and delegate to these private entry points:

```go
func up(ctx context.Context, deps *Deps, name string, opts UpOptions, cleanupTimeout time.Duration) error
func dropForRollback(ctx context.Context, bm BaseManager, dbName string, cleanupTimeout time.Duration) error
```

`Up` calls `up` with `postgres.DefaultCleanupTimeout`. The private `up` rejects
a non-positive cleanup duration before side effects and passes its duration to
every registered database rollback. `dropForRollback` creates the fresh bounded
cleanup context when invoked, calls `DropInstanceDB`, and cancels the context.
The rollback-continuation test calls `up` with a short duration and a fake drop
that blocks on `ctx.Done()`, exercising the actual cleanup stack rather than
only the helper. No mutable global, test-only environment variable, or new
`instance.Deps` field is needed.

`Down` does not need a cleanup-duration override: forward database drops use
the BaseManager's operation policy, and cancellation skips unstarted DB drops.
Its unit fakes inject operation errors/cancellation; Postgres package tests
verify the real operation deadlines separately.

The 10s budget is exclusively for rollback/recovery such as failed-provisioning
drops and relocking. Normal `down` is a forward teardown operation: it gives
each database a fresh 60s operation budget, with no automatic retry or detached
10s retry after a timeout. N unresponsive databases can therefore consume up to
N × 60s of database work, plus the initial connection and non-DB teardown.
This is not an overall command limit; a handled first signal stops the current
DB call and skips all remaining drops. Do not silently shorten successful
forward drops to the rollback budget.

The private CLI signal helper has this signature:

```go
func downSignalContext(parent context.Context) (context.Context, context.CancelFunc)
```

It is the sole signal registration for `down`; do not wrap its parent in
another `signal.NotifyContext` that would keep intercepting the second signal.
Its returned cancel function releases both registration and worker resources.

### Errors and test seams

Use existing wrapped Go errors and `errors.Join`; preserve
`errors.Is(err, context.DeadlineExceeded)` and
`errors.Is(err, context.Canceled)`. Keep `ErrDeferredSwap` exclusively for
exhausted database-in-use retries. No new exported error struct is required.

For swap tests, a private, narrow executor interface is sufficient:

```go
type dbExecer interface {
    Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}
```

The production executor is the existing pgx pool. Do not replace the complete
pgx API with a mock layer. Lifecycle tests continue using the existing
`instance.BaseManager` interface with per-package, mutex-guarded fake call
recording.

### Diagnostic dependencies

Add this field to the existing `doctor.Deps` and `status.Deps` structs:

```go
BMErr error `json:"-"`
```

This optional, non-serialized field carries a backend initialization
failure to the report builder; nil means no initialization error was supplied.
The CLI sets it only when `BM` is nil because initialization failed. Do not
fabricate a connected backend or infer a timeout by parsing an error string.

The JSON exclusion is defensive, not a sanitization mechanism: these dependency
structs are not currently serialized. Wrap connection failures before they
reach `BMErr` or a CLI diagnostic, retaining the original error for inspection
but never interpolating its raw text into messages:

```go
type connectionError struct {
    cause error // original cause for Unwrap; never rendered directly
}
```

Its `Unwrap() error` returns `cause`; `Error() string` uses an allowlisted
connection-failure description (timeout, cancellation, authentication failure,
or generic connection failure, with a PostgreSQL SQLSTATE when available).
Outer wrappers supply the operation and managed database name, not the DSN.
Cover both initial and later per-database connection failures, including lazy
connections that fail during pool acquisition/query rather than pool creation.
Do not rely on stripping only the URL userinfo: pgx parse errors may echo a DSN, including
percent-encoded passwords or credential-bearing query parameters. Report
builders format this safe wrapper, never bypass it by printing the unwrapped cause.

Doctor includes the cause in its existing failed Postgres check and marks
operation deadline/cancellation errors in database checks as failures rather
than warnings. Status uses the cause in the existing unknown schema/data
dimensions. Neither change adds a report JSON field or changes ordinary
non-timeout warning severity.

## Algorithms

### Connection and phase deadlines

```text
1. CLI creates a SIGINT/SIGTERM-aware parent context before opening Postgres.
2. NewBaseManager derives one 10s connection context for pool creation + Ping.
3. Configure pgx connection establishment to be bounded by 10s as well,
   including replacement connections and per-database pools.
4. Each database-only entry point derives a 60s phase context from its parent.
5. All constituent queries, Exec calls, pool acquisition, and row iteration
   use that context; cancel it on every return path.
6. Wrap errors with operation and database names; retain the original cause.
   ⚠ An earlier caller deadline wins. Nested helpers must not restart a
   budget or detach from parent cancellation.
   ⚠ A connection limit is not a query timeout. Configuring ConnectTimeout
   alone does not address an already-connected, unresponsive server.
```

Database-only entry points include `CloneBase`, `DropInstanceDB`,
`InstanceProvenance`, `AppliedMigrations`, `InstanceDBExists`, `BaseStatus`,
and `ListPlaxDatabases`. Standalone `CreateProvenance` and `ReadProvenance`
calls also receive a default operation bound, honoring shorter caller budgets.

`BaseStatus` shares one operation budget across base existence, provenance,
and staged-base queries. It must no longer discard provenance errors as it
currently does. Missing provenance remains a valid absence; an error reading
it is not absence.

### Mixed database/script operations

```text
CreateBase:
  bounded DB setup -> migrate(parent context) -> bounded provenance/lock phase
SeedBase:
  bounded DB check/unlock -> seed(parent context) -> bounded row-count/provenance phase
  -> bounded relock, including on failure
ResetBase:
  bounded drop phase per managed DB -> CreateBase(parent context)
RefreshBase:
  bounded staging checks/create -> migrate(parent context)
  -> bounded provenance phase -> seed(parent context)
  -> bounded row-count/lock phase -> bounded swap phase
```

If an existing staged base is ready, refresh skips staging and enters the
bounded swap phase. If no current base exists, retain the existing create path
only when there is no staged database requiring recovery.

⚠ Never pass a database-phase context to a migration/seed script. The phase
before the script must be canceled, and the phase after it must get a fresh
budget. Filesystem work does not gain a hard timeout merely by receiving a
context; this plan bounds database I/O, not every possible operating-system
stall.

Make row-count polling sleeps context-aware so cancellation does not wait for
an unconditional sleep to finish.

### Script interruption

```text
1. Check the parent context before starting the command.
2. Start sh -c in its own process group, preserving the existing environment,
   workdir, and DATABASE_URL safety rules for that command path.
3. Wait using the existing goroutine + result-channel pattern.
4. On SIGINT/SIGTERM cancellation, kill the command process group and reap it.
5. Return a wrapped context cancellation error, naming migrate or seed.
6. Run bounded recovery/rollback; do not report successful provisioning.
   ⚠ Killing only sh can leave children holding output pipes, hanging Wait.
   ⚠ Commands that deliberately leave the process group are outside this
   guarantee; do not claim arbitrary daemon containment.
```

Apply the instance command runner's existing group-kill approach to the base
manager's command runner, which currently uses `exec.CommandContext` on the
shell alone. Do not couple the Postgres package to `pkg/instance`, change
base/instance environment semantics, or add automatic script timeouts.

### Bounded cleanup and uncertain outcomes

```text
For each rollback drop or lock-restoration attempt:
1. Derive context.WithTimeout(context.WithoutCancel(parent), 10s) at the moment
   cleanup begins, not when the provisioning operation starts.
2. Use that one budget for all work in the attempt (for example, terminate
   connections + drop one database). Nested helpers do not get another 10s.
3. Cancel the cleanup context after completion.
4. Join cleanup failures to the original database operation error, or print
   an explicit rollback warning where lifecycle cleanup already reports to stderr.
5. Continue later cleanup attempts even if this one fails.
```

Cover failed base creation, seed relocking, refresh staging cleanup, temporary
unlocking for provenance, and partial clone cleanup. Restore a lock only if
this operation changed it. Preserve both read and relock errors rather than
overwriting the first failure with the second.

⚠ Register rollback for each successful clone immediately, not after the
multi-database clone loop. Today, a later clone failure can strand earlier
clones because the cleanup stack is populated only after the loop finishes.

⚠ A timed-out CREATE/DROP/ALTER may have completed on the server even when the
client did not receive confirmation. Attempt targeted recovery only for a
managed resource this operation owns; never drop a database rejected by the
pre-existing-target check. Name uncertain outcomes and failed cleanup rather
than claiming that timeout proves no side effect occurred.

These are database cleanup limits, not a new blanket Docker/git/filesystem
timeout policy. Commands with multiple databases can exceed a single cleanup
budget because each resource receives an independent attempt.

### Refresh swap

```text
1. Create one 60s context for the entire swap, including termination, retries,
   backoff, drop, and rename.
2. Retry DROP only for PostgreSQL SQLSTATE 55006 (object_in_use).
3. Keep the existing retry schedule; each attempt shares the original deadline.
4. Honor cancellation during backoff and check it before classifying an error.
5. Exhausted object_in_use retries -> ErrDeferredSwap; keep base_next staged.
6. Timeout, cancellation, connection failure, permission error, or any other
   failure -> return that operation's error, not ErrDeferredSwap.
7. Do not discard errors from connection termination between retries.
   ⚠ A failed/timed-out rename after DROP does not imply the old base survived.
   Retain staged data where possible; give recovery guidance, not blind retry advice.
```

Do not redesign the swap into a transactional/atomic replacement in this
issue. If the current base is absent while a staged database remains, report
an interrupted swap instead of silently creating a replacement base. Recovery
guidance must identify the database names and explain that `plax base reset`
discards staged state; it must not automatically perform that reset.

### CLI callers and `down`

```text
1. Use signal-aware contexts for base commands, up, down, resume, verify,
   status, and doctor wherever they initialize/use BaseManager.
2. Do not turn timeout/cancellation into a nil backend followed by success:
   - up/base: return the error; run any necessary rollback first.
   - resume: abort on initialization timeout/cancellation before starting workloads.
   - verify: return initialization timeout/cancellation without recording a healthy
     verification; query timeouts remain failed database checks.
   - doctor: retain the cause in diagnostics; a database timeout is a failed check.
   - status: keep report-oriented degraded output, mark affected DB dimensions
     unknown, and show the cause; cancellation returns non-zero.
3. Preserve existing degraded behavior for other backend failures unless this
   plan explicitly changes it. No generic exit-code redesign is included.
4. For down, remember a database initialization timeout instead of silently
   skipping it. Continue teardown of other resources.
5. For a per-database drop timeout, remember the error, warn with the database
   name, and continue with later teardown steps.
6. On parent cancellation, cancel the in-flight DB call and start no further
   DB drops/retries. Record its uncertain target plus every unstarted DB as
   residual; continue remaining non-database teardown with an uncanceled context.
7. Still remove the registry entry/port allocations as existing down semantics
   require; join registry errors with retained timeout/cancellation errors.
8. Return non-zero if database timeout or interruption occurred. Print an
   incomplete-teardown summary instead of an unqualified success message.
   ⚠ Registry removal means a second down cannot locate an orphaned DB.
   Name residual DBs and direct the operator to doctor/inspection once Postgres
   recovers; do not promise that simply rerunning down removes them.
```

The `doctor` and `status` diagnostic integration above is proposed for review:
preserve existing JSON shapes and put detailed causes into their existing
message/detail fields. A status report containing unknown dimensions is not a
successful database check, even though the report command may exit 0.

### `Down` database/non-database context split

```text
1. At entry to instance.Down, retain the original ctx as dbCtx and derive
   teardownCtx := context.WithoutCancel(ctx), before any teardown begins.
2. Pass dbCtx only to BaseManager DB drops. Check dbCtx.Err() before each
   drop; parent cancellation records all unstarted DBs as residual and stops
   that loop. A per-operation timeout does not cancel dbCtx, so later DBs
   still receive their own forward-operation budgets.
3. Pass teardownCtx to every Docker StopService, RemoveService, and RemoveNetwork
   call, including the container steps before the DB loop. Never pass dbCtx
   to Docker cleanup or detach the DB calls from it.
4. Native process termination retains its existing finite grace period;
   local worktree/mailbox/registry cleanup continues as before. Join dbCtx
   cancellation with other retained failures before returning.
   ⚠ Deriving teardownCtx only after the DB loop is too late: a signal while
   stopping containers must not cancel their stop/remove requests.
```

`runDown` passes the cancellable signal context to both backend initialization
and `instance.Down`; the latter owns this internal two-context split. No
public signature change or global Docker timeout is introduced. Fakes must
inspect their supplied contexts, not merely record that cleanup was called.

### Status cancellation propagation

```text
1. runStatus returns a wrapped initialization cancellation immediately; it
   does not turn that error into BMErr and render a degraded-success report.
2. status.Build checks for context.Canceled at entry, after live health,
   before/after each DB-facing report stage, and before returning its report.
3. If observed, return nil plus a wrapped ctx.Err(), preserving errors.Is.
   Do not continue subsequent DB stages or return (report, nil).
4. runStatus returns any Build error and rechecks parent cancellation before
   emitting stdout, preventing a completed report from masking an observed SIGINT.
5. A database-phase DeadlineExceeded still produces Unknown dimensions with
   details and a nil Build error, preserving the degraded-report exit-0 policy.
   ⚠ Inspect the parent context/error identity, never the formatted Detail text.
```

### `down` signal escalation

This deliberately replaces `runDown`'s existing non-signal-cancellable
rationale. Deadlines bound a database wait, but operators also need to stop
waiting on demand without immediately abandoning every remaining teardown
step. Idempotent resource operations still help recovery; they do not make
an orphaned database discoverable by a second `down` after registry removal.
Update the old rationale comment alongside the behavior and operational docs.

```text
1. A private CLI downSignalContext(parent) helper owns SIGINT/SIGTERM handling.
2. First SIGINT or first SIGTERM: restore the default disposition for both
   signals before canceling the command context. Then acknowledge on stderr
   that DB work is canceled and non-DB teardown continues. The active DB call
   stops; no later DB drop or retry starts.
3. List unconfirmed/unstarted databases and say teardown is continuing; remaining
   process/container/network/worktree/mailbox/registry cleanup uses an uncanceled
   context. If it completes without a second signal, return cancellation, exit 1.
4. A further SIGINT or SIGTERM delivered after acknowledgement uses default
   OS termination, including during cleanup. Forced termination can leave any
   resource or registry write incomplete and does not run deferred cleanup reliably.
5. Release the helper's signal registration/waiting goroutine on every normal
   return as well; do not modify signal handling in pkg/instance.
   ⚠ A DB phase timeout alone does not cancel the parent: subsequent DB drops
   still get their normal budgets. Only parent cancellation skips them.
   ⚠ Standard POSIX signals may coalesce, and a signal delivered before the
   first is handled/defaults are restored can be lost. Do not promise lossless
   escalation for two back-to-back signals or add a signal-counting framework.
```

Tests send signals to a dedicated child PID, not the test runner. Cover each
first-signal choice (SIGINT and SIGTERM), then wait for acknowledgement emitted
after default dispositions are restored before sending the next signal.
Verify OS signal termination, not Kong exit 1, for this spaced-signal contract.
The acknowledgement must not claim every rapid repeated signal is guaranteed
to be counted, and the manual/guide must state the same delivery limitation.
This escalation policy adds no overall Docker/git/filesystem deadline.

## CLI specification

No command syntax or flags change. Existing forms remain:

```text
plax base create|seed|reset|refresh|status [--pg-url <url>] [--root <path>]
plax up <name> [existing up options] [--pg-url <url>] [--root <path>]
plax down <name> [--pg-url <url>] [--root <path>]
plax resume <name> [--pg-url <url>] [--root <path>]
plax verify <name> [--json] [--pg-url <url>] [--root <path>]
plax status <name> [--json] [--pg-url <url>] [--root <path>]
plax doctor [--json] [--pg-url <url>] [--root <path>]
```

`base status` additionally retains `--json`; other base subcommands do not
gain it. Existing `--root` defaults and short aliases remain unchanged.

- Healthy paths exit 0, with existing runtime-verification behavior unchanged.
- Database timeout or interruption exits 1 for base/up/down/resume/verify;
  doctor retains its report-based exit 1 for failed checks.
- `base refresh` exits 2 only for exhausted database-in-use retries.
- `status` retains degraded-report exit 0 on database timeout, with unknown
  database dimensions and visible cause; SIGINT/SIGTERM cancellation exits 1.
- Records/reports remain on stdout; operational progress, timeout errors,
  cleanup warnings, and recovery advice go to stderr. Existing report fields
  can carry failure details in JSON; never mix human chatter into JSON stdout.
- Deadline diagnostics name the operation and target, for example:
  `clone plax_i1: timed out after 1m0s: context deadline exceeded`.
- Cancellation is reported as cancellation, not timeout. Credentials/DSNs must
  not be introduced into diagnostics.
- First SIGINT/SIGTERM during `down` cancels DB work and continues non-DB
  teardown, returning exit 1 if it finishes. After its acknowledgement, a further
  SIGINT/SIGTERM forces normal OS termination; shell-reported status is
  signal-derived. Back-to-back signals before handling may coalesce/be lost.

Connection and operation limits are separate: `base status` can spend up to
roughly 10s connecting plus 60s reading, and a failed temporary unlock may
require an additional 10s relock attempt. These are not hard whole-process
limits. Scripts can run indefinitely until interrupted, and a legitimate
database clone longer than 60s can fail; document both limitations.

The maintainer explicitly accepted the fixed 60s limit and lack of an escape
hatch during plan discussion. It applies to clone and each refresh database
phase too, even when slow work is legitimate; it is not a claim that target
bases have been benchmarked to finish in 60s. Do not silently add a larger
clone allowance or configuration. Revisit that policy with the maintainer if
representative workload measurements show a need.

Implementation acceptance includes a timed template-copy measurement on the
generic seeded fixture specified below. Record its actual size and timings;
a small or synthetic fixture cannot establish that all production-sized
bases fit the limit. A fixture failure or timeout must be brought back to the
maintainer before declaring this work complete, not hidden by increasing the limit.

## Error handling

| Failure mode | Expected behavior |
|---|---|
| TCP accepts but never completes Postgres startup | Connection deadline → named error, exit 1, no indefinite wait |
| Established connection stops responding | Shared database-phase deadline → preserve deadline cause and target |
| Provenance read times out in `base status` | Propagate error → do not print an apparently complete base report |
| Parent already canceled or has an earlier deadline | Honor parent → no new script starts or extended DB allowance |
| External migrate/seed remains busy | No automatic timeout → allow legitimate long work; SIGINT/SIGTERM cancels it |
| External script interrupted | Kill process group and reap → context error plus bounded rollback/relock |
| Clone fails after target creation, or CREATE outcome is uncertain | Targeted owned-resource cleanup → report residual/uncertain database if cleanup fails |
| Later clone fails in a multi-database `up` | Immediately registered earlier cleanups run → no silent loss of rollback ownership |
| Postgres remains unavailable during rollback | Fresh cleanup budget expires → warning names remaining resource; continue other cleanup |
| Provenance read and relock both fail | Join both errors → retain failure cause and unsafe lock-state warning |
| Swap repeatedly gets SQLSTATE 55006 | Existing retry budget exhausted → staged base retained, exit 2 |
| Swap gets timeout/cancellation/permission/network error | Return actual failure → exit 1, never label it database-in-use |
| Swap drops old base but cannot confirm rename | Preserve staged state where possible → identify uncertain names and manual recovery |
| `down` cannot connect before teardown because of timeout | Remember startup error → continue other teardown, name skipped databases, exit 1 |
| `down` times out or is interrupted while dropping a database | Continue remaining teardown → incomplete summary and exit 1, even after registry removal |
| First SIGINT/SIGTERM during `down` | Cancel current DB work and skip later drops → name residual DBs, continue non-DB teardown, exit 1 if it finishes |
| Further SIGINT/SIGTERM after the first-signal acknowledgement | Default OS termination → no promise of completed cleanup or a saved registry |
| Rapid signals before first-signal handling completes | POSIX delivery/coalescing may lose a repeat → no exact-count or lossless escalation guarantee |
| Connection error contains URL/query credentials | Safe diagnostic wrapper → preserve errors.Is/As without rendering credentials in stderr or report Detail |
| Parent cancellation during `status.Build` | Return a wrapped cancellation error → stop further DB stages; CLI exits 1 without rendering a success report |
| Database deadline during `status.Build` | Keep Unknown dimensions with failure details → return degraded report, exit 0 |
| Tolerant caller encounters timeout/cancellation | Explicit command-specific result above → no silent healthy verification or resumed success |

## Tests

### Unit and local-transport tests (no Postgres required)

| Test | What it verifies |
|---|---|
| `TestPostgres_ConnectDeadline` | A local TCP listener accepts but never responds; the real constructor/pgx returns under an injected short connection deadline |
| `TestPostgres_ConnectCancellation` | Cancel after the listener signals acceptance; real pgx returns a wrapped cancellation error |
| `TestPostgres_ConnectionErrorsDoNotLeakCredentials` | Initial/per-DB parse and connection failures preserve error identity, while safe diagnostics omit plaintext/encoded URL and query credentials |
| `TestPostgres_TimeoutPolicyValidation` | Private duration injection rejects non-positive limits; public defaults remain 10s/60s/10s |
| `TestPostgres_PhaseHonorsParent` | Earlier deadlines and canceled parents win; nested helpers cannot extend a phase |
| `TestPostgres_PhaseBudgetShared` | Several simulated constituent calls share one budget, not a full budget each |
| `TestPostgres_CleanupFreshBoundedContext` | Cleanup begins with a fresh deadline despite an expired parent; nested recovery does not restart it |
| `TestPostgres_SwapRetriesOnlyObjectInUse` | Executor fake returns 55006, permission, and transport failures; only 55006 retries/defers |
| `TestPostgres_SwapDeadlineNotDeferred` | Expiration during execution/backoff returns a deadline error, never ErrDeferredSwap |
| `TestPostgres_SwapCancellationNotDeferred` | Cancellation during backoff exits promptly and preserves staged state |
| `TestPostgres_BaseCommandCancellationStopsChildren` | Base command cancellation kills both shell and a child holding output pipes; no hung Wait |
| `TestPostgres_BaseCommandHasNoAutomaticDeadline` | Script runs beyond injected DB-phase duration, then finishes successfully with an uncanceled parent |
| `TestRunCommand_CancellationStopsProcess` | Retain/strengthen existing instance process-group regression test |
| `TestRunCommand_CanceledContextDoesNotStart` | An already-canceled parent cannot start a side-effecting script |
| `TestInstance_Up_CloneTimeoutRollsBackEarlierDatabases` | Deterministic later-clone failure cleans all earlier clones, network, ports, mailbox, and worktree |
| `TestInstance_Up_MigrationCancellationRollsBack` | Interrupted instance migration returns cancellation and performs rollback with fresh DB cleanup contexts |
| `TestInstance_Up_CleanupTimeoutContinuesRollback` | Blocking fake drop respects injected cleanup bound; later local cleanups still run and stderr names the residual DB |
| `TestInstance_Up_CleanupDurationValidation` | Private up rejects non-positive cleanup durations before side effects; exported Up retains the fixed default |
| `TestInstance_Down_DatabaseTimeoutContinuesTeardown` | Timeout is retained while network/worktree/mailbox/registry teardown continues; return non-nil |
| `TestInstance_Down_CancellationContinuesTeardown` | Cancel during one DB drop; no later DB drops start, all residual names are reported, non-DB cleanup continues, and cancellation is returned |
| `TestInstance_Down_DockerContextSurvivesCancellation` | Cancel before/during container stop and during DB drop; Docker contexts stay live, an active DB context cancels, and no drop starts after cancellation |
| `TestInstance_Down_TimeoutDoesNotCancelLaterDrops` | A fake phase deadline leaves the parent live; the next database is still attempted without a rollback-budget retry |
| `TestCLI_Down_ConnectTimeoutStillTearsDown` | Initialization timeout is not lost when BM remains nil; skipped DB names and non-zero result remain visible |
| `TestCLI_Down_SecondSignalForcesExit` | Child-only harness covers SIGINT/SIGTERM as the first and later signal, waits for post-restoration acknowledgement, then verifies default OS termination without Postgres or Docker |
| `TestCLI_DatabaseTimeoutCallerPolicies` | Table-driven base/up/resume/verify/status/doctor coverage for the documented timeout/degraded behavior, including JSON output validity |
| `TestCLI_DatabaseCancellationCallerPolicies` | No tolerant caller turns cancellation into successful forward work |
| `TestDoctor_DatabaseTimeoutIsFailure` | Deadline errors in existence/orphan/base checks are failures, not warnings or an all-resources-present claim |
| `TestDoctor_ConnectionErrorRetainsCause` | A nil backend with BMErr produces a failed check containing the connection failure |
| `TestStatus_ConnectionErrorMakesDatabaseDimensionsUnknown` | Schema/data details retain the backend cause without changing report JSON shape |
| `TestStatus_CancellationReturnsError` | Cancel the parent before Build or inside each fake DB stage; observe a wrapped context.Canceled error, nil report, and no subsequent DB calls |
| `TestStatus_DeadlineRemainsDegradedReport` | Database deadline errors yield Unknown database dimensions and a nil Build error, not fatal cancellation |
| `TestCLI_ConnectionDiagnosticsDoNotLeakCredentials` | stderr and status/doctor JSON details omit credential sentinels while retaining operation names and timeout/cancellation classification |

Use listener/script readiness channels or marker files before canceling, not
sleep-based assumptions about startup. Test subprocesses have independent
watchdog deadlines and teardown; close accepted sockets/listeners and reap
children even when an assertion fails. Timing assertions allow race-enabled
CI scheduling slack and assert error identity, not just elapsed time.

### Real-binary CLI transport regression (no Postgres or Docker)

`TestCLI_BaseStatusUnresponsivePostgres` lives in `cmd/plax/main_test.go`.
Build the real binary with the existing `buildPlax` helper, supply a generic
blueprint fixture and an explicit local accept-and-stall DSN, and assert exit
1 after the production 10s connection budget plus scheduling slack, naming
connection establishment. It must run with `PLAX_TEST_POSTGRES_URL` unset;
never call `e2ePrereqs`, take the Postgres advisory lock, or probe Docker.
Use a watchdog independent of the client deadline and clean up the listener
and child on failure. Clear the child's `PLAX_TEST_POSTGRES_URL` and point its
`DOCKER_HOST` at a nonexistent test socket to prove those backends are not needed;
do not stop or reconfigure the host's real services. No production timeout
override is added. This test does not prove deadlines on already-established
connections.

This is a transport-only CLI test, like other real-binary tests that run
without infrastructure, not an exception to the repository's infrastructure
E2E skip convention. It therefore runs in normal CI before task 3 adds Postgres.

### Postgres integration tests

Skip when `PLAX_TEST_POSTGRES_URL` is unset and retain existing serialization.
Do not change `LockPostgres` behavior in this plan; that belongs to #83.

- `TestPostgres_ProvenanceDeadline` — hold an exclusive lock on a fixture's
  `_plax_provenance` table using a separate transaction; an injected short
  database phase expires during a real query, preserving deadline identity.
  Release the lock and prove the next healthy read succeeds.
- `TestPostgres_DatabaseEntrypointsHaveDeadlines` — table-driven coverage of
  database-only entry points and the first phase of each mixed entry point;
  block pool acquisition/connection through pgx's configuration hooks and
  verify every method returns under its injected budget without starting scripts.
- `TestPostgres_BaseStatusRelocksAfterTimeout` — hold a provenance-table lock
  before locking the base; time out during temporary-unlock reading and verify
  `datallowconn` is restored with a fresh cleanup budget.
- `TestPostgres_BaseScriptCancellationRestoresState` — table-driven create,
  seed, and refresh scenarios; cancel a marker-ready migrate/seed script,
  verify child termination, and check partial-base/staging cleanup or relocking.
  A bounded cleanup failure must be explicitly returned.
- `TestPostgres_CloneTimeoutCleansOwnedTarget` — force a post-create failure
  under a short parent deadline, then verify the owned target is removed;
  independently verify a pre-existing target is never dropped.
- `TestPostgres_RefreshInterruptedSwapPreservesStaging` — simulate the state
  after old-base drop but before successful rename; refresh refuses to silently
  create a competing base and reports the staged recovery state.
- `TestPostgres_CloneFixtureTiming` — measure template-copy SQL and the full
  `CloneBase` operation on the deterministic seeded fixture below, using the
  production 60s operation budget rather than an injected short timeout.

Retain the existing create/seed/reset/clone/refresh/provenance/applied-migration
healthy-path suites, including staged-refresh resumption.

#### Clone timing fixture and evidence

This is one integration scenario, not a benchmark framework or a claim about
production data. The existing one-row `users` fixture is too small to provide
useful storage-copy evidence. Extend that generic schema in a test-owned base
with 102,400 rows of deterministic 1,024-byte text payloads (100 MiB before
table/index/database overhead), using generated values rather than product data.

```text
1. Require explicit PLAX_TEST_POSTGRES_URL, take the existing advisory lock,
   and create uniquely named test-owned base/targets with cleanup registered.
2. Populate the users fixture, close its connections, and lock the base.
3. Record row count, pg_database_size(base), server version, and test-run context.
4. Under a 60s context, time one CREATE DATABASE target TEMPLATE base call.
   Register target recovery before executing SQL because its outcome may be uncertain.
5. On a distinct target, time CloneBase using its unmodified production policy.
6. Log both elapsed values and actual fixture size with t.Logf; preserve failed
   attempts/timeouts and cleanup failures in the test output, not as skips.
7. Drop only this test's targets/base with fresh bounded cleanup contexts.
   ⚠ No exact speed threshold beyond successful completion under the configured
   deadline: storage and server load differ across machines. Never increase
   the limit silently to make a timing failure pass.
```

Include the fixture size, SQL-copy elapsed time, full clone elapsed time,
server version, and relevant machine/storage context in the implementation
PR's verification evidence. Run the named test explicitly with
`-race -count=1 -v` against the configured test server. A pass is evidence for
that synthetic size/run only; larger or differently stored bases remain a documented risk.
This plan-only PR adds the measurement requirement, not invented results;
its implementation acceptance item stays unchecked until measured.

### End-to-end tests and fixtures

- `TestEndToEnd_UpSIGINTRollsBack` — real Git, Docker, and Postgres fixture; wait
  for an instance migration's readiness marker, signal only the plax PID,
  verify its process group is stopped, and assert clones and provisioning
  resources are removed. No production timeout override is added.
- `TestEndToEnd_DownSIGINTContinuesTeardown` — create a real instance, use an
  accept-and-stall Postgres endpoint for down, signal after connection
  acceptance, and verify other resources/registry are removed, residual DBs
  are named, and the command exits non-zero. Test cleanup uses the real DSN.

Keep these real-infrastructure scenarios in `cmd/plax/e2e_test.go`, matching
the existing `TestEndToEnd_*` names and `PLAX_TEST_POSTGRES_URL` skip convention.
Extend generic sample fixtures and generate listener/marker fixtures in test
temp directories; no real product names, checked-in credentials, or global
server shutdowns are needed.

## Acceptance criteria

- [ ] Run `plax base status` against an accept-and-stall TCP listener; observe
  exit 1 after approximately the 10s connection budget and a connection error.
- [ ] Run `TestCLI_BaseStatusUnresponsivePostgres` with
  `PLAX_TEST_POSTGRES_URL` unset and no Docker daemon; verify it executes rather
  than skipping and passes in the normal CI job.
- [ ] Block a provenance query on a real Postgres fixture; observe expiration
  of one shared injected operation budget, not one budget per SQL call.
- [ ] Exercise every database entry point with deadline/cancellation coverage;
  preserve `errors.Is` identity through operation and cleanup wrapping.
- [ ] Cancel `base create`/`seed`/`refresh` and instance migration scripts after
  readiness; observe child termination, prompt Wait completion, and bounded recovery.
- [ ] Run scripts longer than the injected database-phase budget; observe
  successful completion without an automatic script deadline.
- [ ] Fail a later clone in multi-database `up`; verify all previously created
  resources receive cleanup and every cleanup failure is named.
- [ ] Time out lock-sensitive base work; verify relocking is attempted with a
  fresh 10s budget and failed relocking cannot be silently reported as success.
- [ ] Return swap timeout/network/permission errors as exit 1; return exit 2
  only after database-in-use retry exhaustion, preserving staging where possible.
- [ ] Interrupt or time out database work during `down`; verify other teardown
  continues, registry/ports are removed, residual DBs are named, and exit is 1.
- [ ] After a first signal during `down`, verify no additional DB drops start;
  verify Docker calls receive live teardown contexts even when cancellation
  precedes the DB loop. After acknowledgement, send another signal during held
  cleanup and observe default OS termination for SIGINT/SIGTERM combinations.
- [ ] Cancel `status` during each DB-facing stage; verify Build returns a wrapped
  cancellation, later DB stages do not run, and CLI exits 1; retain exit 0 with
  Unknown dimensions for a database-phase deadline without parent cancellation.
- [ ] Exercise tolerant CLI callers; verify the documented degraded-report
  behavior without silently recording a healthy verification on timeout.
- [ ] Confirm no new timeout flags, environment variables, blueprint fields,
  JSON report shapes, or whole-command deadlines were introduced.
- [ ] Run `TestPostgres_CloneFixtureTiming` on the configured test server;
  report actual fixture size, template-copy SQL elapsed time, full clone elapsed
  time, and run context. Bring any failure/timeout back to the maintainer before
  completing the work; do not claim this bounds every larger base.
- [ ] Exercise connection failures containing plaintext and encoded credential
  sentinels; verify stderr and status/doctor JSON contain none of them while
  errors.Is/As still identify the original timeout, cancellation, or pgx cause.
- [ ] Update manual and bundled guide with limits, script interruption,
  incomplete teardown, and staged-swap recovery; retain maintainer-owned README prose.
- [ ] Run full local checks and Postgres/E2E suites; report actual output,
  failures, skips, and infrastructure availability before declaring implementation complete.

Verification at implementation time:

```sh
make check
gofmt -s -l .
go mod tidy -diff
go test -race -count=1 ./...
```

Until #83 is fixed, run the all-package checks with
`PLAX_TEST_POSTGRES_URL` unset, then run the two infrastructure-bearing
packages sequentially with it set and explicit budgets:

```sh
go test -race -count=1 -timeout=30m -v ./pkg/derive/postgres
go test -race -count=1 -timeout=30m -v ./cmd/plax
```

⚠ Unsetting the variable skips infrastructure E2E tests, but does not make
the existing Postgres package unit-only: `pgTestURL` currently falls back to
localhost, and its helpers skip only if connection fails. Record which tests actually
ran, do not call the unset run infrastructure-free, and inspect the local
server target before running tests that create/drop fixture databases.
New integration tests in this plan follow the explicit-environment skip
convention. Aligning the old helper belongs in the test-infrastructure
discussion for #83, not an unreviewed change here.

This is a temporary verification procedure, not an implementation of #83.
An absent server/Docker/tool is a reported blocker or skip, not evidence that
the integration acceptance criteria passed. Keep acceptance boxes unchecked
until the corresponding evidence exists; plan approval is not verification.

## Dependencies

No new external Go modules are required. Retain the existing versions:

| Module | Version | Use |
|---|---|---|
| `github.com/jackc/pgx/v5` | v5.10.0 | pgx pools, connection deadlines, PostgreSQL error codes, real-query tests |
| `github.com/alecthomas/kong` | v1.16.0 | Existing CLI parsing and unchanged command syntax |
| `github.com/google/go-cmp` | v0.7.0 | Existing fixture/report comparisons where appropriate |
| `github.com/docker/docker` | v27.1.1+incompatible | Existing lifecycle and E2E Docker operations |

Standard-library additions/use: `context`, `errors`, `fmt`, `net`, `os`,
`os/exec`, `os/signal`, `syscall`, `sync`, and `time`, plus existing test
helpers. `pgconn` and `pgxpool` are subpackages of the pinned pgx module.
Go remains at the repository's `go 1.26.5` directive.
