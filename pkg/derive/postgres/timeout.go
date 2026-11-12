package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCleanupTimeout bounds one rollback/recovery attempt, not forward teardown.
const DefaultCleanupTimeout = 10 * time.Second

type timeouts struct {
	connect   time.Duration
	operation time.Duration
	cleanup   time.Duration
}

func defaultTimeouts() timeouts {
	return timeouts{10 * time.Second, 60 * time.Second, DefaultCleanupTimeout}
}

type connectionError struct {
	cause error
}

func (e *connectionError) Unwrap() error { return e.cause }

func (e *connectionError) Error() string {
	switch {
	case errors.Is(e.cause, context.Canceled):
		return "connection canceled: context canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		return "connection timed out: context deadline exceeded"
	}
	var pgErr *pgconn.PgError
	if errors.As(e.cause, &pgErr) {
		if pgErr.Code == "28P01" || pgErr.Code == "28000" {
			return "connection authentication failed (SQLSTATE " + pgErr.Code + ")"
		}
		return "connection failed (SQLSTATE " + pgErr.Code + ")"
	}
	return "connection failed"
}

func safeDBError(err error) error {
	var safe *connectionError
	if errors.As(err, &safe) {
		return err
	}
	var connErr *pgconn.ConnectError
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &connErr) || errors.As(err, &parseErr) {
		return &connectionError{cause: err}
	}
	return err
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// The parent deadline wins, so constituent/nested calls cannot restart a phase.
func phase(ctx context.Context, limit time.Duration, op string) (context.Context, func(*error)) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	deadline, _ := ctx.Deadline()
	budget := max(0, time.Until(deadline)).Round(time.Millisecond)
	return ctx, func(err *error) {
		defer cancel()
		if *err == nil {
			*err = ctx.Err()
		}
		if *err == nil {
			return
		}
		if ctx.Err() != nil && !errors.Is(*err, ctx.Err()) {
			*err = errors.Join(*err, ctx.Err())
		}
		*err = safeDBError(*err)
		if errors.Is(*err, context.Canceled) {
			*err = fmt.Errorf("%s: canceled: %w", op, *err)
		} else if errors.Is(*err, context.DeadlineExceeded) {
			*err = fmt.Errorf("%s: timed out (phase budget %s): %w", op, budget, *err)
		} else {
			*err = fmt.Errorf("%s: %w", op, *err)
		}
	}
}

func (bm *BaseManager) databasePhase(ctx context.Context, op string, fn func(context.Context) error) (err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, op)
	defer finish(&err)
	return fn(ctx)
}

func (bm *BaseManager) recovery(ctx context.Context, op string, fn func(context.Context) error) (err error) {
	ctx, finish := phase(context.WithoutCancel(ctx), bm.timeouts.cleanup, op)
	defer finish(&err)
	return fn(ctx)
}

func newPool(ctx context.Context, dsn string, connectTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, &connectionError{cause: err}
	}
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, &connectionError{cause: err}
	}
	return pool, nil
}

func (bm *BaseManager) poolForDB(ctx context.Context, name string) (*pgxpool.Pool, error) {
	return newPool(ctx, bm.dsnForDB(name), bm.timeouts.connect)
}
