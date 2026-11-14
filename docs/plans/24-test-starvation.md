# Plan 24 — Deterministic Postgres test execution

## Objective

Make canonical Postgres-enabled test scheduling deterministic and configured setup failures visible as scoped hardening for [#83](https://github.com/apollopower/plax/issues/83), without claiming to resolve the remaining non-lock hangs or CI execution gap.

**Status: implemented and locally verified, including the approved fixture-regression amendment. The issue-state acceptance discrepancy below remains; configured infrastructure CI is separate.**

Agreed policy:

| Boundary | Policy |
|---|---|
| `PLAX_TEST_POSTGRES_URL` empty/unset | Skip Postgres-dependent tests before connecting; no localhost fallback |
| Variable explicitly configured | Backend/setup failures fail tests, not skip them |
| Configured CLI E2E prerequisites | Missing git/python3/psql or unavailable Docker fails, rather than silently reducing coverage |
| Canonical configured test run | `make test` runs all packages with `-p=1`, `-race`, `-count=1`, and `-timeout=10m` |
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

Keep the existing Go default-sized **10m per-package watchdog in both modes**.
Serial package execution removes competing package timers; it does not justify
tripling their individual limits. Ten minutes is more than eight times the
observed worst local package runtime (~73s), but that measurement is not a CI
runtime guarantee. If later CI measurements need a different budget, bring the
evidence back for review rather than silently increasing it. This is not a total
run deadline: N stuck serialized packages can still consume roughly N × 10m,
plus build/other work. No whole-run watchdog is introduced in this plan.

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
- At planning time, `LockPostgres` used unbounded contexts and skipped connection failures.
  `t.Skipf` aborts that test; it does **not** continue its mutations lock-free.
  The remaining silent failure is loss of intended test coverage.
- A parent-held session lock can serialize its spawned CLI mutations against
  other cooperating tests without the CLI acquiring the lock. Keep that model;
  it depends on the parent session remaining connected and covering child cleanup.
- At planning time, legacy Postgres helpers fell back to localhost and skipped after a failed
  ping/constructor. The provenance fixtures did not take the shared lock.

This is a test-harness change, not a product change. Do not add a Postgres CI job,
change production timeouts or teardown exit codes, remove anonymous volumes,
change application readiness/schema reporting, or release anything. Infrastructure
CI follows as a separately reviewed task. Do not modify README narrative or
include private application smoke evidence in committed content.

**Non-lock hangs are explicitly not fixed here.** Existing raw fixture SQL using
`context.Background()` and the `runPlax` helper's `exec.Command` waits do not gain
per-call bounds. A single stuck operation can still consume the entire
10m package watchdog in either canonical mode, which is longer
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
not fix or close #83. Implementing it changes the canonical local path, not the
unverified historical CI outcome. Keep #83 open pending CI adoption evidence and
an explicit maintainer decision on the remaining hang gaps; do not infer full
resolution from these local scheduling/helper changes alone. The next
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
  basemanager_test.go                    # gate, fatal setup, isolated cleanup policy, script-phase regression
  provenance_test.go                    # remove fallback/skips; lock mutating fixture once
  watchdog_test.go                      # controlled raw-fixture SQL package-watchdog boundary
cmd/plax/e2e_test.go                     # consistent gate; fail missing configured prerequisites
cmd/plax/watchdog_linux_test.go          # contained runPlax watchdog/descendant cleanup regression
cmd/plax/watchdog_other_test.go          # explicit unsupported-platform skip; no unsafe child tree
docs/
  testing.md                            # full/partial runs, safe targets, limitations, CI adoption gap
  plans/index.md                        # phase 24 inventory entry
  plans/23-db-timeouts.md                # mark its temporary verification workaround superseded
```

No runtime source, bundled guide, or user manual change is expected. Their CLI
contract is unchanged. Docker-only package tests' independent availability policy
is outside this plan; explicitly opting into CLI E2E must nevertheless require
that suite's Docker dependency.
The CLI watchdog files use `//go:build linux` and `//go:build !linux`, respectively;
the latter contains the explicit unsupported-platform skip, not a child harness.

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
3. Configured → go test -race -count=1 -timeout=10m -p=1 ./...
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

### Approved fixture-regression amendment

After the acceptance failures recorded below, the maintainer authorized these
two additional test-only fixes. No production code or timeout policy changes:

1. `cleanupOwnedDB` captures the known-good fixture manager (including its pool
   and timeout policy) when cleanup is registered, before later failure injection.
   The intentional 50ms recovery failure must still be asserted by the test body;
   ordinary owned-database teardown uses its original fixed cleanup allowance,
   not the injected failure policy. Do not mutate the manager under test to mask
   its result. Add `TestPostgres_FixtureCleanupIsolatesFailureInjection` to verify
   teardown succeeds despite an injected nanosecond policy and leaves no fixture.
2. Refactor `TestPostgres_MixedOperationsDoNotTimeOutScripts` to observe phase
   separation instead of forcing all real DDL/checkpoint work under an artificial
   1s limit. Keep normal fixed database policy and the existing 15s parent guard.
   Record the preceding DB context through the test pool's `PrepareConn` hook;
   gate each script on readiness/release files. Require that DB context to be
   canceled while its caller and script remain live, then release the script and
   require the whole create/seed/refresh operation to succeed. Check both migrate
   and seed boundaries during refresh. Any database failure is still fatal and
   must not be mislabeled as proof of script-deadline inheritance.
3. Retain `TestPostgres_BaseCommandHasNoAutomaticDeadline` unchanged: that local
   command-only regression proves a script outlasts an injected short DB budget
   without involving storage-sensitive DDL. Together the tests cover command
   duration and mixed-operation context separation, not storage speed.
   ⚠ Use channels for observed contexts and join the operation goroutine before
   fixture teardown on every exit path. Do not add runtime seams or raise the
   production, canonical, parent-guard, or intentional-failure budgets.

## CLI specification

No Plax CLI changes. Developer entry points are:

```sh
# No Postgres connection or localhost fallback; Postgres tests skip.
env -u PLAX_TEST_POSTGRES_URL make test

# Explicit, isolated test server; required dependencies must be available.
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' make test
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' make check

# Equivalent direct configured all-package invocation:
go test -race -count=1 -timeout=10m -p=1 ./...

# Supported partial run: real Postgres tests, no Docker or CLI E2E required.
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' \
  go test -race -count=1 -timeout=10m ./pkg/derive/postgres
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
| `TestTestutil_MakeTestRoutesByConfiguration` | Fake go records configured -p=1 versus unset parallelism; both explicitly use -timeout=10m, race/uncached tests, and exactly one correct fixed stderr notice, absent from stdout and free of URL/credential sentinels |
| `TestTestutil_MakeTestPropagatesFailure` | Fake go fails → Makefile target non-zero; mode notice still appears exactly once |
| `TestCLI_E2EConfiguredPrerequisiteFailure` | Child harness with missing required tool/unreachable Docker fails instead of skipping |
| `TestCLI_RunPlaxPackageWatchdogBoundary` | Linux-only contained child test blocks in the real runPlax helper; short Go package watchdog aborts without its cleanup marker; supervisor reclaims the surviving test-owned worker |

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
- `TestPostgres_FixturePackageWatchdogBoundary` — a child test blocks in a raw
  fixture query with `context.Background()`; the short Go package watchdog, not
  a query deadline, aborts it. Parent observes incomplete child cleanup and
  reclaims only the owned fixture/session.
- `TestPostgres_FixtureCleanupIsolatesFailureInjection` — ordinary fixture cleanup
  retains its registered pool/policy despite later injected limits; no DB remains.
- `TestPostgres_MixedOperationsDoNotTimeOutScripts` — create/seed/refresh scripts
  wait behind readiness/release gates; preceding DB contexts are canceled while
  the caller remains live, and subsequent phases complete under normal policy.

Lock-lifecycle tests must not acquire an outer `LockPostgres` and then recursively
wait on the same key. Register cleanup immediately for every test-owned session.
Retain all existing Postgres, CLI transport, and eight infrastructure E2E scenarios.

### Controlled package-watchdog boundary tests

These tests demonstrate the two remaining hang modes; they do not add deadlines
to existing fixture SQL or `runPlax`. Do not wait ten minutes to test the Go
watchdog. Re-execute only dedicated child-test entry points from the already
built test binary, using **`-test.timeout=3s`** and an independent **15s parent
execution watchdog**. Ordinary suites retain their explicit 10m timeout. If the
parent watchdog fires or readiness is never confirmed, fail the regression;
do not count an arbitrary crash/kill as the expected package timeout.

```text
1. Parent creates only temporary, uniquely identified fixture resources and
   registers its own bounded cleanup before starting the child test.
2. Child registers a cleanup-marker callback, then enters one deliberate wait:
   - SQL: query a table locked by the parent in a uniquely owned fixture DB.
     Child uses raw pgx I/O with context.Background(), not a plan-23 DB phase.
   - CLI: call the existing runPlax helper with a temporary fake CLI that writes
     its PID/readiness marker and remains running. No real application or Docker.
3. Confirm the intended blocked state before the child watchdog fires:
   SQL backend identity + lock-wait state, or the fake CLI's readiness marker.
4. Wait/reap the child test. Require non-zero status and Go's package-timeout
   diagnostic; verify the child's registered cleanup marker is absent.
5. Parent independently cleans up and records what the timed-out test did not:
   terminate only the identified SQL fixture backend if it remains, release
   the owned lock/transaction, and drop only its fixture DB; or stop/reap the
   exact fake-CLI worker. Use fresh bounded parent cleanup contexts.
6. Assert no owned DB session, worker, or fixture remains. Preserve readiness,
   timeout, missing-cleanup-marker, and parent-recovery evidence in test output.
   ⚠ Never signal the ordinary test runner or a shared server. Do not search by
   broad process/database prefixes or rely on an arbitrary startup sleep.
```

For the CLI case, process death alone does not prove an orphan was reaped. Keep
Linux child-subreaper/process-group handling in a throwaway supervisor helper
process, not the ordinary test runner: that avoids adopting or reaping unrelated
tests' children. Record the owned PID/group before waiting, and have that
supervisor stop and reap its adopted worker even on assertion failures. An outer
independent watchdog guards the supervisor and targets only its recorded tree.
On other platforms, explicitly skip this case with the unsupported-containment
reason instead of starting an uncontained orphan. The SQL boundary test still
requires the explicit isolated Postgres URL and otherwise follows the normal gate.

Name the child-only entry points `TestPostgres_FixtureWatchdogChild`,
`TestCLI_RunPlaxWatchdogSupervisorChild`, and `TestCLI_RunPlaxWatchdogChild`;
they skip in ordinary parent runs. No child helper may recursively invoke the
full package suite or acquire the parent's already-held advisory lock. The
private fake-CLI/supervisor machinery exists only for this negative test, not
as a general runner or a change to production script/fixture timeout policy.

## Acceptance criteria

- [x] With the variable unset, all Postgres-dependent tests skip before any
  connection, including the old provenance/base-manager helpers; no localhost fallback.
- [x] With a malformed, unreachable, or stalled explicitly configured endpoint,
  helper and applicable package invocations fail rather than silently skipping.
- [x] A contended lock expires under an injected short wait budget, preserves
  its cause, closes the waiter, and never reaches protected fixture work.
- [x] Normal/failing test bodies release the advisory lock; lost-session/cleanup
  failures are visible, and cleanup has a fresh bounded allowance.
- [x] `make test`/`make check` use explicit serialized package scheduling when
  configured; a fake-go regression verifies flags, the explicit 10m timeout in
  both modes, and failure propagation. No total-run deadline is claimed.
- [x] Configured E2E missing-prerequisite failures are non-zero, not skips.
- [x] Run the documented Postgres-only command against an isolated server with
  `DOCKER_HOST` pointing at a nonexistent test socket and psql available; verify
  real Postgres tests execute without CLI E2E, and report the result as partial.
- [x] Verify both Makefile modes emit exactly one fixed stderr notice before Go,
  including failure paths; no configured URL or credential sentinel is printed.
- [x] Run the configured full suite uncached/race-enabled on an isolated server;
  both infrastructure packages and all eight E2E scenarios execute. Report exact
  durations, failures, skips, and environment limitations; do not claim a faster run.
- [x] Run the unset-variable suite and full formatting/tidy/vet/build/lint checks;
  confirm ordinary unit/local-transport coverage still executes.
- [x] Update AGENTS/testing documentation and the plan inventory; supersede plan
  23's temporary procedure without rewriting its historical verification evidence.
- [x] Confirm runtime source, CLI, production deadlines, CI workflow/service
  configuration, README prose, and release state are unchanged.
- [x] Document that raw fixture SQL/child hangs retain only the package watchdog,
  teardown can remain incomplete on abort, and the existing direct CI path does
  not validate configured serialization or actual infrastructure coverage.
- [x] Run both controlled watchdog-boundary regressions on Linux with the isolated
  Postgres server: confirm the intended SQL/runPlax wait, short child-package
  timeout (not parent-watchdog termination), absent child-cleanup marker, and
  successful parent recovery with no test-owned session/worker/fixture left.
- [ ] Keep the objective/index limited to local scheduling and visible configured
  failures; report CI adoption/non-lock hangs as remaining gaps and leave #83 open
  for a separate verification/maintainer closure decision.
- [x] Verify both approved fixture fixes with repeated uncached race tests, then
  rerun the full configured canonical check without excluding either scenario or
  raising timeout budgets; retain the earlier acceptance failure evidence.

### Local verification evidence (2026-11-12)

The parallel/serial baselines in the Objective remain historical investigation,
not proof of this implementation or a speed claim. Implementation validation used
Linux amd64, Go 1.26.5, golangci-lint v2.15.0, and an explicitly configured,
disposable PostgreSQL 16.15 Docker server on a random loopback port. Docker,
git, python3, and psql were available. Local validation was not a constrained
four-core CI reproduction; some independent local checks overlapped.

Pre-amendment runs, retained as historical evidence:

| Run | Passing test/subtest events | Failures | Skips | Selected package duration |
|---|---:|---:|---:|---|
| Configured full suite, direct `-json -race -count=1 -timeout=10m -p=1 ./...` | 629 | 0 | 7 child-only helpers | CLI 77.042s; Postgres 43.898s; testutil 4.739s |
| Unconfigured canonical `make test`, `GOFLAGS=-json` for evidence | 561 | 0 | 64 (including child-only helpers and explicitly gated Postgres/E2E tests/subtests) | CLI 45.576s; Postgres 8.059s; testutil 3.365s |
| Explicit Postgres-only command, nonexistent `DOCKER_HOST` socket, psql available | 84 | 0 | 1 child-only helper | Postgres 42.917s; partial coverage, no CLI E2E |

- All eight `TestEndToEnd_*` CLI scenarios executed and passed in the configured
  full run. The seven configured skips were only the prerequisite, signal,
  upgrade, two CLI watchdog, SQL watchdog, and testutil child entry points.
  `pkg/stamp` has no test files; it is not included in the test-event skip count.
- Configured canonical `make check` passed with explicit serialized, uncached
  race-enabled scheduling; formatting, module tidy/diff, vet, build, and lint
  checks passed. Initial lint found four local style/unused-assignment diagnostics;
  these were corrected before the clean check.
- Separate malformed/refused/stalled endpoint invocations selected two legacy
  Postgres tests and one real CLI E2E entry point. All nine selected tests failed
  as expected across six package invocations, with no skips, package-watchdog
  termination, or credential sentinels in diagnostics. The stalled runs took
  20.679s (two Postgres setup failures) and 10.966s (one CLI setup failure),
  including Go invocation overhead. These intentional negative results are not
  failures in the green full-suite counts above.
- Fake-go tests verified both exact stderr notices, their ordering before Go,
  explicit flags despite inherited `GOFLAGS=-p=9`, and failure propagation.
  The actual unconfigured canonical run emitted exactly its one fixed notice
  on stderr, outside JSON test output.
- Both controlled watchdog regressions confirmed readiness, the 3s Go timeout,
  missing child-cleanup markers, and parent-owned recovery. The CLI supervisor
  additionally confirmed the worker survived the caller, then stopped and reaped
  that adopted PID. CLI containment/prerequisite and local helper/routing tests
  also passed three consecutive race-enabled repetitions. Non-Linux runtime
  behavior was not exercised; `go list` confirmed selection of the explicit
  unsupported-platform skip file for Darwin.
- Runtime source, CLI surface, product deadlines, CI workflow/services, README,
  module versions, and releases are unchanged. No shared application server/base
  or instance was reset, refreshed, or removed. Raw fixture SQL/CLI waits still
  retain only the package watchdog; no whole-run watchdog or infrastructure CI
  adoption is claimed.

**Recorded acceptance failure (before the approved amendment):** a configured
`make check` failed in two existing plan-23 cases, despite the earlier passing
runs above. CLI passed in 88.430s and testutil in 4.731s; Postgres failed in
59.276s. The full-suite acceptance row was reopened and implementation paused:

- `TestPostgres_BaseScriptCancellationRestoresState/cleanup_failure` left its
  intentionally injected 50ms recovery budget active for ordinary fixture
  teardown. That teardown timed out and left one owned fixture database.
- `TestPostgres_MixedOperationsDoNotTimeOutScripts/refresh` timed out in its
  database-only swap under the test's injected 1s budget, not in a script.
  The diagnostic claiming a script inherited that deadline is misleading for
  this failure. The isolated server logged checkpoint/sync work around and
  beyond one second during this run; that is not proof of script cancellation.

These failures are retained, not hidden by rerunning until green or excluding
tests. The maintainer subsequently approved the test-only amendment in Algorithms:
capture the pre-injection teardown pool/policy and observe mixed-operation context
separation instead of imposing a 1s DDL/storage requirement. Production, canonical,
parent-guard, and intentional-failure budgets are unchanged. Earlier green counts
remain evidence for those specific pre-amendment runs only.

Post-amendment focused validation: one `-race -count=5` invocation exercised five
repetitions of cleanup isolation, script cancellation/recovery, mixed-operation
boundaries, and the unchanged command-duration regression; all passed in 16.692s.
Each mixed-operation run observed all four boundaries (create migrate, seed,
refresh migrate + seed); no fixture databases remained afterward.
An attempted `GOFLAGS=-json make check`
stopped at lint before tests because the linter's `go list -f` conflicts with that
logging flag. Canonical checks were rerun without it; JSON evidence belongs to
`make test` only.

Final post-amendment validation:

- Canonical configured `make check` passed: CLI **77.032s**, Postgres **40.040s**,
  testutil **4.735s**, zero lint issues. Formatting/simplification, module tidy/diff,
  vet, and build checks passed without dependency or production-source changes.
- A separate canonical `make test` used `GOFLAGS=-json` only for structured
  evidence. The subsequent unset and partial runs were sequential, not concurrent:

| Final run | Passing test/subtest events | Failures | Skips | Package / wall durations |
|---|---:|---:|---:|---|
| Configured canonical full suite | 631 | 0 | 7 child-only helpers | CLI 77.303s; Postgres 39.353s; testutil 4.735s; wall 210.420s |
| Unconfigured canonical full suite | 561 | 0 | 65 (child-only helpers and gated Postgres/E2E tests/subtests) | CLI 45.633s; Postgres 8.058s; testutil 3.379s; wall 46.779s |
| Explicit Postgres-only command with a nonexistent Docker socket | 86 | 0 | 1 child-only helper | Postgres 38.945s; wall 39.574s; partial coverage only |

- All eight infrastructure E2E scenarios passed in the final configured run.
  Both Makefile modes emitted exactly one correct fixed stderr notice; the
  direct partial run emitted none. The fixture regressions and both controlled
  watchdog boundaries passed, without raising policy or failure-injection budgets.
- The final server contained no fixture databases, other client sessions, or
  advisory locks after validation; its owned container and attached anonymous
  volume were removed. No shared services or application instances were disturbed.
- Fifteen of sixteen acceptance rows are verified. Only the pre-existing issue
  state conflicts with the approved plan; no infrastructure CI coverage is claimed.

The earlier owned validation server (including the failed test's remaining
fixture DB) and its attached anonymous volume were discarded after preserving the failure
and server logs. Shared services and the existing workload-volume policy were
not changed.

**Issue-state discrepancy:** GitHub reports #83 manually closed by `apollopower`
at 2026-11-13 03:54:56 UTC, before this implementation was published. The
issue-state acceptance row remains unchecked because the approved plan required
leaving it open for CI evidence and a separate remaining-gap decision. This implementation
does not change issue state, infer that decision, or claim full resolution;
reopening or any issue comment requires separate maintainer authorization.

## Dependencies

No new modules. Retain Go 1.26.5, `github.com/jackc/pgx/v5` v5.10.0, and the
existing `golang.org/x/sys` v0.47.0 for test-only Linux subreaper support.
Use standard-library context/errors/time/testing/os/exec/net helpers, existing
GNU make, per-test temporary directories, and the existing Docker/Git/Postgres
fixtures. No mock framework, test daemon, global scheduler, or new runtime package.
