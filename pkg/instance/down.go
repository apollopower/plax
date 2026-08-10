package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/apollopower/plax/pkg/mailbox"
	"github.com/apollopower/plax/pkg/process"
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

	// Step 1: Terminate native processes. The recorded start time guards
	// against PGID reuse: if the original process is gone and its PGID now
	// belongs to an unrelated process, that process is never signaled.
	for procName, pgid := range rec.PIDs {
		fmt.Fprintf(os.Stderr, "stopping %s...\n", procName)
		err := process.Terminate(pgid, rec.PIDStarts[procName], 5*time.Second)
		switch {
		case errors.Is(err, process.ErrStaleProcess):
			fmt.Fprintf(os.Stderr, "note: %s already gone (pgid %d reused by another process)\n", procName, pgid)
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: terminate %s (pgid %d): %v\n", procName, pgid, err)
		}
	}

	// Step 2: Stop and remove dedicated containers.
	if deps.Docker != nil {
		for svcName, cid := range rec.ContainerIDs {
			fmt.Fprintf(os.Stderr, "stopping %s...\n", svcName)
			if err := deps.Docker.StopService(ctx, cid); err != nil {
				fmt.Fprintf(os.Stderr, "warning: stop %s: %v\n", svcName, err)
			}
			if err := deps.Docker.RemoveService(ctx, cid); err != nil {
				fmt.Fprintf(os.Stderr, "warning: remove %s: %v\n", svcName, err)
			}
		}
	} else if len(rec.ContainerIDs) > 0 {
		fmt.Fprintf(os.Stderr, "warning: docker unavailable — skipping container removal for %d service(s)\n", len(rec.ContainerIDs))
	}

	// Step 3: Remove container volumes.
	// ServiceDef does not yet declare volumes, so this is a no-op.
	// When volumes are added, iterate the same list that Up used.

	// Step 4: Drop instance database.
	if deps.BM != nil {
		fmt.Fprintf(os.Stderr, "dropping database %s...\n", rec.DBName)
		if err := deps.BM.DropInstanceDB(ctx, rec.DBName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: drop database: %v\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "warning: postgres unavailable — skipping database drop for %s\n", rec.DBName)
	}

	// Step 5: Remove Docker network.
	if deps.Docker != nil {
		netName := "plax-" + name + "-net"
		if err := deps.Docker.RemoveNetwork(ctx, netName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove network: %v\n", err)
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
