package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func stalledPostgres(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		close(accepted)
		<-stop
	}()
	t.Cleanup(func() {
		close(stop)
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		<-done
	})
	return "postgres://tester:secret@" + ln.Addr().String() + "/postgres?sslmode=disable", accepted
}

func shortTimeouts() timeouts {
	return timeouts{100 * time.Millisecond, 150 * time.Millisecond, 100 * time.Millisecond}
}

func TestPostgres_ConnectDeadline(t *testing.T) {
	dsn, _ := stalledPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	bm, err := newBaseManager(ctx, dsn, ".", testBlueprint(), shortTimeouts())
	if bm != nil {
		bm.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("got %v, want named deadline", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("connection exceeded injected budget")
	}
}

func TestPostgres_ConnectCancellation(t *testing.T) {
	dsn, accepted := stalledPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		bm, err := newBaseManager(ctx, dsn, ".", testBlueprint(), defaultTimeouts())
		if bm != nil {
			bm.Close()
		}
		done <- err
	}()
	select {
	case <-accepted:
		cancel()
	case <-ctx.Done():
		t.Fatal("listener was not reached")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not stop connection")
	}
}

func TestPostgres_TimeoutPolicyValidation(t *testing.T) {
	if got := defaultTimeouts(); got != (timeouts{10 * time.Second, time.Minute, 10 * time.Second}) {
		t.Fatalf("defaults = %+v", got)
	}
	for _, limits := range []timeouts{{0, 1, 1}, {1, -1, 1}, {1, 1, 0}} {
		if _, err := newBaseManager(context.Background(), "postgres://unused", ".", testBlueprint(), limits); err == nil {
			t.Fatalf("accepted %+v", limits)
		}
	}
}

func TestPostgres_PhaseHonorsParent(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ctx, finish := phase(parent, time.Minute, "outer")
	deadline, _ := ctx.Deadline()
	want, _ := parent.Deadline()
	if !deadline.Equal(want) {
		t.Fatal("extended parent deadline")
	}
	cancel()
	err := waitContext(ctx, time.Hour)
	finish(&err)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPostgres_PhaseCancellationWithCleanupTimeout(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	_, finish := phase(parent, time.Minute, "clone owned")
	err := fmt.Errorf("cleanup owned: %w", context.DeadlineExceeded)
	finish(&err)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "clone owned: canceled:") {
		t.Fatalf("cleanup timeout hid parent cancellation: %v", err)
	}
}

