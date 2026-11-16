package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/testutil"
	"github.com/jackc/pgx/v5"
)

func TestPostgres_FixtureWatchdogChild(t *testing.T) {
	name := os.Getenv("PLAX_TEST_WATCHDOG_DB")
	if name == "" {
		t.Skip("child-only helper")
	}
	cfg := testDatabaseConfig(t, testutil.PostgresURL(t), name)
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(&connectionError{cause: err})
	}
	t.Cleanup(func() {
		if err := os.WriteFile(os.Getenv("PLAX_TEST_WATCHDOG_CLEANUP"), []byte("cleaned"), 0600); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := conn.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	// This deliberately bypasses product phase limits to expose the fixture gap.
	if _, err := conn.Exec(context.Background(), "SELECT * FROM watchdog_fixture"); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fixture query unexpectedly completed")
}

func TestPostgres_FixturePackageWatchdogBoundary(t *testing.T) {
	dsn := testutil.PostgresURL(t)
	testutil.LockPostgres(t, dsn)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(&connectionError{cause: err})
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	name := fmt.Sprintf("plax_watchdog_%x", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	var holder *pgx.Conn
	var tx pgx.Tx
	var backendPID int32
	recovered := false
	recoverFixture := func() {
		if recovered {
			return
		}
		recovered = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if backendPID != 0 {
			if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE pid=$1 AND datname=$2 AND application_name=$2`, backendPID, name); err != nil {
				t.Error(err)
			}
		}
		if tx != nil {
			if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error(err)
			}
		}
		if holder != nil {
			if err := holder.Close(ctx); err != nil {
				t.Error(err)
			}
		}
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
			t.Error(err)
		}
		var remains bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_database WHERE datname=$1)
			OR EXISTS(SELECT FROM pg_stat_activity WHERE datname=$1)`, name).Scan(&remains); err != nil {
			t.Error(err)
		} else if remains {
			t.Error("watchdog fixture/session remains after parent recovery")
		} else {
			t.Log("parent recovery: owned SQL session, lock, and fixture reclaimed")
		}
	}
	t.Cleanup(recoverFixture)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	cfg := testDatabaseConfig(t, dsn, name)
	holder, err = pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(&connectionError{cause: err})
	}
	if _, err := holder.Exec(ctx, "CREATE TABLE watchdog_fixture (id integer)"); err != nil {
		t.Fatal(err)
	}
	tx, err = holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE watchdog_fixture IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-cleanup")
	guard, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	cmd := exec.CommandContext(guard, os.Args[0], "-test.run=^TestPostgres_FixtureWatchdogChild$", "-test.timeout=3s", "-test.v")
	cmd.Env = append(os.Environ(), "PLAX_TEST_WATCHDOG_DB="+name, "PLAX_TEST_WATCHDOG_CLEANUP="+marker)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		stop()
		if !waited {
			if err := <-done; err != nil {
				t.Logf("reaped failed SQL watchdog child: %v", err)
			}
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := admin.QueryRow(guard, `SELECT pid FROM pg_stat_activity
			WHERE datname=$1 AND application_name=$1 AND wait_event_type='Lock'`, name).Scan(&backendPID)
		if err == nil {
			t.Logf("readiness: owned backend %d blocked in raw fixture SQL", backendPID)
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			waited = true
			t.Fatalf("child exited without confirmed SQL wait: %v\n%s", err, out.String())
		case <-guard.Done():
			t.Fatal("SQL readiness exceeded independent parent watchdog")
		case <-ticker.C:
		}
	}
	err = <-done
	waited = true
	if guard.Err() != nil || err == nil || !strings.Contains(out.String(), "panic: test timed out after 3s") {
		t.Fatalf("expected package watchdog, not parent termination: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("child cleanup marker unexpectedly present or unreadable")
	}
	t.Log("Go package timeout after 3s; child-cleanup marker absent")
	recoverFixture()
}
