// Package testutil holds helpers shared across package test suites.
package testutil

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Direct go test runs can overlap package binaries. Database-mutating fixtures
// share this stable key, including CLI children protected by the parent session.
const pgLockKey = 7078410

// LockPostgres takes a session-scoped advisory lock, released when the test
// finishes. Acquire it once per test — pg_advisory_lock is not re-entrant
// across sessions, so a test must not call this twice.
func LockPostgres(t *testing.T, url string) {
	t.Helper()
	ctx := t.Context()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	limits := defaultPostgresLimits()
	conn, err := acquirePostgresLock(ctx, url, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := releasePostgresLock(ctx, conn, limits.cleanup); err != nil {
			t.Error(err)
		}
	})
}

// PostgresURL skips before any real Postgres setup unless explicitly opted in.
func PostgresURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PLAX_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PLAX_TEST_POSTGRES_URL unset/empty; skipping Postgres integration")
	}
	return url
}

type postgresLimits struct {
	connect time.Duration
	lock    time.Duration
	cleanup time.Duration
}

func defaultPostgresLimits() postgresLimits {
	return postgresLimits{10 * time.Second, 2 * time.Minute, 10 * time.Second}
}

type postgresError struct {
	op    string
	cause error
}

func (e *postgresError) Unwrap() error { return e.cause }

func (e *postgresError) Error() string {
	classification := "failed"
	switch {
	case errors.Is(e.cause, context.Canceled):
		classification = "canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		classification = "deadline exceeded"
	default:
		var pgErr *pgconn.PgError
		if errors.As(e.cause, &pgErr) && validSQLState(pgErr.Code) {
			classification += " (SQLSTATE " + pgErr.Code + ")"
		}
	}
	return "Postgres test " + e.op + ": " + classification
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func acquirePostgresLock(ctx context.Context, url string, limits postgresLimits) (*pgx.Conn, error) {
	if limits.connect <= 0 || limits.lock <= 0 || limits.cleanup <= 0 {
		return nil, errors.New("postgres test limits must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, &postgresError{"connect", err}
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, &postgresError{"configure", err}
	}
	cfg.ConnectTimeout = limits.connect
	connectCtx, cancel := context.WithTimeout(ctx, limits.connect)
	conn, err := pgx.ConnectConfig(connectCtx, cfg)
	if err != nil {
		err = errors.Join(err, connectCtx.Err())
	}
	cancel()
	if err != nil {
		return nil, &postgresError{"connect", err}
	}
	lockCtx, cancel := context.WithTimeout(ctx, limits.lock)
	_, err = conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", pgLockKey)
	err = errors.Join(err, lockCtx.Err())
	cancel()
	if err == nil {
		return conn, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limits.cleanup)
	defer cancel()
	closeErr := conn.Close(cleanupCtx)
	if closeErr != nil {
		closeErr = &postgresError{"close after acquisition failure", errors.Join(closeErr, cleanupCtx.Err())}
	}
	return nil, errors.Join(&postgresError{"lock", err}, closeErr)
}

func releasePostgresLock(ctx context.Context, conn *pgx.Conn, cleanupTimeout time.Duration) error {
	if cleanupTimeout <= 0 {
		return errors.New("postgres test cleanup limit must be positive")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var held bool
	err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", pgLockKey).Scan(&held)
	if err == nil && !held {
		err = errors.New("expected advisory lock not held")
	}
	var unlockErr, closeErr error
	if err != nil {
		unlockErr = &postgresError{"unlock", errors.Join(err, ctx.Err())}
	}
	if err := conn.Close(ctx); err != nil {
		closeErr = &postgresError{"close", errors.Join(err, ctx.Err())}
	}
	return errors.Join(unlockErr, closeErr)
}
