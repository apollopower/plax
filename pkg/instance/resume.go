package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/verify"

	"github.com/docker/docker/errdefs"
)

// Resume restarts a suspended instance: starts stopped containers, probes
// port availability, re-derives .env, and spawns processes.
func Resume(ctx context.Context, deps *Deps, name string) error {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return fmt.Errorf("instance %q not found", name)
	}

	if rec.State != registry.StateSuspended {
		if rec.State == registry.StateRunning {
			return fmt.Errorf("instance %q is already running", name)
		}
		return fmt.Errorf("instance %q is in state %q — expected suspended", name, rec.State)
	}

	for varName, port := range rec.Ports {
		if !portpool.ProbeFree(port) {
			pid, cmdline, ok := portpool.PortOwner(port)
			if ok {
				return fmt.Errorf("port %d (%s) is in use by pid %d (%s) — free the port and retry, or run 'plax down %s' and 'plax up %s' to reallocate",
					port, varName, pid, cmdline, name, name)
			}
			return fmt.Errorf("port %d (%s) is in use (owner unknown) — free the port and retry, or run 'plax down %s' and 'plax up %s' to reallocate",
				port, varName, name, name)
		}
	}

	if len(rec.ContainerIDs) > 0 && deps.Docker == nil {
		return fmt.Errorf("docker unavailable — cannot start %d container(s); fix Docker and retry", len(rec.ContainerIDs))
	}

	cleanupCtx := context.WithoutCancel(ctx)
	var cleanups []func()
	success := false
	defer func() {
		if !success {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		}
	}()

	startedContainers := map[string]string{}
	if deps.Docker != nil {
		cleanups = append(cleanups, func() {
			for svcName, cid := range startedContainers {
				if err := deps.Docker.StopService(cleanupCtx, cid); err != nil {
					fmt.Fprintf(os.Stderr, "rollback: stop %s: %v\n", svcName, err)
				}
			}
		})
		type containerResult struct {
			name           string
			cid            string
			alreadyRunning bool
			err            error
		}
		ch := make(chan containerResult, len(rec.ContainerIDs))
		for svcName, cid := range rec.ContainerIDs {
			go func(name, cid string) {
				fmt.Fprintf(os.Stderr, "starting %s...\n", name)
				alreadyRunning, err := deps.Docker.StartService(ctx, cid)
				ch <- containerResult{name, cid, alreadyRunning, err}
			}(svcName, cid)
		}
		var firstErr error
		for range rec.ContainerIDs {
			r := <-ch
			if r.err != nil {
				if firstErr == nil {
					if errdefs.IsNotFound(r.err) {
						firstErr = fmt.Errorf("container for %q no longer exists — run 'plax down %s' then 'plax up %s' to rebuild: %w", r.name, name, name, r.err)
					} else {
						firstErr = fmt.Errorf("starting %s: %w", r.name, r.err)
					}
				}
				continue
			}
			if !r.alreadyRunning {
				startedContainers[r.name] = r.cid
			}
		}
		if firstErr != nil {
			return firstErr
		}
	}

	values := map[string]string{}
	for varName, port := range rec.Ports {
		values[varName] = strconv.Itoa(port)
	}
	for key, physicalName := range rec.DBNames {
		if key == "" {
			values["DB_NAME"] = physicalName
		} else {
			values["DB_NAME_"+key] = physicalName
		}
	}

	pids := map[string]int{}
	pidStarts := map[string]int64{}
	if len(deps.Blueprint.Processes) > 0 {
		envPath := filepath.Join(rec.WorktreePath, ".env")
		if deps.Blueprint.Env.Template != "" {
			if _, err := os.Stat(envPath); os.IsNotExist(err) {
				return fmt.Errorf("env: .env not found at %s — run 'plax rederive' to regenerate", envPath)
			}
		}
		derivedEnv, err := env.ParseFile(envPath)
		if err != nil && deps.Blueprint.Env.Template != "" {
			return fmt.Errorf("env: parse .env: %w", err)
		}

		// Env checks after .env parse — failure leaves instance suspended.
		if deps.Blueprint.Env.Template != "" {
			templatePath := filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)
			userEnvPath := filepath.Join(deps.RepoRoot, ".env")
			scrub := verify.BuildScrubSet(deps.Blueprint)
			if results := verify.CheckEnv(templatePath, userEnvPath, envPath, deps.Blueprint.Env.Holes, scrub); anyFailed(results) {
				rec.Health = registry.HealthUnhealthy
				now := time.Now()
				rec.VerifiedAt = &now
				if err := deps.Registry.UpdateInstance(name, rec); err != nil {
					fmt.Fprintf(os.Stderr, "warning: updating health: %v\n", err)
				}
				if err := deps.Registry.Save(); err != nil {
					fmt.Fprintf(os.Stderr, "warning: saving health: %v\n", err)
				}
				printVerificationErrors(results)
				return &verify.VerificationError{Results: results, Layer: 1}
			}
		}

		logDir := filepath.Join(deps.RepoRoot, ".plax", "logs", name)
		cleanups = append(cleanups, func() {
			for procName, pgid := range pids {
				if err := process.Terminate(pgid, pidStarts[procName], 5*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "rollback: terminate %s: %v\n", procName, err)
				}
			}
		})
		{
			type procResult struct {
				name      string
				pgid      int
				startTime int64
				err       error
			}
			ch := make(chan procResult, len(deps.Blueprint.Processes))
			for _, proc := range deps.Blueprint.Processes {
				go func(proc blueprint.ProcessDef) {
					fmt.Fprintf(os.Stderr, "starting %s...\n", proc.Name)
					procEnv := buildProcessEnv(derivedEnv, rec.Ports, proc)
					renderedCmd, err := env.Render(proc.Command, values)
					if err != nil {
						ch <- procResult{name: proc.Name, err: fmt.Errorf("process %q: %w", proc.Name, err)}
						return
					}
					procDir := filepath.Join(rec.WorktreePath, proc.Workdir)
					logPath := filepath.Join(logDir, proc.Name+".log")
					pgid, startTime, err := process.Spawn(proc.Name, renderedCmd, procEnv, procDir, logPath)
					ch <- procResult{proc.Name, pgid, startTime, err}
				}(proc)
			}
			var firstErr error
			for range deps.Blueprint.Processes {
				r := <-ch
				if r.err != nil {
					if firstErr == nil {
						firstErr = r.err
					}
					continue
				}
				pids[r.name] = r.pgid
				pidStarts[r.name] = r.startTime
			}
			if firstErr != nil {
				return firstErr
			}
		}
	}

	if len(startedContainers) > 0 || len(pids) > 0 {
		time.Sleep(settleDelay)
		for svcName := range startedContainers {
			cid := rec.ContainerIDs[svcName]
			running, err := deps.Docker.ServiceRunning(ctx, cid)
			if err != nil {
				return fmt.Errorf("checking %s: %w", svcName, err)
			}
			if !running {
				return fmt.Errorf("service %s exited immediately after start", svcName)
			}
		}
		for procName, pgid := range pids {
			if !process.IsAlive(pgid) {
				return fmt.Errorf("process %s exited immediately after start — see .plax/logs/%s/%s.log", procName, name, procName)
			}
		}
	}

	rec.State = registry.StateRunning
	rec.PIDs = pids
	rec.PIDStarts = pidStarts
	if err := deps.Registry.UpdateInstance(name, rec); err != nil {
		return err
	}

	if err := deps.Registry.Save(); err != nil {
		return fmt.Errorf("registry: write: %w", err)
	}

	// Cleanup: if anything after this fails (non-VerificationError), revert
	// the registry state back to suspended so we don't leave a zombie.
	cleanups = append(cleanups, func() {
		rec.State = registry.StateSuspended
		_ = deps.Registry.UpdateInstance(name, rec)
		_ = deps.Registry.Save()
	})

	// Runtime verification after settle — failure keeps workloads running.
	results, verr := verify.RunVerify(ctx, &verify.Deps{
		Blueprint: deps.Blueprint,
		Registry:  deps.Registry,
		BM:        deps.BM,
		RepoRoot:  deps.RepoRoot,
	}, name)
	var vErr *verify.VerificationError
	if errors.As(verr, &vErr) {
		printVerificationErrors(vErr.Results)
		fmt.Fprintf(os.Stderr, "instance %s is up but unhealthy — investigate, "+
			"then 'plax verify %s' to re-check or 'plax down %s' to tear down\n",
			name, name, name)
		success = true
		return vErr
	}
	if verr != nil {
		return verr
	}
	printVerificationSuccess(results)

	success = true
	return nil
}