func TestPostgres_PhaseBudgetShared(t *testing.T) {
	ctx, finish := phase(context.Background(), 150*time.Millisecond, "outer")
	var err error
	defer finish(&err)
	first, _ := ctx.Deadline()
	if err := waitContext(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	nested, end := phase(ctx, time.Second, "nested")
	second, _ := nested.Deadline()
	if !second.Equal(first) {
		t.Fatal("nested helper restarted the budget")
	}
	err = waitContext(nested, time.Hour)
	end(&err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestPostgres_CleanupFreshBoundedContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	bm := &BaseManager{timeouts: shortTimeouts()}
	err := bm.recovery(parent, "cleanup", func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("cleanup inherited cancellation")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > bm.timeouts.cleanup {
			t.Fatal("unbounded cleanup")
		}
		return bm.databasePhase(ctx, "nested drop", func(nested context.Context) error {
			d, _ := nested.Deadline()
			if !d.Equal(deadline) {
				t.Fatal("nested cleanup extended deadline")
			}
			return waitContext(nested, time.Hour)
		})
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestPostgres_ConnectionErrorsDoNotLeakCredentials(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:plain-secret@localhost:bad/postgres?password=query-secret",
		"postgres://user:encoded%2Fsecret@localhost:bad/postgres?password=query-secret",
	} {
		_, err := newBaseManager(context.Background(), dsn, ".", testBlueprint(), shortTimeouts())
		bm := &BaseManager{dsn: dsn, timeouts: shortTimeouts()}
		_, perDBErr := bm.poolForDB(context.Background(), "owned")
		for _, err := range []error{err, perDBErr} {
			var parseErr *pgconn.ParseConfigError
			if !errors.As(err, &parseErr) {
				t.Fatalf("lost pgx cause: %v", err)
			}
			assertNoCredentials(t, err)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, &pgconn.PgError{Code: "28P01", Message: "plain-secret"}} {
		err := &connectionError{cause: cause}
		if !errors.Is(err, cause) {
			t.Fatal("lost cause")
		}
		assertNoCredentials(t, err)
	}
	// Real lazy pool acquisition must receive the same safe rendering.
	dsn, _ := stalledPostgres(t)
	pool, err := newPool(context.Background(), dsn, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = ReadProvenance(context.Background(), pool)
	if err == nil {
		t.Fatal("expected lazy connection failure")
	}
	assertNoCredentials(t, err)
}

func assertNoCredentials(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{"plain-secret", "encoded/secret", "encoded%2Fsecret", "query-secret", "postgres://", "tester", "secret@"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("diagnostic leaked %q: %v", secret, err)
		}
	}
}

type swapExecutor struct {
	dropErr      error
	drops        int
	afterDrop    func()
	terminateErr error
	renameErr    error
}

func (f *swapExecutor) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "DROP") {
		f.drops++
		if f.afterDrop != nil {
			f.afterDrop()
		}
		return pgconn.CommandTag{}, f.dropErr
	}
	if strings.HasPrefix(sql, "SELECT") && f.drops > 0 {
		return pgconn.CommandTag{}, f.terminateErr
	}
	if strings.HasPrefix(sql, "ALTER") {
		return pgconn.CommandTag{}, f.renameErr
	}
	return pgconn.CommandTag{}, nil
}

func TestPostgres_SwapRetriesOnlyObjectInUse(t *testing.T) {
	for _, cause := range []error{&pgconn.PgError{Code: "55006"}, &pgconn.PgError{Code: "42501"}, fmt.Errorf("transport failed")} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			bm := &BaseManager{baseName: "base", nextName: "next"}
			f := &swapExecutor{dropErr: cause}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := bm.swap(ctx, f)
			var pgErr *pgconn.PgError
			retry := errors.As(cause, &pgErr) && pgErr.Code == "55006"
			if retry {
				if f.drops != 7 || !errors.Is(err, ErrDeferredSwap) || !errors.Is(err, cause) {
					t.Fatalf("drops=%d err=%v", f.drops, err)
				}
			} else if f.drops != 1 || !errors.Is(err, cause) || errors.Is(err, ErrDeferredSwap) {
				t.Fatalf("drops=%d err=%v", f.drops, err)
			}
		})
	}
	t.Run("termination failure between retries", func(t *testing.T) {
		cause := errors.New("termination failed")
		f := &swapExecutor{dropErr: &pgconn.PgError{Code: "55006"}, terminateErr: cause}
		err := (&BaseManager{}).swap(context.Background(), f)
		if f.drops != 1 || !errors.Is(err, cause) || errors.Is(err, ErrDeferredSwap) {
			t.Fatal(err)
		}
	})
	t.Run("rename failure after old base drop", func(t *testing.T) {
		cause := errors.New("rename failed")
		f := &swapExecutor{renameErr: cause}
		err := (&BaseManager{baseName: "old_base", nextName: "staged_base"}).swap(context.Background(), f)
		if !errors.Is(err, cause) || errors.Is(err, ErrDeferredSwap) || !strings.Contains(err.Error(), "unconfirmed after dropping old base") || !strings.Contains(err.Error(), "staged_base") {
			t.Fatal(err)
		}
	})
}

func TestPostgres_SwapDeadlineNotDeferred(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := (&BaseManager{}).swap(ctx, &swapExecutor{dropErr: &pgconn.PgError{Code: "55006"}})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDeferredSwap) {
		t.Fatal(err)
	}
}

func TestPostgres_SwapCancellationNotDeferred(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &swapExecutor{dropErr: &pgconn.PgError{Code: "55006"}, afterDrop: cancel}
	err := (&BaseManager{}).swap(ctx, f)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDeferredSwap) || f.drops != 1 {
		t.Fatal(err)
	}
}
