package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
)

func waitMarker(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := waitContext(ctx, 10*time.Millisecond); err != nil {
			t.Fatalf("waiting for %s: %v", path, err)
		}
	}
}

func TestPostgres_BaseCommandCancellationStopsChildren(t *testing.T) {
	dir := t.TempDir()
	bm := &BaseManager{repoRoot: dir, bp: &blueprint.Blueprint{}, timeouts: shortTimeouts()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bm.runCommand(ctx, `sh -c 'echo ready > ready; sleep 30; echo escaped > escaped' & wait`, "base")
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case err := <-done:
				if err != nil {
					t.Logf("watchdog cleanup: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("script cleanup did not finish")
			}
		}
	}()
	waitMarker(t, filepath.Join(dir, "ready"))
	cancel()
	select {
	case err := <-done:
		finished = true
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child held output pipe open after cancellation")
	}
}

func TestPostgres_BaseCommandHasNoAutomaticDeadline(t *testing.T) {
	bm := &BaseManager{repoRoot: t.TempDir(), bp: &blueprint.Blueprint{}, timeouts: shortTimeouts()}
	bm.timeouts.operation = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := bm.runCommand(ctx, "sleep 0.1", "base"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) <= bm.timeouts.operation {
		t.Fatal("script did not outlast database budget")
	}
	cancel()
	if err := bm.runCommand(ctx, "touch unexpected", "base"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bm.repoRoot, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("canceled script started")
	}
}
