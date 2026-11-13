# Plan 24 — Deterministic Postgres test execution

## Objective

Make Postgres-enabled test runs deterministic and fail visibly on configured infrastructure failures, resolving [#83](https://github.com/apollopower/plax/issues/83) before adding infrastructure CI.

**Status: draft for maintainer review; no implementation authorized yet.**

Proposed policy, requiring approval:

| Boundary | Proposal |
|---|---|
| `PLAX_TEST_POSTGRES_URL` empty/unset | Skip Postgres-dependent tests before connecting; no localhost fallback |
| Variable explicitly configured | Backend/setup failures fail tests, not skip them |
| Configured CLI E2E prerequisites | Missing git/python3/psql or unavailable Docker fails, rather than silently reducing coverage |
| Canonical configured test run | `make test` runs all packages with `-p=1`, `-race`, `-count=1`, and `-timeout=30m` |
| Canonical unconfigured test run | Keep package parallelism; `-race -count=1 -timeout=10m` |
| Canonical test-mode visibility | One fixed stderr notice before invoking Go; never print the configured URL |
| Postgres-only development | Supported direct package invocation without Docker/CLI E2E; report it as partial coverage |
| Test-lock connection establishment | 10s |
| Test advisory-lock acquisition | 2m, separate from connection establishment |
| Test-lock release/connection close | One fresh, cancellation-independent 10s budget |
| Raw fixture SQL / existing `runPlax` child waits | No new per-call deadline; the package watchdog remains their only test-harness backstop |

The test-lock limits are test infrastructure settings, not additions to plan 23's
product policy. No new flag or environment variable is proposed. Serialization
trades some package parallelism for independent package runtime budgets; it is
not intended as a speed optimization.

### Investigation and scope

- Historical CI [run 33416610225](https://github.com/apollopower/plax/actions/runs/33416610225)
  from closed [PR #81](https://github.com/apollopower/plax/pull/81) confirms both
  packages reached their independent 10m limits. Its captured stacks show a
  provenance DB operation and an E2E child-command wait, not an advisory-lock
  acquisition. Do not claim those snapshots independently prove lock starvation.
- After merged plan 23 (`b596d47`), an unmodified parallel baseline on an isolated
  PostgreSQL 16 server passed: Postgres **54.648s**, CLI **73.203s**, wall time
  **73.807s**, 149 passing test/subtest events, two child-only helper skips,
  and all eight infrastructure E2E scenarios executed. Invocation used
  `-race -count=1 -p=2 -timeout=10m` and `GOMAXPROCS=4` per process. This was
  not a four-core CI reproduction, and historical starvation was not reproduced.
- The corresponding unmodified serial baseline (`-p=1`, same selected packages
  and server, `GOMAXPROCS=4`) also passed: Postgres **31.491s**, CLI **71.110s**,
  wall time **103.439s**, the same 149 passing test/subtest events, two helper
  skips, and eight E2E scenarios. These are single local observations, not a
  benchmark or evidence that serialization makes total execution faster.
- Current `LockPostgres` uses unbounded contexts and skips connection failures.
  `t.Skipf` aborts that test; it does **not** continue its mutations lock-free.
  The remaining silent failure is loss of intended test coverage.
- A parent-held session lock can serialize its spawned CLI mutations against
  other cooperating tests without the CLI acquiring the lock. Keep that model;
  it depends on the parent session remaining connected and covering child cleanup.
- Legacy Postgres helpers still fall back to localhost and skip after a failed
  ping/constructor. The provenance fixtures do not take the shared lock.

This is a test-harness change, not a product change. Do not add a Postgres CI job,
change production timeouts or teardown exit codes, remove anonymous volumes,
change application readiness/schema reporting, or release anything. Infrastructure
CI follows as a separately reviewed task. Do not modify README narrative or
include private application smoke evidence in committed content.

**Non-lock hangs are explicitly not fixed here.** Existing raw fixture SQL using
`context.Background()` and the `runPlax` helper's `exec.Command` waits do not gain
per-call bounds. A single stuck operation can still consume the entire configured
30m package watchdog (10m in the unconfigured canonical mode), which is longer
than the measured baseline. Plan 23's product DB deadlines do not bound every
fixture operation or an intentionally deadline-free migration script. A package
watchdog abort is not graceful cleanup: it can bypass test cleanup and leave a
spawned CLI/workload behind. Inspect and recover only test-owned resources after
such an abort. Per-fixture SQL deadlines and managed child watchdogs would need
separate scope/approval; do not claim this plan prevents all historical hangs.

**CI adoption remains separate.** The current CI workflow directly invokes
`go test -race -count=1 ./...`, with no Postgres service or configured test URL.
It bypasses the Makefile routing and its mode notice. Future fake-go regression
tests can cover routing decisions in that job, but cannot establish that real CI
has exercised the configured serialized suite. Merging this plan-only PR does
not fix or close #83. Implementing it changes the canonical local path; the next
infrastructure-CI task must separately review provisioning and actual invocation
(prefer `make test` for one canonical path). Do not claim infrastructure CI
coverage until that job is implemented and has run successfully.

## Package layout

```text
Makefile                                # canonical scheduling and visible stderr mode notice
AGENTS.md                               # explicit test opt-in and canonical invocation
pkg/testutil/
  testutil.go                           # shared URL gate; bounded, fail-closed lock lifecycle
  testutil_test.go                      # transport/child-harness/real-lock/Makefile tests
pkg/derive/postgres/
  basemanager_test.go                    # consistent gate and fatal configured setup failures
  provenance_test.go                    # remove fallback/skips; lock mutating fixture once
cmd/plax/e2e_test.go                     # consistent gate; fail missing configured prerequisites
docs/
  testing.md                            # full/partial runs, safe targets, limitations, CI adoption gap
  plans/index.md                        # phase 24 inventory entry
  plans/23-db-timeouts.md                # mark its temporary verification workaround superseded
```

No runtime source, bundled guide, or user manual change is expected. Their CLI
contract is unchanged. Docker-only package tests' independent availability policy
is outside this plan; explicitly opting into CLI E2E must nevertheless require
that suite's Docker dependency.

## Type specifications

Keep `LockPostgres(t *testing.T, url string)` and the existing advisory key
`7078410`. Add one shared test-only entry point:

```go
func PostgresURL(t *testing.T) string

type postgresLimits struct {
    connect time.Duration
    lock    time.Duration
    cleanup time.Duration
}

func acquirePostgresLock(ctx context.Context, url string, limits postgresLimits) (*pgx.Conn, error)
func releasePostgresLock(ctx context.Context, conn *pgx.Conn, cleanupTimeout time.Duration) error
```

`PostgresURL` owns the unset-variable skip and otherwise returns the configured
value without logging it. Helpers called with an explicit URL fail on setup
errors; they have no fallback or additional availability skips. Define defaults
in a private function, not mutable globals; private duration injection rejects
non-positive limits and lets helper tests use milliseconds.

Private lock/setup errors must retain `Unwrap`/`errors.Is` identity while rendering
only an operation name and allowlisted failure classification (including
deadline/cancellation or SQLSTATE when available). Never interpolate a raw DSN,
pgx parse/connect error, or connection config into diagnostics. No serialized
types, schema fields, or new external modules are needed.

## Algorithms

### Canonical scheduling and opt-in

```text
1. make test checks whether PLAX_TEST_POSTGRES_URL is nonempty without printing it,
   and emits one fixed test-mode notice to stderr before invoking Go.
2. Unset → go test -race -count=1 -timeout=10m ./...
3. Configured → go test -race -count=1 -timeout=30m -p=1 ./...
4. Propagate go test's failure status. make check continues to call this target.
5. Real-Postgres setup/mutation paths call PostgresURL before connecting or
   mutating a database; generic temp-repo fixtures remain usable without Postgres.
6. Configured constructor/ping/setup errors fail, never skip.
7. Configured E2E missing-tool/Docker prerequisites fail before fixture creation.
   ⚠ -p=1 serializes package binaries, not goroutines within a package.
   ⚠ A raw go test invocation can bypass Makefile policy. Document -p=1 for
     direct configured all-package runs; do not claim universal scheduling control.
```

Use explicit command-line flags so an inherited `GOFLAGS=-p=...` does not silently
undo configured serialization. Do not hide failures by omitting either package,
discarding status, excluding E2E, or increasing deadlines during acceptance.
Choosing an explicitly documented Postgres-only development run is supported,
but it is not full-suite acceptance and must not be reported as such.

Emit the notice from the Makefile recipe, outside Go's test-output capture, so
successful non-verbose runs still show the coverage mode. Use these exact fixed
messages once per `make test` invocation, including when the Go command fails:

```text
tests: Postgres disabled (PLAX_TEST_POSTGRES_URL unset/empty); skipping Postgres integration and CLI E2E.
tests: Postgres + CLI E2E enabled; serial package execution (-p=1).
```

Do not print either notice from every helper/test. A direct `go test` invocation
does not get this Makefile notice; document `-v` to inspect its skip messages.
No URL, credentials, or environment dump may appear in the mode notice.
Keep the mode-selection recipe quiet (`@`) so Make's command echo does not
duplicate the notice literals on stdout.

### Lock lifecycle

```text
1. Connect to the explicit test server with a 10s context and pgx ConnectTimeout.
2. Acquire the existing session-scoped advisory lock with a separate 2m context.
3. On acquisition failure, close the owned connection with fresh bounded cleanup;
   retain both errors. Fail before any protected fixture work.
4. Register test cleanup immediately after successful acquisition.
5. Keep the connection alive for the entire test, including spawned CLI calls.
6. Cleanup uses one fresh 10s budget for unlock + close; close is always attempted,
   including when unlock fails or reports that the expected lock is not held.
7. Report cleanup errors as test failures without preventing other cleanups.
   ⚠ SQL cancellation may race with acquisition; closing the owned connection
     is necessary even when acquisition's outcome is uncertain.
   ⚠ The same test must not acquire the session lock twice on separate connections.
   ⚠ Register resource cleanups after lock acquisition so LIFO cleanup releases
     the lock last. No detached fixture-mutating child may outlive that scope.
```

Honor earlier caller cancellation/deadlines; this is not a cross-process lease
monitor or protection from a deliberately stopped shared server. Do not silently
reconnect/reacquire after a lock-session failure and pretend work stayed protected.
The public wrapper derives setup contexts from `t.Context()` and any earlier
`t.Deadline()`; cleanup detaches cancellation and receives its own budget. This
does not promise cleanup after Go's whole-package watchdog terminates the process.
The 10s lock-cleanup budget starts when that cleanup callback begins, not when
earlier fixture/resource cleanups begin. An unbounded earlier cleanup can still
prevent it from running until the package watchdog aborts; do not promise a
10s bound on the entire test teardown.

### Legacy fixture alignment

Remove the implicit localhost URL and availability skips from `pgTestURL`,
`testPool`, and `testManagerWith`; use the shared gate and bounded connection/ping
setup. Have the provenance fixture acquire the advisory lock once at its outer
mutating setup, not once per pool creation. Its repeated pool opens must not
deadlock through repeated lock acquisition.

Use parsed pgx configuration to select fixture databases instead of last-slash
DSN surgery, preserving URL/query credentials and connection options. Scope all
cleanup to test-owned resources; this does not authorize running against a shared
application server. No generic fixture framework or per-test server provisioner.

## CLI specification

No Plax CLI changes. Developer entry points are:

```sh
# No Postgres connection or localhost fallback; Postgres tests skip.
env -u PLAX_TEST_POSTGRES_URL make test

# Explicit, isolated test server; required dependencies must be available.
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' make test
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' make check

# Equivalent direct configured all-package invocation:
go test -race -count=1 -timeout=30m -p=1 ./...

# Supported partial run: real Postgres tests, no Docker or CLI E2E required.
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' \
  go test -race -count=1 -timeout=30m ./pkg/derive/postgres
```

The direct all-package command requires the variable exported in the environment.
The Postgres-only command sets it only for that invocation; `psql` is still
required by the existing migration/seed fixtures. It deliberately omits the CLI
package, so missing Docker is not an error for that partial run. Optional helper
coverage can include `./pkg/testutil` alongside `./pkg/derive/postgres` with
explicit `-p=1`. No new opt-in variable or Makefile target is needed.

Configured `make test`/`make check` still promise the full suite, including CLI
E2E, and must fail on its missing prerequisites. Do not turn those failures into
skips to support Postgres-only development. Document the partial command in
`docs/testing.md` and AGENTS.md, and distinguish its result from a full check.

An unset run is Postgres-free, not necessarily infrastructure-free: existing
Docker-only tests retain their own policy. The Makefile notice makes the
Postgres/CLI E2E coverage omission explicit. Direct Go users can add `-v` to see
skips; the current CI invocation remains a direct, unconfigured run until the
separate CI task. Do not put credentials in shared reports or examples.

## Error handling

| Failure mode | Expected behavior |
|---|---|
| Test URL unset | Shared gate → skip only Postgres-dependent tests before connection/side effects |
| Configured URL invalid/refused/stalled | Named, credential-safe setup failure → test/run non-zero, never skip |
| Server disappears between lock acquisition and manager/pool setup | Fatal configured setup error → no apparent coverage success |
| Lock held by another session past budget | Acquisition deadline → close owned waiter, fail before fixture work |
| Caller already canceled/earlier deadline | Honor parent → no restarted allowance or fixture work |
| Unlock fails/lock no longer held | Retain failure, still close connection → test non-zero |
| Test body fails after lock acquisition | LIFO cleanup still releases/closes → later acquisition succeeds |
| Required E2E tool/Docker unavailable with URL configured | Explicit prerequisite failure → no silent E2E skips |
| Developer has Postgres but not Docker | Explicit Postgres-only package run → supported partial coverage, not full-suite acceptance |
| Raw fixture SQL, child wait, or earlier resource cleanup hangs | Package watchdog only → possible abort without completed cleanup; no non-lock-hang fix claimed |
| Canonical run omits Postgres coverage | Fixed Makefile stderr notice before Go → visible reduced mode without revealing a URL |
| Make-invoked test command fails | Preserve non-zero status → make test/check fail |
| URL or pgx cause contains credentials | Allowlisted diagnostic → retain inspectable error identity without disclosure |

## Tests

Unit/local-transport and child-process tests (no real Postgres required):

| Test | Evidence |
|---|---|
| `TestTestutil_PostgresURLUnsetSkips` | Child harness observes skip before a body/connection marker; no localhost probe |
| `TestTestutil_ConfiguredPostgresFailureIsFatal` | Malformed/refused URL → child non-zero, not skip, protected-body marker absent |
| `TestTestutil_PostgresConnectDeadline` | Accept-and-stall listener → private short connection budget, deadline identity retained |
| `TestTestutil_PostgresCallerCancellation` | Canceled/earlier-deadline parent wins without starting protected work |
| `TestTestutil_PostgresLimitValidation` | Reject non-positive private limits; fixed defaults remain 10s/2m/10s |
| `TestTestutil_PostgresDiagnosticsHideCredentials` | Plain/encoded URL and query sentinels absent from errors/child output |
| `TestTestutil_MakeTestRoutesByConfiguration` | Fake go records configured -p=1/30m versus unset parallelism/10m; both race/uncached; exactly one correct fixed stderr notice, absent from stdout and free of URL/credential sentinels |
| `TestTestutil_MakeTestPropagatesFailure` | Fake go fails → Makefile target non-zero; mode notice still appears exactly once |
| `TestCLI_E2EConfiguredPrerequisiteFailure` | Child harness with missing required tool/unreachable Docker fails instead of skipping |

Use child-only helper entry points to test `t.Fatal`/`t.Skip` without turning the
parent regression test into an intentional failure: `TestTestutil_PostgresHelperChild`
and `TestCLI_E2EPrereqChild` skip in their parent process. Fake prerequisite tools
allow the missing-Docker regression to run even without locally installed psql.
Use readiness markers/channels and independent watchdogs; do not signal the test
runner or rely on startup sleeps.
Temporary fake tools may record arguments but must not connect or echo the DSN.

Real Postgres tests require an explicit isolated URL; otherwise they skip:

- `TestTestutil_PostgresLockContentionDeadline` — controlled holder, short waiter
  budget, no protected body, preserved deadline, and no leftover waiter session.
- `TestTestutil_PostgresLockReleasedOnCleanup` — normal and intentionally failing
  child bodies release the lock; a following acquisition succeeds.
- `TestTestutil_PostgresLockCleanupFailureIsReported` — terminate only a test-owned
  lock session; cleanup reports failure and does not hang or silently pass.
- `TestPostgres_TestDatabaseURLPreservesOptions` — fixture DB selection retains
  query options/encoded credentials without rendering them in test output.

Lock-lifecycle tests must not acquire an outer `LockPostgres` and then recursively
wait on the same key. Register cleanup immediately for every test-owned session.
Retain all existing Postgres, CLI transport, and eight infrastructure E2E scenarios.

## Acceptance criteria

- [ ] With the variable unset, all Postgres-dependent tests skip before any
  connection, including the old provenance/base-manager helpers; no localhost fallback.
- [ ] With a malformed, unreachable, or stalled explicitly configured endpoint,
  helper and applicable package invocations fail rather than silently skipping.
- [ ] A contended lock expires under an injected short wait budget, preserves
  its cause, closes the waiter, and never reaches protected fixture work.
- [ ] Normal/failing test bodies release the advisory lock; lost-session/cleanup
  failures are visible, and cleanup has a fresh bounded allowance.
- [ ] `make test`/`make check` use explicit serialized package scheduling when
  configured; a fake-go regression verifies flags and failure propagation.
- [ ] Configured E2E missing-prerequisite failures are non-zero, not skips.
- [ ] Run the documented Postgres-only command against an isolated server with
  `DOCKER_HOST` pointing at a nonexistent test socket and psql available; verify
  real Postgres tests execute without CLI E2E, and report the result as partial.
- [ ] Verify both Makefile modes emit exactly one fixed stderr notice before Go,
  including failure paths; no configured URL or credential sentinel is printed.
- [ ] Run the configured full suite uncached/race-enabled on an isolated server;
  both infrastructure packages and all eight E2E scenarios execute. Report exact
  durations, failures, skips, and environment limitations; do not claim a faster run.
- [ ] Run the unset-variable suite and full formatting/tidy/vet/build/lint checks;
  confirm ordinary unit/local-transport coverage still executes.
- [ ] Update AGENTS/testing documentation and the plan inventory; supersede plan
  23's temporary procedure without rewriting its historical verification evidence.
- [ ] Confirm runtime source, CLI, production deadlines, CI workflow/service
  configuration, README prose, and release state are unchanged.
- [ ] Document that raw fixture SQL/child hangs retain only the package watchdog,
  teardown can remain incomplete on abort, and the existing direct CI path does
  not validate configured serialization or actual infrastructure coverage.

The parallel baseline above is investigation evidence, not verification of this
unimplemented plan. During this plan-only work, use the explicit isolated server
and `GOFLAGS=-p=1` for all-package checks to avoid the old localhost fallback.

## Dependencies

No new modules. Retain Go 1.26.5 and `github.com/jackc/pgx/v5` v5.10.0.
Use standard-library context/errors/time/testing/os/exec/net helpers, existing
GNU make, per-test temporary directories, and the existing Docker/Git/Postgres
fixtures. No mock framework, test daemon, global scheduler, or new runtime package.
