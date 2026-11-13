package testutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func shortPostgresLimits() postgresLimits {
	return postgresLimits{100 * time.Millisecond, 150 * time.Millisecond, time.Second}
}

func helperChild(t *testing.T, mode, dsn string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTestutil_PostgresHelperChild$", "-test.v", "-test.timeout=12s")
	cmd.Env = append(os.Environ(), "PLAX_TEST_HELPER="+mode, "PLAX_TEST_POSTGRES_URL="+dsn)
	if mode == "gate-unset" {
		cmd.Env = nil
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "PLAX_TEST_POSTGRES_URL=") && !strings.HasPrefix(entry, "PLAX_TEST_HELPER=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "PLAX_TEST_HELPER="+mode)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("helper child exceeded independent watchdog")
	}
	return string(out), err
}

func TestTestutil_PostgresHelperChild(t *testing.T) {
	mode := os.Getenv("PLAX_TEST_HELPER")
	if mode == "" {
		t.Skip("child-only helper")
	}
	dsn := PostgresURL(t)
	LockPostgres(t, dsn)
	fmt.Println("PROTECTED")
	switch mode {
	case "fail":
		t.Fatal("intentional protected-body failure")
	case "lost":
		conn := testConnection(t, dsn)
		var pid uint32
		if err := conn.QueryRow(t.Context(), `SELECT pid FROM pg_locks
			WHERE locktype='advisory' AND classid=0 AND objid=$1 AND objsubid=1 AND granted
			AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`, pgLockKey).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(), "SELECT pg_terminate_backend($1)", pid); err != nil {
			t.Fatal(err)
		}
	case "normal", "gate":
	default:
		t.Fatal("unknown child mode")
	}
}

func TestTestutil_PostgresURLUnsetSkips(t *testing.T) {
	for _, mode := range []string{"unset", "empty"} {
		t.Run(mode, func(t *testing.T) {
			// The child's gate must run before LockPostgres or the body marker.
			childMode := "gate"
			if mode == "unset" {
				childMode = "gate-unset"
			}
			out, err := helperChild(t, childMode, "")
			if err != nil || !strings.Contains(out, "--- SKIP:") || strings.Contains(out, "PROTECTED") {
				t.Fatalf("gate did not skip before setup: err=%v\n%s", err, out)
			}
		})
	}
}

func refusedPostgres(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return "postgres://user:plain-secret@" + addr + "/postgres?sslmode=disable"
}

func TestTestutil_ConfiguredPostgresFailureIsFatal(t *testing.T) {
	for _, dsn := range []string{"postgres://user:plain-secret@localhost:bad/db", refusedPostgres(t)} {
		out, err := helperChild(t, "normal", dsn)
		if err == nil || strings.Contains(out, "--- SKIP:") || strings.Contains(out, "PROTECTED") || !strings.Contains(out, "Postgres test") {
			t.Fatalf("configured failure was not fatal before body: err=%v\n%s", err, out)
		}
		assertSafeDiagnostic(t, out)
	}
}

func stalledConnection(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		<-stop
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		<-done
	})
	return "postgres://user:plain-secret@" + ln.Addr().String() + "/postgres?sslmode=disable", accepted
}

func TestTestutil_PostgresConnectDeadline(t *testing.T) {
	dsn, accepted := stalledConnection(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := acquirePostgresLock(ctx, dsn, shortPostgresLimits())
	if conn != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("connection deadline lost: %v", err)
	}
	select {
	case <-accepted:
	default:
		t.Fatal("listener never reached")
	}
	assertSafeDiagnostic(t, err.Error())
}

func TestTestutil_PostgresCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := acquirePostgresLock(ctx, "not even a DSN", shortPostgresLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-canceled parent not honored: %v", err)
	}
	for _, mode := range []string{"cancel", "earlier deadline"} {
		t.Run(mode, func(t *testing.T) {
			dsn, accepted := stalledConnection(t)
			limit := 100 * time.Millisecond
			if mode == "cancel" {
				limit = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), limit)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := acquirePostgresLock(ctx, dsn, defaultPostgresLimits())
				done <- err
			}()
			select {
			case <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("listener not reached")
			}
			want := context.DeadlineExceeded
			if mode == "cancel" {
				cancel()
				want = context.Canceled
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("parent did not stop setup")
			}
		})
	}
}

func TestTestutil_PostgresLimitValidation(t *testing.T) {
	if got := defaultPostgresLimits(); got != (postgresLimits{10 * time.Second, 2 * time.Minute, 10 * time.Second}) {
		t.Fatalf("defaults: %+v", got)
	}
	for _, limits := range []postgresLimits{{0, 1, 1}, {1, -1, 1}, {1, 1, 0}} {
		if _, err := acquirePostgresLock(t.Context(), "unused", limits); err == nil {
			t.Fatal("accepted invalid limits")
		}
	}
	if err := releasePostgresLock(t.Context(), nil, 0); err == nil {
		t.Fatal("accepted invalid cleanup limit")
	}
}

func assertSafeDiagnostic(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{"plain-secret", "encoded%2Fsecret", "encoded/secret", "query-secret", "postgres://"} {
		if strings.Contains(text, secret) {
			t.Fatal("diagnostic leaked credential/URL sentinel")
		}
	}
}

