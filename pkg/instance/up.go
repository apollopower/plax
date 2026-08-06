package instance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/worktree"
)

// Up creates a full instance: branch, worktree, network, ports, .env,
// database, containers, processes, registry entry. Rolls back all side
// effects on failure.
func Up(ctx context.Context, deps *Deps, name string) (err error) {
	if err := validateName(name); err != nil {
		return err
	}

	// Check instance does not already exist.
	if _, found := deps.Registry.GetInstance(name); found {
		return fmt.Errorf("instance %q already exists", name)
	}
	if worktree.BranchExists(deps.RepoRoot, name) {
		return fmt.Errorf("branch %q exists but instance is not registered — run 'git branch -D %s' to clean up",
			worktree.BranchName(name), worktree.BranchName(name))
	}

	// Check base database.
	baseInfo, err := deps.BM.BaseStatus(ctx)
	if err != nil {
		return fmt.Errorf("checking base: %w", err)
	}
	if !baseInfo.Exists {
		return fmt.Errorf("base database does not exist — run 'plax base reset' first")
	}
	if !baseInfo.Locked {
		return fmt.Errorf("base database is not locked — run 'plax base reset' to repair")
	}

	// Rollback: each step that produces a side effect appends a cleanup
	// function. On failure, cleanups run in reverse order.
	var cleanups []func()
	success := false
	defer func() {
		if !success {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		}
	}()

	// Step 1: Create branch and worktree.
	fmt.Fprintf(os.Stderr, "creating branch and worktree...\n")
	worktreePath, err := worktree.Create(deps.RepoRoot, name)
	if err != nil {
		return err
	}
	cleanups = append(cleanups, func() {
		if err := worktree.Remove(deps.RepoRoot, name); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: remove worktree: %v\n", err)
		}
	})

	// Step 2: Create Docker network.
	netName := "plax-" + name + "-net"
	fmt.Fprintf(os.Stderr, "creating network %s...\n", netName)
	if err := deps.Docker.CreateNetwork(ctx, netName); err != nil {
		return err
	}
	cleanups = append(cleanups, func() {
		if err := deps.Docker.RemoveNetwork(ctx, netName); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: remove network: %v\n", err)
		}
	})

	// Step 3: Allocate ports.
	fmt.Fprintf(os.Stderr, "allocating ports...\n")
	allocated, err := allocatePorts(deps)
	if err != nil {
		return err
	}
	cleanups = append(cleanups, func() {
		for _, port := range allocated {
			deps.Pool.Release(port)
		}
	})

	// Build the values map for .env derivation and command templating.
	values := map[string]string{"DB_NAME": "plax_" + name}
	for varName, port := range allocated {
		values[varName] = strconv.Itoa(port)
	}

	// Step 4: Derive .env.
	// The user's own .env provides real secrets; the template provides
	// structure and defaults; holes get per-instance values.
	fmt.Fprintf(os.Stderr, "deriving .env...\n")
	templatePath := filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)
	overridesPath := filepath.Join(deps.RepoRoot, ".env")
	envPath := filepath.Join(worktreePath, ".env")
	if err := env.Derive(templatePath, overridesPath, deps.Blueprint.Env.Holes, values, envPath); err != nil {
		return err
	}
	// No separate cleanup — removing the worktree removes .env.

	// Step 5: Clone database.
	dbName := "plax_" + name
	fmt.Fprintf(os.Stderr, "cloning database %s...\n", dbName)
	if err := deps.BM.CloneBase(ctx, dbName); err != nil {
		return fmt.Errorf("cloning database: %w", err)
	}
	cleanups = append(cleanups, func() {
		if err := deps.BM.DropInstanceDB(ctx, dbName); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: drop database: %v\n", err)
		}
	})

	// Step 6: Compute provenance and blueprint stamp.
	toolchainHash := hashFile(filepath.Join(deps.RepoRoot, deps.Blueprint.Toolchain))
	deps.Registry.BlueprintStamp = computeBlueprintStamp(deps.RepoRoot, deps.Blueprint)

	// Step 7: Start dedicated containers.
	containerIDs := map[string]string{}
	for svcName, svc := range deps.Blueprint.Services {
		if svc.Isolation != blueprint.IsolationDedicated {
			continue
		}
		fmt.Fprintf(os.Stderr, "starting %s...\n", svcName)

		portMap := map[string]int{}
		for containerPort, portDef := range svc.Ports {
			portMap[containerPort] = allocated[portDef.Var]
		}

		svcEnv := map[string]string{}
		for k, v := range svc.Env {
			svcEnv[k] = v
		}
		for _, portDef := range svc.Ports {
			svcEnv[portDef.Var] = strconv.Itoa(allocated[portDef.Var])
		}

		cfg := docker.ServiceConfig{
			InstanceName: name,
			ServiceName:  svcName,
			Image:        svc.Image,
			Command:      svc.Command,
			Env:          svcEnv,
			PortMap:      portMap,
			NetworkName:  netName,
		}
		cid, err := deps.Docker.RunService(ctx, cfg)
		if err != nil {
			// Stop and remove any containers started so far.
			for _, id := range containerIDs {
				_ = deps.Docker.StopService(ctx, id)
				_ = deps.Docker.RemoveService(ctx, id)
			}
			return fmt.Errorf("starting %s: %w", svcName, err)
		}
		containerIDs[svcName] = cid
	}
	if len(containerIDs) > 0 {
		cleanups = append(cleanups, func() {
			for svcName, cid := range containerIDs {
				if err := deps.Docker.StopService(ctx, cid); err != nil {
					fmt.Fprintf(os.Stderr, "rollback: stop %s: %v\n", svcName, err)
				}
				if err := deps.Docker.RemoveService(ctx, cid); err != nil {
					fmt.Fprintf(os.Stderr, "rollback: remove %s: %v\n", svcName, err)
				}
			}
		})
	}

	// Step 8: Start native processes.
	pids := map[string]int{}
	if len(deps.Blueprint.Processes) > 0 {
		logDir := filepath.Join(deps.RepoRoot, ".plax", "logs", name)

		derivedEnv, err := env.ParseFile(envPath)
		if err != nil {
			return fmt.Errorf("parsing derived .env: %w", err)
		}

		for _, proc := range deps.Blueprint.Processes {
			fmt.Fprintf(os.Stderr, "starting %s...\n", proc.Name)

			procEnv := buildProcessEnv(derivedEnv, allocated, proc)

			renderedCmd, err := env.Render(proc.Command, values)
			if err != nil {
				for _, pgid := range pids {
					_ = process.Terminate(pgid, 5*time.Second)
				}
				return fmt.Errorf("process %q: %w", proc.Name, err)
			}

			procDir := filepath.Join(worktreePath, proc.Workdir)
			logPath := filepath.Join(logDir, proc.Name+".log")

			pgid, err := process.Spawn(proc.Name, renderedCmd, procEnv, procDir, logPath)
			if err != nil {
				for _, pg := range pids {
					_ = process.Terminate(pg, 5*time.Second)
				}
				return err
			}
			pids[proc.Name] = pgid
		}
	}
	if len(pids) > 0 {
		cleanups = append(cleanups, func() {
			for procName, pgid := range pids {
				if err := process.Terminate(pgid, 5*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "rollback: terminate %s: %v\n", procName, err)
				}
			}
		})
	}

	// Step 9: Write registry.
	if err := deps.Registry.AddInstance(name, registry.InstanceRecord{
		Branch:       worktree.BranchName(name),
		WorktreePath: worktreePath,
		CreatedAt:    time.Now(),
		State:        "running",
		Ports:        allocated,
		DBName:       dbName,
		ContainerIDs: containerIDs,
		PIDs:         pids,
		Provenance: registry.Provenance{
			BaseVersion: baseInfo.ProvenanceVer,
			Toolchain:   toolchainHash,
		},
	}); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if err := deps.Registry.Save(); err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	// Print summary.
	fmt.Fprintf(os.Stderr, "\ninstance %s up\n", name)
	fmt.Fprintf(os.Stderr, "  worktree:  %s\n", worktree.WorktreeRelPath(name))
	fmt.Fprintf(os.Stderr, "  branch:    %s\n", worktree.BranchName(name))
	fmt.Fprintf(os.Stderr, "  database:  %s\n", dbName)
	if len(allocated) > 0 {
		fmt.Fprintf(os.Stderr, "  ports:")
		// Sorted for deterministic output.
		for _, varName := range sortedKeys(allocated) {
			fmt.Fprintf(os.Stderr, " %s=%d", varName, allocated[varName])
		}
		fmt.Fprintln(os.Stderr)
	}
	if len(pids) > 0 {
		fmt.Fprintf(os.Stderr, "  logs:      .plax/logs/%s/\n", name)
	}

	success = true
	return nil
}

