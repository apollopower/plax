package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/apollopower/plax/pkg/process"
)

func Suspend(ctx context.Context, deps *Deps, name string) error {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return fmt.Errorf("instance %q not found", name)
	}

	if rec.State == "suspended" {
		fmt.Fprintf(os.Stderr, "instance %s is already suspended\n", name)
		return nil
	}

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

	if deps.Docker != nil {
		for svcName, cid := range rec.ContainerIDs {
			fmt.Fprintf(os.Stderr, "stopping %s...\n", svcName)
			if err := deps.Docker.StopService(ctx, cid); err != nil {
				fmt.Fprintf(os.Stderr, "warning: stop %s: %v\n", svcName, err)
			}
		}
	} else if len(rec.ContainerIDs) > 0 {
		fmt.Fprintf(os.Stderr, "warning: docker unavailable — skipping container stops for %d service(s)\n", len(rec.ContainerIDs))
	}

	rec.State = "suspended"
	rec.PIDs = nil
	rec.PIDStarts = nil
	deps.Registry.Instances[name] = rec

	if err := deps.Registry.Save(); err != nil {
		return fmt.Errorf("registry: write: %w", err)
	}

	fmt.Fprintf(os.Stderr, "instance %s suspended\n", name)
	return nil
}
