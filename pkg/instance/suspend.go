package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
)

func Suspend(ctx context.Context, deps *Deps, name string) error {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return fmt.Errorf("instance %q not found", name)
	}

	if rec.State == registry.StateSuspended {
		fmt.Fprintf(os.Stderr, "instance %s is already suspended\n", name)
		return nil
	}

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
				fmt.Fprintf(os.Stderr, "warning: %s process leader died but children survived (pgid %d) — suspend continuing\n", r.name, rec.PIDs[r.name])
			case r.err != nil:
				fmt.Fprintf(os.Stderr, "warning: terminate %s (pgid %d): %v\n", r.name, rec.PIDs[r.name], r.err)
			}
		}
	}

	if deps.Docker != nil {
		type svcResult struct {
			name string
			err  error
		}
		ch := make(chan svcResult, len(rec.ContainerIDs))
		for svcName, cid := range rec.ContainerIDs {
			go func(name, cid string) {
				err := deps.Docker.StopService(ctx, cid)
				ch <- svcResult{name, err}
			}(svcName, cid)
		}
		for range rec.ContainerIDs {
			r := <-ch
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "warning: stop %s: %v\n", r.name, r.err)
			}
		}
	} else if len(rec.ContainerIDs) > 0 {
		fmt.Fprintf(os.Stderr, "warning: docker unavailable — skipping container stops for %d service(s)\n", len(rec.ContainerIDs))
	}

	rec.State = registry.StateSuspended
	rec.PIDs = nil
	rec.PIDStarts = nil
	deps.Registry.Instances[name] = rec

	if err := deps.Registry.Save(); err != nil {
		return fmt.Errorf("registry: write: %w", err)
	}

	fmt.Fprintf(os.Stderr, "instance %s suspended\n", name)
	return nil
}