// allocatePorts allocates one port per port-bearing entity in the blueprint.
// Returns a map of port var name → host port number.
func allocatePorts(deps *Deps) (map[string]int, error) {
	allocated := map[string]int{}

	for svcName, svc := range deps.Blueprint.Services {
		if svc.Isolation != blueprint.IsolationDedicated {
			continue
		}
		for _, portDef := range svc.Ports {
			port, err := deps.Pool.Allocate(deps.RepoRoot, svcName)
			if err != nil {
				// Release already-allocated ports.
				for _, p := range allocated {
					deps.Pool.Release(p)
				}
				return nil, err
			}
			allocated[portDef.Var] = port
		}
	}

	for _, proc := range deps.Blueprint.Processes {
		if proc.PortVar == "" {
			continue
		}
		port, err := deps.Pool.Allocate(deps.RepoRoot, proc.Name)
		if err != nil {
			for _, p := range allocated {
				deps.Pool.Release(p)
			}
			return nil, err
		}
		allocated[proc.PortVar] = port
	}

	return allocated, nil
}

// buildProcessEnv constructs the environment for a native process:
// host env + derived .env vars + explicit port var.
func buildProcessEnv(derivedEnv map[string]string, allocated map[string]int, proc blueprint.ProcessDef) []string {
	envMap := map[string]string{}
	for _, e := range os.Environ() {
		k, v, _ := splitEnv(e)
		envMap[k] = v
	}
	for k, v := range derivedEnv {
		envMap[k] = v
	}
	if proc.PortVar != "" {
		if port, ok := allocated[proc.PortVar]; ok {
			envMap[proc.PortVar] = strconv.Itoa(port)
		}
	}

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result
}

func splitEnv(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// sortedKeys returns the keys of a map sorted alphabetically.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort for small maps.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
