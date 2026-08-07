// Package testutil holds helpers shared across package test suites.
package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// pgLockKey is an arbitrary, stable advisory-lock key. Tests that create,
// drop, or clone the well-known plax_base database hold this lock for their
// whole duration, because `go test` runs package binaries in parallel and
// both pkg/derive/postgres and the cmd/plax end-to-end test share that
// database on the same server.
const pgLockKey = 7078410

// LockPostgres takes a session-scoped advisory lock, released when the test
// finishes. Acquire it once per test — pg_advisory_lock is not re-entrant
// across sessions, so a test must not call this twice.
//
// Skips when Postgres is unreachable, matching the integration tests' own
// behavior.
func LockPostgres(t *testing.T, url string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Skipf("skipping: cannot connect to Postgres (%s): %v", url, err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", pgLockKey); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", pgLockKey)
		_ = conn.Close(ctx)
	})
}