func TestTestutil_PostgresDiagnosticsHideCredentials(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:plain-secret@localhost:bad/db?password=query-secret",
		"postgres://user:encoded%2Fsecret@localhost:bad/db?password=query-secret",
	} {
		_, err := acquirePostgresLock(t.Context(), dsn, shortPostgresLimits())
		var parseErr *pgconn.ParseConfigError
		if !errors.As(err, &parseErr) {
			t.Fatal("lost parse-error identity")
		}
		assertSafeDiagnostic(t, err.Error())
		out, err := helperChild(t, "normal", dsn)
		if err == nil {
			t.Fatal("child unexpectedly succeeded")
		}
		assertSafeDiagnostic(t, out)
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded,
		&pgconn.PgError{Code: "28P01", Message: "query-secret"},
		&pgconn.PgError{Code: "query-secret", Message: "plain-secret"}} {
		err := &postgresError{"connect", cause}
		if !errors.Is(err, cause) {
			t.Fatal("lost original cause")
		}
		assertSafeDiagnostic(t, err.Error())
	}
}

func testConnection(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(&postgresError{"test connect", err})
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := conn.Close(ctx); err != nil {
			t.Error(&postgresError{"test close", err})
		}
	})
	return conn
}

func TestTestutil_PostgresLockContentionDeadline(t *testing.T) {
	dsn := PostgresURL(t)
	holder, err := acquirePostgresLock(t.Context(), dsn, defaultPostgresLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := releasePostgresLock(t.Context(), holder, time.Second); err != nil {
			t.Error(err)
		}
	})
	app := fmt.Sprintf("plax_waiter_%d", time.Now().UnixNano())
	waiterDSN := dsn + " application_name=" + app
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("cannot configure waiter URL")
		}
		q := u.Query()
		q.Set("application_name", app)
		u.RawQuery = q.Encode()
		waiterDSN = u.String()
	}
	limits := defaultPostgresLimits()
	limits.lock = 100 * time.Millisecond
	conn, err := acquirePostgresLock(t.Context(), waiterDSN, limits)
	if conn != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter reached protected work or lost deadline: %v", err)
	}
	probe := testConnection(t, dsn)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		var count int
		if err := probe.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", app).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("waiter session survived")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestTestutil_PostgresLockReleasedOnCleanup(t *testing.T) {
	dsn := PostgresURL(t)
	for _, mode := range []string{"normal", "fail"} {
		out, err := helperChild(t, mode, dsn)
		if (err != nil) != (mode == "fail") || !strings.Contains(out, "PROTECTED") || strings.Contains(out, "Postgres test unlock") {
			t.Fatalf("unexpected child cleanup: %v\n%s", err, out)
		}
		conn, err := acquirePostgresLock(t.Context(), dsn, shortPostgresLimits())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := releasePostgresLock(ctx, conn, time.Second); err != nil {
			t.Fatalf("cleanup inherited cancellation: %v", err)
		}
		if !conn.IsClosed() {
			t.Fatal("cleanup did not close session")
		}
	}
}

func TestTestutil_PostgresLockCleanupFailureIsReported(t *testing.T) {
	dsn := PostgresURL(t)
	out, err := helperChild(t, "lost", dsn)
	if err == nil || !strings.Contains(out, "PROTECTED") || !strings.Contains(out, "Postgres test unlock: failed") {
		t.Fatalf("cleanup failure not reported: %v\n%s", err, out)
	}
	conn, err := acquirePostgresLock(t.Context(), dsn, shortPostgresLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", pgLockKey); err != nil {
		t.Fatal(err)
	}
	if err := releasePostgresLock(t.Context(), conn, time.Second); err == nil || !conn.IsClosed() {
		t.Fatal("missing lock was not reported/connection not closed")
	}
}

func fakeMakeTest(t *testing.T, configured bool, status string) (string, string, string, error) {
	t.Helper()
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), data, 0600); err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\nprintf 'GO_STARTED\\n' >&2\nexit \"$GO_STATUS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dsn := ""
	if configured {
		dsn = "postgres://user:plain-secret@unused/db?password=query-secret"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, makeBin, "--no-print-directory", "test")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir, "ARGS_FILE="+filepath.Join(dir, "args"), "GO_STATUS="+status,
		"PLAX_TEST_POSTGRES_URL="+dsn, "GOFLAGS=-p=9", "MAKEFLAGS=", "MFLAGS=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	args, readErr := os.ReadFile(filepath.Join(dir, "args"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(args), stdout.String(), stderr.String(), err
}

func checkModeNotice(t *testing.T, configured bool, stdout, stderr string) {
	t.Helper()
	want := "tests: Postgres disabled (PLAX_TEST_POSTGRES_URL unset/empty); skipping Postgres integration and CLI E2E."
	if configured {
		want = "tests: Postgres + CLI E2E enabled; serial package execution (-p=1)."
	}
	if stdout != "" || strings.Count(stderr, "tests: Postgres") != 1 || !strings.HasPrefix(stderr, want+"\nGO_STARTED\n") {
		t.Fatalf("mode notice missing, duplicated, or not before Go: stdout=%q stderr=%q", stdout, stderr)
	}
	assertSafeDiagnostic(t, stdout+stderr)
}

func TestTestutil_MakeTestRoutesByConfiguration(t *testing.T) {
	for _, configured := range []bool{false, true} {
		args, stdout, stderr, err := fakeMakeTest(t, configured, "0")
		if err != nil {
			t.Fatal(err)
		}
		want := "test\n-race\n-count=1\n-timeout=10m\n"
		if configured {
			want += "-p=1\n"
		}
		if args != want+"./...\n" {
			t.Fatalf("Go flags = %q", args)
		}
		checkModeNotice(t, configured, stdout, stderr)
	}
}

func TestTestutil_MakeTestPropagatesFailure(t *testing.T) {
	for _, configured := range []bool{false, true} {
		_, stdout, stderr, err := fakeMakeTest(t, configured, "7")
		if err == nil {
			t.Fatal("make hid Go failure")
		}
		checkModeNotice(t, configured, stdout, stderr)
	}
}
