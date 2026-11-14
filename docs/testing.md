# Testing

## Canonical checks and coverage modes

Run `make check` for formatting, vet, lint, and tests. Also run `gofmt -s -l .`,
`go mod tidy -diff`, and `go build ./...` before committing.

```sh
# No real Postgres connections; integration and CLI E2E skip before setup.
env -u PLAX_TEST_POSTGRES_URL make test

# Full suite, including real Postgres and all eight CLI E2E scenarios.
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' make check
```

Both modes run `go test -race -count=1 -timeout=10m ./...`. A nonempty URL adds
explicit `-p=1` so package binaries run serially, even with an inherited
`GOFLAGS=-p=...`. Unset/empty retains Go's ordinary package scheduling.
Serializing packages is not a speed optimization or goroutine scheduler.

The Makefile prints exactly one fixed stderr notice before invoking Go:

```text
tests: Postgres disabled (PLAX_TEST_POSTGRES_URL unset/empty); skipping Postgres integration and CLI E2E.
tests: Postgres + CLI E2E enabled; serial package execution (-p=1).
```

The URL is never part of the notice. A green unconfigured run has reduced
Postgres coverage, not necessarily zero infrastructure: Docker-only package
tests retain their own availability policy, and local TCP transport regressions
still execute. Configured Postgres setup failures are failures, never availability
skips. Full-suite CLI E2E also requires git, python3, psql, and a working Docker
daemon; missing prerequisites fail before creating fixtures.

## Safe server setup

Use a disposable **isolated server**, not merely a separate database on a shared
application server. Tests create/drop databases, change connection permissions,
terminate identified fixture sessions, and exercise template cloning. A user
with database-creation and fixture-session termination privileges is required.
Existing migration/seed fixtures use `psql` and `bash` from `PATH`.

For example, create a dedicated PostgreSQL 16 container with a throwaway password
and a randomly assigned loopback port:

```sh
docker run --detach --name plax-tests-postgres \
  --publish 127.0.0.1::5432 \
  --env POSTGRES_PASSWORD='<disposable-password>' postgres:16
docker exec plax-tests-postgres pg_isready -U postgres
docker port plax-tests-postgres 5432/tcp
```

Wait for readiness before running tests. Set `PLAX_TEST_POSTGRES_URL` to that
endpoint's `postgres` database, using the reported port and throwaway password
(with appropriate URL escaping), and `sslmode=disable` for this local container.
Never paste an actual credential-bearing URL into shared reports. After test
cleanup and inspection, remove only this owned container and its owned volume:

```sh
docker rm --force --volumes plax-tests-postgres
```

Choose a different unique container name if that name already exists. Do not
stop, reset, refresh, or clean up another application's server/base/instances.

## Direct and partial runs

Makefile notices/routing do not apply to direct Go commands. For an equivalent
configured full run, export the URL first, then:

```sh
go test -race -count=1 -timeout=10m -p=1 ./...
```

Developers with Postgres but no Docker can run just the Postgres package:

```sh
PLAX_TEST_POSTGRES_URL='<isolated-test-url>' \
  go test -race -count=1 -timeout=10m ./pkg/derive/postgres
```

This still requires `psql`. To include real advisory-lock helper regressions,
select `./pkg/testutil ./pkg/derive/postgres` with explicit `-p=1`. These are
**partial coverage**, not full-suite `make check` or CLI E2E acceptance. An invalid
`DOCKER_HOST` is harmless for these explicitly selected packages; do not weaken
full-suite prerequisite failures into skips. Add `-v` to direct runs to inspect
individual skips and watchdog-boundary evidence.

## Budgets, remaining hangs, and recovery

The shared advisory lock uses key `7078410`: 10s to connect, a separate 2m to
acquire, and one fresh cancellation-independent 10s budget for unlock + close.
Earlier caller cancellation/deadlines win during setup. Close is always attempted
after unlock failure. Acquire only once per outer mutating fixture, before
registering resource cleanups; LIFO releases the lock last. Keep parent-held
sessions alive until spawned CLI work and its cleanup finish. This is not a
cross-process lease monitor or a promise of protection after session loss.

Failure-injection fixtures capture their ordinary cleanup pool/policy before
injecting short limits. The mixed-operation regression observes completed DB
contexts at gated script boundaries instead of requiring real DDL to finish in
1s. A separate command-only regression retains the short-budget duration check.

**Raw fixture SQL and existing `runPlax` waits still have only the Go package
watchdog as their test-harness backstop.** Product database-phase limits do not
cover every fixture call or intentionally deadline-free migration script. Earlier
resource cleanup can hang before lock cleanup begins, so the 10s lock-cleanup
allowance is not a bound on total teardown. The explicit 10m timeout is per
package, not per test or whole run: N stuck serialized packages may take roughly
N × 10m plus build/other work.

A package timeout aborts the test process and can bypass `t.Cleanup`, leaving
database fixtures or CLI/workload descendants. Inspect the output and reclaim
only resources owned by that disposable run. Do not assume rerunning tests cleans
everything, use broad process/database-prefix kills, or operate on shared services.
Prefer discarding the isolated server after inspecting it. Anonymous workload
volumes and broader teardown semantics are not changed here.

Two controlled regressions demonstrate, rather than fix, this residual gap:
`TestPostgres_FixturePackageWatchdogBoundary` blocks raw SQL on an owned table;
`TestCLI_RunPlaxPackageWatchdogBoundary` blocks the real helper on a fake CLI.
They use a 3s child-package timeout with an independent 15s parent guard, require
readiness and the Go timeout diagnostic, show missing child-cleanup markers,
and recover their exact owned fixtures/workers from the parent. The CLI case
uses a throwaway Linux child-subreaper supervisor; other platforms explicitly
skip that containment regression. Child-only test entry points skip normally.

## CI adoption is separate

The current CI workflow still runs `go test -race -count=1 ./...` directly with
no configured Postgres server. It bypasses Makefile routing and notices; fake-go
regressions validate those decisions but not actual configured infrastructure
execution in CI. Provisioning and adoption of the canonical path require a
separate change and a successful configured CI run. This local scheduling/setup
hardening does not prove that historical starvation was fixed or establish #83
resolution; configured CI evidence and a maintainer decision on the remaining
non-lock hangs are still needed.
