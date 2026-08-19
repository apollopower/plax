package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/apollopower/plax/pkg/mailbox"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/worktree"
)

// Down destroys an instance: kills processes, removes containers, drops
// database, removes network, releases ports, removes worktree, deletes
// branch, removes registry entry.
//
// Down is deliberately not atomic. Steps are idempotent and tolerant of
// missing resources — failures are logged to stderr and execution continues.
// A nil BM or Docker (backend unavailable at startup) skips only that
// backend's resources. The registry entry is always removed because it is
// the source of truth for what exists; leaving a stale entry would block
// future Up calls and leak port allocations.
func Down(ctx context.Context, deps *Deps, name string) error {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return fmt.Errorf("instance %q not found", name)
	}

	// Step 1: Terminate native processes concurrently. The recorded start
	// time guards against PGID reuse: if the original process is gone and its
	// PGID now belongs to an unrelated process, that process is never signaled.
	{
		type procResult struct {
			name string
			err  error
		}
		ch := make(chan procResult, len(rec.PIDs))
		for procName, pgid := range rec.PIDs {
			go func(name string, pgid int) {
				err := process.Terminate(pgid, rec.PIDStarts[name], 5*time.Second)
				ch <- procResult{name, err}
			}(procName, pgid)
		}
		for range rec.PIDs {
			r := <-ch
			switch {
			case errors.Is(r.err, process.ErrStaleProcess):
				fmt.Fprintf(os.Stderr, "note: %s already gone (pgid %d reused by another process)\n", r.name, rec.PIDs[r.name])
			case errors.Is(r.err, process.ErrGroupSurvivors):
				fmt.Fprintf(os.Stderr, "warning: %s process leader died but children survived (pgid %d) — teardown continuing\n", r.name, rec.PIDs[r.name])
			case r.err != nil:
				fmt.Fprintf(os.Stderr, "warning: terminate %s (pgid %d): %v\n", r.name, rec.PIDs[r.name], r.err)
			}
		}
	}

	// Step 2: Stop and remove dedicated containers concurrently.
	if deps.Docker != nil {
		type svcResult struct {
			name string
			err  error
		}
		ch := make(chan svcResult, len(rec.ContainerIDs))
		for svcName, cid := range rec.ContainerIDs {
			go func(name, cid string) {
				stopErr := deps.Docker.StopService(ctx, cid)
				rmErr := deps.Docker.RemoveService(ctx, cid)
				if stopErr != nil {
					ch <- svcResult{name, stopErr}
					return
				}
				ch <- svcResult{name, rmErr}
			}(svcName, cid)
		}
		for range rec.ContainerIDs {
			r := <-ch
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", r.name, r.err)
			}
		}
	} else if len(rec.ContainerIDs) > 0 {
		fmt.Fprintf(os.Stderr, "warning: docker unavailable — skipping container removal for %d service(s)\n", len(rec.ContainerIDs))
	}

	// Step 3: Remove container volumes.
	// ServiceDef does not yet declare volumes, so this is a no-op.
	// When volumes are added, iterate the same list that Up used.

	// Step 4: Drop all instance databases.
	names := registry.DBNamesFromRecord(rec)
	if deps.BM != nil {
		for _, dbName := range names {
			fmt.Fprintf(os.Stderr, "dropping database %s...\n", dbName)
			if err := deps.BM.DropInstanceDB(ctx, dbName); err != nil {
				fmt.Fprintf(os.Stderr, "warning: drop database %s: %v\n", dbName, err)
			}
		}
	} else if len(names) > 0 {
		fmt.Fprintf(os.Stderr, "warning: postgres unavailable — skipping database drop for %d database(s)\n", len(names))
	}

	// Step 5: Remove Docker network.
	if deps.Docker != nil {
		netName := "plax-" + name + "-net"
		if err := deps.Docker.RemoveNetwork(ctx, netName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove network: %v\n", err)
		}
	}

	// Step 5.5: Best-effort scratch removal. git worktree remove deletes the
	// whole directory, so this normally no-ops; it covers a failed worktree
	// removal stranding a scratch tree full of agent debris.
	if rec.WorktreePath != "" {
		if err := os.RemoveAll(filepath.Join(rec.WorktreePath, "scratch")); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove scratch: %v\n", err)
		}
	}

	// Step 6: Remove worktree and branch. Remove deletes the branch even
	// when the worktree itself is already gone.
	fmt.Fprintf(os.Stderr, "removing worktree...\n")
	if err := worktree.Remove(deps.RepoRoot, name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: remove worktree: %v\n", err)
	}

	// Step 7: Remove mailbox directory.
	if err := mailbox.RemoveDir(deps.RepoRoot, name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: remove mailbox: %v\n", err)
	}

	// Step 8: Remove registry entry. Always runs — even if earlier steps
	// failed — because the registry is the source of truth for what exists.
	if err := deps.Registry.RemoveInstance(name); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if err := deps.Registry.Save(); err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	fmt.Fprintf(os.Stderr, "instance %s down\n", name)
	return nil
}
