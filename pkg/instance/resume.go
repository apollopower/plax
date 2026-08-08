package instance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/process"
)

func Resume(ctx context.Context, deps *Deps, name string) error {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return fmt.Errorf("instance %q not found", name)
	}

	if rec.State != "suspended" {
		if rec.State == "running" {
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
		for svcName, cid := range rec.ContainerIDs {
			fmt.Fprintf(os.Stderr, "starting %s...\n", svcName)
			alreadyRunning, err := deps.Docker.StartService(ctx, cid)
			if err != nil {
				if strings.Contains(err.Error(), "No such container") {
					return fmt.Errorf("container for %q no longer exists — run 'plax down %s' then 'plax up %s' to rebuild: %w", svcName, name, name, err)
				}
				return fmt.Errorf("starting %s: %w", svcName, err)
			}
			if !alreadyRunning {
				startedContainers[svcName] = cid
			}
		}
	}

	values := map[string]string{"DB_NAME": rec.DBName}
	for varName, port := range rec.Ports {
		values[varName] = strconv.Itoa(port)
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

		logDir := filepath.Join(deps.RepoRoot, ".plax", "logs", name)
		cleanups = append(cleanups, func() {
			for procName, pgid := range pids {
				if err := process.Terminate(pgid, pidStarts[procName], 5*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "rollback: terminate %s: %v\n", procName, err)
				}
			}
		})
		for _, proc := range deps.Blueprint.Processes {
			fmt.Fprintf(os.Stderr, "starting %s...\n", proc.Name)
			procEnv := buildProcessEnv(derivedEnv, rec.Ports, proc)

			renderedCmd, err := env.Render(proc.Command, values)
			if err != nil {
				return fmt.Errorf("process %q: %w", proc.Name, err)
			}

			procDir := filepath.Join(rec.WorktreePath, proc.Workdir)
			logPath := filepath.Join(logDir, proc.Name+".log")

			pgid, startTime, err := process.Spawn(proc.Name, renderedCmd, procEnv, procDir, logPath)
			if err != nil {
				return err
			}
			pids[proc.Name] = pgid
			pidStarts[proc.Name] = startTime
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

	rec.State = "running"
	rec.PIDs = pids
	rec.PIDStarts = pidStarts
	deps.Registry.Instances[name] = rec

	if err := deps.Registry.Save(); err != nil {
		return fmt.Errorf("registry: write: %w", err)
	}

	success = true
	return nil
}
