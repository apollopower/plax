package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/mailbox"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/toolchain"
	"github.com/apollopower/plax/pkg/worktree"
)

// settleDelay is how long Up waits after starting workloads before checking
// they are still alive. It catches the common failure — a bad command or
// image exiting immediately — without pretending to be a readiness check.
const settleDelay = 300 * time.Millisecond

// Up creates a full instance: branch, worktree, network, ports, .env,
// database, containers, processes, registry entry. Rolls back all side
// effects on failure.
func Up(ctx context.Context, deps *Deps, name string) (err error) {
	if err := validateName(name); err != nil {
		return err
	}

	// Structural blueprint errors (duplicate processes, port-var collisions,
	// unsafe names) must fail before any side effect. Hole warnings are not
	// fatal: derivation appends holes missing from the template.
	if errs := blueprint.ValidateStructural(deps.Blueprint); len(errs) > 0 {
		return fmt.Errorf("invalid blueprint: %w", errors.Join(errs...))
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
	//
	// Cleanups run with a non-canceled context: when ctx cancellation (e.g.
	// Ctrl-C) is what caused the failure, Docker and Postgres cleanup calls
	// must still be able to complete.
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

	// Step 1: Create branch and worktree.
	if deps.SourceRef != "" {
		fmt.Fprintf(os.Stderr, "creating branch and worktree from %s...\n", deps.SourceRef)
	} else {
		fmt.Fprintf(os.Stderr, "creating branch and worktree...\n")
	}
	worktreePath, err := worktree.Create(deps.RepoRoot, name, deps.ResolvedRef)
	if err != nil {
		return err
	}
	cleanups = append(cleanups, func() {
		if err := worktree.Remove(deps.RepoRoot, name); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: remove worktree: %v\n", err)
		}
	})

	// Step 2: Create mailbox directory.
	if err := mailbox.CreateDir(deps.RepoRoot, name); err != nil {
		return fmt.Errorf("mailbox: creating directory: %w", err)
	}
	cleanups = append(cleanups, func() {
		if err := mailbox.RemoveDir(deps.RepoRoot, name); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: remove mailbox: %v\n", err)
		}
	})

	// Step 3: Create Docker network.
	netName := "plax-" + name + "-net"
	fmt.Fprintf(os.Stderr, "creating network %s...\n", netName)
	if err := deps.Docker.CreateNetwork(ctx, netName); err != nil {
		return err
	}
	cleanups = append(cleanups, func() {
		if err := deps.Docker.RemoveNetwork(cleanupCtx, netName); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: remove network: %v\n", err)
		}
	})

	// Step 3: Allocate ports.
	fmt.Fprintf(os.Stderr, "allocating ports...\n")
	allocated, err := allocatePorts(deps, name)
	if err != nil {
		return err
	}
	cleanups = append(cleanups, func() {
		for _, port := range allocated {
			deps.Pool.Release(port)
		}
	})

	// Build the values map for .env derivation and command templating.
	// Database names are constructed from the blueprint's Databases slice
	// (or a single default DB if none declared).
	dbNames := buildDBNames(deps.Blueprint, name)
	values := map[string]string{}
	for varName, port := range allocated {
		values[varName] = strconv.Itoa(port)
	}
	for key, physicalName := range dbNames {
		if key == "" {
			values["DB_NAME"] = physicalName
		} else {
			values["DB_NAME_"+key] = physicalName
		}
	}

	// Step 4: Derive .env.
	// The user's own .env provides real secrets; the template provides
	// structure and defaults; holes get per-instance values.
	// Skipped for blueprints without an env template — there is nothing
	// to derive.
	envPath := filepath.Join(worktreePath, ".env")
	if deps.Blueprint.Env.Template != "" {
		fmt.Fprintf(os.Stderr, "deriving .env...\n")
		templatePath := filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)
		overridesPath := filepath.Join(deps.RepoRoot, ".env")
		if err := env.Derive(templatePath, overridesPath, deps.Blueprint.Env.Holes, values, envPath); err != nil {
			return err
		}
		// No separate cleanup — removing the worktree removes .env.
	}

	// Step 5: Clone all databases (primary + any declared databases).
	clonedDBs := []string{}
	for _, physicalName := range dbNames {
		fmt.Fprintf(os.Stderr, "cloning database %s...\n", physicalName)
		if err := deps.BM.CloneBase(ctx, physicalName); err != nil {
			return fmt.Errorf("cloning database: %w", err)
		}
		clonedDBs = append(clonedDBs, physicalName)
	}
	cleanups = append(cleanups, func() {
		for _, db := range clonedDBs {
			if err := deps.BM.DropInstanceDB(cleanupCtx, db); err != nil {
				fmt.Fprintf(os.Stderr, "rollback: drop database %s: %v\n", db, err)
			}
		}
	})

	// Step 6: Compute provenance and blueprint stamp.
	toolchainHash := hashFile(filepath.Join(deps.RepoRoot, deps.Blueprint.Toolchain))
	var toolVersions map[string]string
	if deps.Blueprint.Toolchain != "" {
		pins, err := toolchain.ParsePins(filepath.Join(deps.RepoRoot, deps.Blueprint.Toolchain))
		if err == nil && pins != nil {
			toolVersions = toolchain.ResolveVersions(pins)
		}
	}
	baseRef, baseCommit, err := worktree.HeadRef(deps.RepoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: recording head ref: %v\n", err)
		baseRef, baseCommit = "", ""
	}

	// Step 7: Start dedicated containers.
	// The cleanup is registered before the loop: closures capture the map,
	// so the deferred rollback sees every container started before a failure.
	containerIDs := map[string]string{}
	cleanups = append(cleanups, func() {
		for svcName, cid := range containerIDs {
			if err := deps.Docker.StopService(cleanupCtx, cid); err != nil {
				fmt.Fprintf(os.Stderr, "rollback: stop %s: %v\n", svcName, err)
			}
			if err := deps.Docker.RemoveService(cleanupCtx, cid); err != nil {
				fmt.Fprintf(os.Stderr, "rollback: remove %s: %v\n", svcName, err)
			}
		}
	})
	for svcName, svc := range deps.Blueprint.Services {
		if svc.Isolation != blueprint.IsolationDedicated {
			if svc.Isolation != blueprint.IsolationLogical {
				fmt.Fprintf(os.Stderr, "skipping %s (isolation %q not implemented)\n", svcName, svc.Isolation)
			}
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
			return fmt.Errorf("starting %s: %w", svcName, err)
		}
		containerIDs[svcName] = cid
	}

	// Step 8: Start native processes.
	pids := map[string]int{}
	pidStarts := map[string]int64{}
	cleanups = append(cleanups, func() {
		for procName, pgid := range pids {
			if err := process.Terminate(pgid, pidStarts[procName], 5*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "rollback: terminate %s: %v\n", procName, err)
			}
		}
	})
	if len(deps.Blueprint.Processes) > 0 {
		logDir := filepath.Join(deps.RepoRoot, ".plax", "logs", name)

		derivedEnv := map[string]string{}
		if deps.Blueprint.Env.Template != "" {
			var err error
			derivedEnv, err = env.ParseFile(envPath)
			if err != nil {
				return fmt.Errorf("parsing derived .env: %w", err)
			}
		}

		for _, proc := range deps.Blueprint.Processes {
			fmt.Fprintf(os.Stderr, "starting %s...\n", proc.Name)

			procEnv := buildProcessEnv(derivedEnv, allocated, proc)

			renderedCmd, err := env.Render(proc.Command, values)
			if err != nil {
				return fmt.Errorf("process %q: %w", proc.Name, err)
			}

			procDir := filepath.Join(worktreePath, proc.Workdir)
			logPath := filepath.Join(logDir, proc.Name+".log")

			pgid, startTime, err := process.Spawn(proc.Name, renderedCmd, procEnv, procDir, logPath)
			if err != nil {
				return err
			}
			pids[proc.Name] = pgid
			pidStarts[proc.Name] = startTime
		}
	}

	// Step 9: Verify the workloads stayed up. Spawn and ContainerStart only
	// prove the workload started; a typo'd command exits immediately and must
	// fail the whole up rather than record a "running" instance.
	if len(containerIDs) > 0 || len(pids) > 0 {
		time.Sleep(settleDelay)
		for svcName, cid := range containerIDs {
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

	// Step 10: Write registry.
	if err := deps.Registry.AddInstance(name, registry.InstanceRecord{
		Branch:       worktree.BranchName(name),
		WorktreePath: worktreePath,
		CreatedAt:    time.Now(),
		State:        registry.StateRunning,
		Ports:        allocated,
		DBName:       dbNames[""],
		DBNames:      dbNames,
		ContainerIDs: containerIDs,
		PIDs:         pids,
		PIDStarts:    pidStarts,
		BaseRef:      baseRef,
		BaseCommit:   baseCommit,
		SourceRef:    deps.SourceRef,
		Provenance: registry.Provenance{
			BaseVersion:  baseInfo.ProvenanceVer,
			Toolchain:    toolchainHash,
			ToolVersions: toolVersions,
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
	if len(dbNames) > 0 {
		dbList := sortedDBNames(dbNames)
		label := "  databases:"
		if len(dbList) == 1 {
			label = "  database:"
		}
		fmt.Fprintf(os.Stderr, "%s %s\n", label, strings.Join(dbList, ", "))
	}
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
func allocatePorts(deps *Deps, instanceName string) (map[string]int, error) {
	allocated := map[string]int{}

	for svcName, svc := range deps.Blueprint.Services {
		if svc.Isolation != blueprint.IsolationDedicated {
			continue
		}
		for _, portDef := range svc.Ports {
			port, err := deps.Pool.Allocate(instanceName, svcName)
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
		port, err := deps.Pool.Allocate(instanceName, proc.Name)
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

func sortedDBNames(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make([]string, len(keys))
	for i, k := range keys {
		result[i] = m[k]
	}
	return result
}

// buildDBNames constructs the physical database name map from a blueprint.
// If no databases are declared, a single default entry with key "" is returned.
func buildDBNames(bp *blueprint.Blueprint, instanceName string) map[string]string {
	dbNames := map[string]string{}
	for _, svc := range bp.Services {
		if svc.Isolation != blueprint.IsolationLogical {
			continue
		}
		if len(svc.Databases) == 0 {
			if _, ok := dbNames[""]; !ok {
				dbNames[""] = "plax_" + instanceName
			}
		} else {
			for _, dbDef := range svc.Databases {
				key := dbDef.Name
				physicalName := "plax_" + instanceName
				if key != "" {
					physicalName += "_" + key
				}
				dbNames[key] = physicalName
			}
		}
	}
	if _, ok := dbNames[""]; !ok {
		dbNames[""] = "plax_" + instanceName
	}
	return dbNames
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
