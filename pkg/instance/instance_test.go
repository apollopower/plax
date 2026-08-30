package instance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/worktree"
)

func hashStamp(repoRoot string, bp *blueprint.Blueprint) registry.BlueprintStamp {
	hashFile := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		h := sha256.Sum256(data)
		return fmt.Sprintf("%x", h[:])
	}
	return registry.BlueprintStamp{
		ComposeHash:    hashFile(filepath.Join(repoRoot, "docker-compose.yml")),
		EnvExampleHash: hashFile(filepath.Join(repoRoot, bp.Env.Template)),
		ToolchainHash:  hashFile(filepath.Join(repoRoot, bp.Toolchain)),
	}
}

// --- fakes ---

type fakeBM struct {
	info      postgres.BaseInfo
	statusErr error
	cloneFunc func(ctx context.Context, targetDB string) error
	dropErr   error

	mu      sync.Mutex
	cloned  []string
	dropped []string
}

func (f *fakeBM) BaseStatus(context.Context) (postgres.BaseInfo, error) {
	if f.statusErr != nil {
		return postgres.BaseInfo{}, f.statusErr
	}
	return f.info, nil
}

func (f *fakeBM) CloneBase(ctx context.Context, targetDB string) error {
	if f.cloneFunc != nil {
		return f.cloneFunc(ctx, targetDB)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cloned = append(f.cloned, targetDB)
	return nil
}

func (f *fakeBM) DropInstanceDB(_ context.Context, dbName string) error {
	if f.dropErr != nil {
		return f.dropErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, dbName)
	return nil
}

func (f *fakeBM) clonedDBs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cloned...)
}

func (f *fakeBM) InstanceProvenance(_ context.Context, dbName string) (*postgres.ProvenanceRow, error) {
	return &postgres.ProvenanceRow{Version: 1, Source: dbName, SchemaHash: "abc"}, nil
}

func (f *fakeBM) InstanceDBExists(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (f *fakeBM) AppliedMigrations(context.Context, string) ([]string, error) {
	return nil, nil
}

func (f *fakeBM) droppedDBs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dropped...)
}

type fakeDocker struct {
	runErr            error
	stopErr           error
	running           bool
	runningErr        error
	createNetErr      error
	runErrFor         map[string]error
	skipBindListeners bool // when true, RunService does not bind TCP listeners

	mu               sync.Mutex
	createdNets      []string
	removedNets      []string
	started          []string
	stopped          []string
	removed          []string
	removeNetCtxErrs []error // ctx.Err() observed at each RemoveNetwork call
	runCfgs          []docker.ServiceConfig
	listeners        []net.Listener // real TCP listeners for verify checks
}

func (f *fakeDocker) listenOnPorts(portMap map[string]int) {
	for _, hostPort := range portMap {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
		if err == nil {
			f.listeners = append(f.listeners, l)
		}
	}
}

func (f *fakeDocker) closeListeners() {
	for _, l := range f.listeners {
		_ = l.Close()
	}
	f.listeners = nil
}

func (f *fakeDocker) CreateNetwork(_ context.Context, name string) error {
	if f.createNetErr != nil {
		return f.createNetErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdNets = append(f.createdNets, name)
	return nil
}

func (f *fakeDocker) RemoveNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedNets = append(f.removedNets, name)
	f.removeNetCtxErrs = append(f.removeNetCtxErrs, ctx.Err())
	return nil
}

func (f *fakeDocker) RunService(_ context.Context, cfg docker.ServiceConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return "", f.runErr
	}
	if f.runErrFor != nil {
		if err, ok := f.runErrFor[cfg.ServiceName]; ok {
			return "", err
		}
	}
	id := "cid-" + cfg.ServiceName
	f.started = append(f.started, cfg.ServiceName)
	f.runCfgs = append(f.runCfgs, cfg)
	if !f.skipBindListeners {
		f.listenOnPorts(cfg.PortMap)
	}
	return id, nil
}

func (f *fakeDocker) StopService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	f.closeListeners()
	if f.stopErr != nil {
		return f.stopErr
	}
	return nil
}

func (f *fakeDocker) RemoveService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeDocker) ServiceRunning(context.Context, string) (bool, error) {
	return f.running, f.runningErr
}

func (f *fakeDocker) StartService(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	// Re-bind listeners for verify checks after resume.
	for _, cfg := range f.runCfgs {
		if "cid-"+cfg.ServiceName == id {
			f.listenOnPorts(cfg.PortMap)
			break
		}
	}
	return false, nil
}

func (f *fakeDocker) ServiceExists(_ context.Context, id string) (bool, error) {
	return true, nil
}

// --- helpers ---

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}

	files := map[string]string{
		// .env is ignored so the derived .env does not dirty git status.
		".gitignore":         ".env\n",
		".env.example":       "PORT=3000\n",
		".tool-versions":     "golang 1.26\n",
		"docker-compose.yml": "services: {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	add := exec.Command("git", "add", ".")
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s", out)
	}
	commit := exec.Command("git", "commit", "-m", "init")
	commit.Dir = dir
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s", out)
	}
	return dir
}

func testBlueprint() *blueprint.Blueprint {
	return &blueprint.Blueprint{
		Version:   1,
		Name:      "test",
		PortPool:  blueprint.PortPool{Start: 25000, End: 25100},
		Toolchain: ".tool-versions",
		Seed:      blueprint.SeedConfig{Migrate: "true", Command: "c", Workdir: "."},
		Services: map[string]blueprint.ServiceDef{
			"db":    {Isolation: blueprint.IsolationLogical, Type: "postgres"},
			"redis": {Isolation: blueprint.IsolationDedicated, Image: "redis:7", Ports: map[string]blueprint.PortDef{"6379": {Var: "REDIS_PORT"}}},
		},
		Processes: []blueprint.ProcessDef{
			{Name: "app", Isolation: blueprint.IsolationNative, Command: "sleep 60", Workdir: ".", PortVar: "PORT"},
		},
		Env: blueprint.EnvConfig{Template: ".env.example", Holes: map[string]string{"PORT": "{{PORT}}"}},
	}
}

// testDeps assembles Deps with fake backends, a real temp git repo, a real
// registry, and a real port pool in a high range.
func testDeps(t *testing.T, bp *blueprint.Blueprint) (*Deps, *fakeBM, *fakeDocker) {
	t.Helper()
	repo := initRepo(t)

	reg, err := registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	bm := &fakeBM{info: postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 3}}
	drv := &fakeDocker{running: true}

	reg.BlueprintStamp = hashStamp(repo, bp)

	pool, err := portpool.New(bp.PortPool.Start, bp.PortPool.End, reg)
	if err != nil {
		reg.Close()
		t.Fatalf("portpool.New: %v", err)
	}
	t.Cleanup(func() {
		drv.closeListeners()
		pool.Close()
		reg.Close()
	})
	deps := &Deps{
		Blueprint: bp,
		Registry:  reg,
		Pool:      pool,
		BM:        bm,
		Docker:    drv,
		RepoRoot:  repo,
	}
	return deps, bm, drv
}

// cleanupInstance tears down whatever is registered, for tests that fail
// midway through their assertions.
func cleanupInstance(t *testing.T, deps *Deps, name string) {
	t.Helper()
	if _, found := deps.Registry.GetInstance(name); found {
		if err := Down(context.Background(), deps, name); err != nil {
			t.Logf("cleanup: down %s: %v", name, err)
		}
	}
}

func assertNoResidue(t *testing.T, deps *Deps, name string) {
	t.Helper()
	if worktree.BranchExists(deps.RepoRoot, name) {
		t.Errorf("branch %q still exists", worktree.BranchName(name))
	}
	if _, err := os.Stat(filepath.Join(deps.RepoRoot, worktree.WorktreeRelPath(name))); !os.IsNotExist(err) {
		t.Errorf("worktree for %q still exists", name)
	}
	if _, found := deps.Registry.GetInstance(name); found {
		t.Errorf("registry still has instance %q", name)
	}
	if len(deps.Registry.PortAllocations) > 0 {
		t.Errorf("port allocations not released: %v", deps.Registry.PortAllocations)
	}
}

// --- tests ---

func TestInstance_UpSuccess(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not registered")
	}
	if rec.State != registry.StateRunning {
		t.Errorf("state = %q, want running", rec.State)
	}
	if rec.DBName != "plax_i1" {
		t.Errorf("db = %q, want plax_i1", rec.DBName)
	}
	if rec.Ports["PORT"] == 0 || rec.Ports["REDIS_PORT"] == 0 {
		t.Errorf("ports not allocated: %v", rec.Ports)
	}
	if rec.Provenance.BaseVersion != 3 {
		t.Errorf("base version = %d, want 3", rec.Provenance.BaseVersion)
	}
	if rec.Provenance.Toolchain == "" {
		t.Error("toolchain hash not recorded")
	}
	if deps.Registry.BlueprintStamp.ComposeHash == "" ||
		deps.Registry.BlueprintStamp.EnvExampleHash == "" ||
		deps.Registry.BlueprintStamp.ToolchainHash == "" {
		t.Errorf("blueprint stamp incomplete: %+v", deps.Registry.BlueprintStamp)
	}
	if len(rec.PIDStarts) != 1 {
		t.Errorf("pid start times not recorded: %v", rec.PIDStarts)
	}

	if !worktree.BranchExists(deps.RepoRoot, "i1") {
		t.Error("branch not created")
	}
	envData, err := os.ReadFile(filepath.Join(rec.WorktreePath, ".env"))
	if err != nil {
		t.Fatalf("derived .env: %v", err)
	}
	want := fmt.Sprintf("PORT=%d", rec.Ports["PORT"])
	if !strings.Contains(string(envData), want) {
		t.Errorf("derived .env missing %s:\n%s", want, envData)
	}

	if got := bm.clonedDBs(); len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("cloned = %v, want [plax_i1]", got)
	}
	if len(drv.createdNets) != 1 {
		t.Errorf("createdNets = %v", drv.createdNets)
	}
	if len(drv.started) != 1 || drv.started[0] != "redis" {
		t.Errorf("started = %v", drv.started)
	}
	if len(drv.runCfgs) != 1 {
		t.Fatalf("runCfgs = %v", drv.runCfgs)
	}
	if drv.runCfgs[0].InstanceName != "i1" {
		t.Errorf("InstanceName = %q, want i1", drv.runCfgs[0].InstanceName)
	}
	if drv.runCfgs[0].ServiceName != "redis" {
		t.Errorf("ServiceName = %q, want redis", drv.runCfgs[0].ServiceName)
	}
	if !process.IsAlive(rec.PIDs["app"]) {
		t.Error("native process should be alive after up")
	}
}

func TestInstance_Up_ScratchCreatedAndExcluded(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, _ := deps.Registry.GetInstance("i1")
	scratch := filepath.Join(rec.WorktreePath, "scratch")
	if info, err := os.Stat(scratch); err != nil || !info.IsDir() {
		t.Fatalf("scratch dir after up: info=%v err=%v", info, err)
	}

	excludeCmd := exec.Command("git", "rev-parse", "--git-path", "info/exclude")
	excludeCmd.Dir = rec.WorktreePath
	out, err := excludeCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-path: %v", err)
	}
	excludeData, err := os.ReadFile(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	if !strings.Contains(string(excludeData), "scratch/") {
		t.Errorf("exclude file missing scratch/:\n%s", excludeData)
	}

	status := exec.Command("git", "status", "--porcelain")
	status.Dir = rec.WorktreePath
	statusOut, err := status.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(statusOut) > 0 {
		t.Errorf("git status not clean after up:\n%s", statusOut)
	}
}

func TestInstance_Down_RemovesScratch(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rec, _ := deps.Registry.GetInstance("i1")
	scratch := filepath.Join(rec.WorktreePath, "scratch")
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch dir after up: %v", err)
	}

	// Lock the worktrees parent so git worktree remove fails mid-way,
	// exercising scratch removal independent of worktree deletion.
	wtParent := filepath.Dir(rec.WorktreePath)
	if err := os.Chmod(wtParent, 0555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wtParent, 0755) })

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Down: %v", err)
	}

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch residue after down: %v", err)
	}
	if _, found := deps.Registry.GetInstance("i1"); found {
		t.Error("registry still has instance")
	}
}

func TestInstance_UpDuplicateName(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := Up(context.Background(), deps, "i1", UpOptions{}); err == nil {
		t.Fatal("second Up should fail")
	}
}

func TestInstance_UpHyphenNameRejected(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	err := Up(context.Background(), deps, "foo-bar", UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "^[a-z][a-z0-9_]*$") {
		t.Fatalf("Up(foo-bar) = %v, want charset error", err)
	}
	assertNoResidue(t, deps, "foo-bar")
}

func TestInstance_UpInvalidBlueprintNoSideEffects(t *testing.T) {
	bp := testBlueprint()
	bp.Processes = append(bp.Processes, blueprint.ProcessDef{
		Name: "app", Isolation: blueprint.IsolationNative, Command: "sleep 60", Workdir: ".",
	})

	deps, bm, drv := testDeps(t, bp)

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate process") {
		t.Fatalf("Up = %v, want duplicate process error", err)
	}

	assertNoResidue(t, deps, "i1")
	if len(bm.clonedDBs()) > 0 || len(drv.createdNets) > 0 {
		t.Errorf("backends touched despite invalid blueprint: cloned=%v nets=%v", bm.clonedDBs(), drv.createdNets)
	}
}

func TestInstance_UpRollbackOnCloneFailure(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())
	bm.cloneFunc = func(context.Context, string) error {
		return errors.New("boom")
	}

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "cloning database") {
		t.Fatalf("Up = %v, want clone failure", err)
	}

	assertNoResidue(t, deps, "i1")
	if len(drv.removedNets) != 1 {
		t.Errorf("rollback did not remove network: %v", drv.removedNets)
	}
	if len(drv.removed) > 0 {
		t.Errorf("containers should never have started: removed=%v", drv.removed)
	}
}

func TestInstance_UpRollbackOnImmediateExit(t *testing.T) {
	bp := testBlueprint()
	bp.Processes[0].Command = "exit 1"

	deps, _, drv := testDeps(t, bp)

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("Up = %v, want immediate-exit error", err)
	}

	assertNoResidue(t, deps, "i1")
	if got := bmDropped(deps, t); len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("rollback should drop the cloned db, dropped=%v", got)
	}
	if len(drv.stopped) != 1 || len(drv.removed) != 1 {
		t.Errorf("rollback should stop/remove the container: stopped=%v removed=%v", drv.stopped, drv.removed)
	}
	if len(drv.removedNets) != 1 {
		t.Errorf("rollback should remove the network: %v", drv.removedNets)
	}
}

func TestInstance_UpEnvCheckFailure_RollsBack(t *testing.T) {
	bp := testBlueprint()
	deps, bm, drv := testDeps(t, bp)

	// Make the template contain an unresolved hole reference that Derive
	// will render successfully but CheckEnv will detect as unresolved.
	// Add a hole whose template value is passed through Render and appears
	// verbatim in the derived file. Then make CheckEnv detect it as an
	// unresolved hole by embedding {{}} in the template line directly.
	//
	// Actually, the simplest approach: write the user .env with a key whose
	// value contains {{}}. Derive will write it to the derived file.
	// CheckEnv's checkEnvNoUnresolved will then catch it.
	repo := deps.RepoRoot
	userEnvPath := filepath.Join(repo, ".env")
	if err := os.WriteFile(userEnvPath, []byte("UNRESOLVED_VAR={{MISSING}}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err == nil {
		t.Fatal("Up should fail with env check error")
	}
	if !strings.Contains(err.Error(), "env-unresolved-holes") {
		t.Fatalf("expected env-unresolved-holes error, got: %v", err)
	}

	assertNoResidue(t, deps, "i1")
	if len(bm.clonedDBs()) > 0 {
		t.Errorf("databases should not have been cloned: %v", bm.clonedDBs())
	}
	if len(drv.removedNets) != 1 {
		t.Errorf("network should have been rolled back, removedNets=%v", drv.removedNets)
	}
}

func TestInstance_Up_DoesNotBlockOnTCPReadiness(t *testing.T) {
	deps, _, drv := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	// Don't bind TCP listeners. A freshly started service legitimately takes
	// time to answer, so `up` must not block on or flag TCP readiness — that
	// is deferred to the live ls/status/verify probes. (Plan 16.)
	drv.skipBindListeners = true

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err != nil {
		t.Fatalf("Up should succeed without TCP readiness probe, got: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance should be registered after up")
	}
	if rec.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy (liveness + env passed)", rec.Health)
	}
	if !worktree.BranchExists(deps.RepoRoot, "i1") {
		t.Error("worktree should not have been rolled back")
	}
}

// bmDropped reaches the fake through the deps interface for assertions.
func bmDropped(deps *Deps, t *testing.T) []string {
	t.Helper()
	f, ok := deps.BM.(*fakeBM)
	if !ok {
		t.Fatal("deps.BM is not the fake")
	}
	return f.droppedDBs()
}

func TestInstance_UpRollbackOnCancel(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())

	cloneEntered := make(chan struct{})
	bm.cloneFunc = func(ctx context.Context, _ string) error {
		close(cloneEntered)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Up(ctx, deps, "i1", UpOptions{})
	}()

	<-cloneEntered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Up should fail when its context is canceled")
	}

	// Rollback must have run with a live context even though the operation
	// context was canceled.
	if len(drv.removedNets) != 1 {
		t.Fatalf("rollback did not remove network: %v", drv.removedNets)
	}
	if drv.removeNetCtxErrs[0] != nil {
		t.Errorf("cleanup ran with canceled context: %v", drv.removeNetCtxErrs[0])
	}
	assertNoResidue(t, deps, "i1")
}

func TestInstance_UpConcurrentPartialFailure(t *testing.T) {
	bp := testBlueprint()
	bp.Services["redis2"] = blueprint.ServiceDef{
		Isolation: blueprint.IsolationDedicated,
		Image:     "redis:7",
		Ports:     map[string]blueprint.PortDef{"6380": {Var: "REDIS2_PORT"}},
	}

	deps, _, drv := testDeps(t, bp)
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	drv.runErrFor = map[string]error{"redis2": errors.New("injected failure")}

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Up = %v, want injected failure error", err)
	}

	assertNoResidue(t, deps, "i1")

	if len(drv.removedNets) != 1 {
		t.Errorf("rollback did not remove network: %v", drv.removedNets)
	}

	if len(drv.stopped) != 1 || len(drv.removed) != 1 {
		t.Errorf("successful containers should be stopped+removed during rollback: stopped=%v removed=%v", drv.stopped, drv.removed)
	}
}

func TestInstance_DownSuccess(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rec, _ := deps.Registry.GetInstance("i1")
	pgid := rec.PIDs["app"]

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Down: %v", err)
	}

	assertNoResidue(t, deps, "i1")
	if process.IsAlive(pgid) {
		t.Error("native process should be dead after down")
	}
	if got := bm.droppedDBs(); len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("dropped = %v, want [plax_i1]", got)
	}
	if len(drv.stopped) != 1 || len(drv.removed) != 1 || len(drv.removedNets) != 1 {
		t.Errorf("docker teardown incomplete: stopped=%v removed=%v nets=%v", drv.stopped, drv.removed, drv.removedNets)
	}
}

func TestInstance_DownStopFailureStillRemoves(t *testing.T) {
	deps, _, drv := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	drv.stopErr = errors.New("stop failed")

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Down: %v", err)
	}

	if len(drv.stopped) != 1 {
		t.Errorf("StopService should have been called: stopped=%v", drv.stopped)
	}
	if len(drv.removed) != 1 {
		t.Errorf("RemoveService should have been called despite stop failure: removed=%v", drv.removed)
	}
	assertNoResidue(t, deps, "i1")
}

func TestInstance_DownNotFound(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Down(context.Background(), deps, "nope"); err == nil {
		t.Fatal("Down on nonexistent instance should fail")
	}
}

func TestInstance_DownMissingWorktreeBranchDeleted(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Manually delete the worktree and git's administrative entry, leaving
	// only the branch.
	if err := os.RemoveAll(filepath.Join(deps.RepoRoot, worktree.WorktreeRelPath("i1"))); err != nil {
		t.Fatal(err)
	}
	prune := exec.Command("git", "worktree", "prune")
	prune.Dir = deps.RepoRoot
	if out, err := prune.CombinedOutput(); err != nil {
		t.Fatalf("prune: %s", out)
	}

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Down should tolerate the missing worktree: %v", err)
	}
	if worktree.BranchExists(deps.RepoRoot, "i1") {
		t.Error("branch should be deleted even though the worktree was missing")
	}
	if _, found := deps.Registry.GetInstance("i1"); found {
		t.Error("registry entry should be removed")
	}
}

func TestInstance_DownNilBackendsStillCleans(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rec, _ := deps.Registry.GetInstance("i1")
	pgid := rec.PIDs["app"]

	// A Down with unavailable backends must still clean processes, worktree,
	// and registry.
	partial := &Deps{Registry: deps.Registry, RepoRoot: deps.RepoRoot}
	if err := Down(context.Background(), partial, "i1"); err != nil {
		t.Fatalf("Down with nil backends: %v", err)
	}

	if process.IsAlive(pgid) {
		t.Error("process should be terminated even with nil backends")
	}
	if worktree.BranchExists(deps.RepoRoot, "i1") {
		t.Error("branch should be deleted")
	}
	if _, found := deps.Registry.GetInstance("i1"); found {
		t.Error("registry entry should be removed")
	}
	if len(deps.Registry.PortAllocations) > 0 {
		t.Error("port allocations should be released")
	}
}

func TestInstance_UpMultipleDatabases(t *testing.T) {
	bp := testBlueprint()
	bp.Services["db"] = blueprint.ServiceDef{
		Isolation: blueprint.IsolationLogical,
		Type:      "postgres",
		Image:     "postgres:16",
		Databases: []blueprint.DatabaseDef{
			{Name: "test", From: "base"},
		},
	}

	deps, bm, _ := testDeps(t, bp)
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not registered")
	}

	if rec.DBName != "plax_i1" {
		t.Errorf("DBName = %q, want plax_i1", rec.DBName)
	}
	if rec.DBNames[""] != "plax_i1" {
		t.Errorf("DBNames[\"\"] = %q, want plax_i1", rec.DBNames[""])
	}
	if rec.DBNames["test"] != "plax_i1_test" {
		t.Errorf("DBNames[\"test\"] = %q, want plax_i1_test", rec.DBNames["test"])
	}

	cloned := bm.clonedDBs()
	if len(cloned) != 2 {
		t.Fatalf("cloned %d databases, want 2: %v", len(cloned), cloned)
	}
	hasPlax := false
	hasTest := false
	for _, db := range cloned {
		if db == "plax_i1" {
			hasPlax = true
		}
		if db == "plax_i1_test" {
			hasTest = true
		}
	}
	if !hasPlax || !hasTest {
		t.Errorf("missing expected databases, cloned=%v", cloned)
	}
}

func TestInstance_DownMultipleDatabases(t *testing.T) {
	bp := testBlueprint()
	bp.Services["db"] = blueprint.ServiceDef{
		Isolation: blueprint.IsolationLogical,
		Type:      "postgres",
		Image:     "postgres:16",
		Databases: []blueprint.DatabaseDef{
			{Name: "test", From: "base"},
		},
	}

	deps, bm, _ := testDeps(t, bp)
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Down: %v", err)
	}

	dropped := bm.droppedDBs()
	if len(dropped) != 2 {
		t.Fatalf("dropped %d databases, want 2: %v", len(dropped), dropped)
	}
	hasPlax := false
	hasTest := false
	for _, db := range dropped {
		if db == "plax_i1" {
			hasPlax = true
		}
		if db == "plax_i1_test" {
			hasTest = true
		}
	}
	if !hasPlax || !hasTest {
		t.Errorf("missing expected databases, dropped=%v", dropped)
	}
}

func TestInstance_DownOldRecordNoDBNames(t *testing.T) {
	deps, bm, _ := testDeps(t, testBlueprint())

	if err := deps.Registry.AddInstance("legacy", registry.InstanceRecord{
		Branch:       "plax/legacy",
		WorktreePath: filepath.Join(deps.RepoRoot, ".plax", "worktrees", "legacy"),
		CreatedAt:    time.Now(),
		State:        registry.StateSuspended,
		Ports:        map[string]int{},
		DBName:       "plax_legacy",
		ContainerIDs: map[string]string{},
		PIDs:         map[string]int{},
		PIDStarts:    map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Registry.Save(); err != nil {
		t.Fatal(err)
	}

	if err := Down(context.Background(), deps, "legacy"); err != nil {
		t.Fatalf("Down: %v", err)
	}

	dropped := bm.droppedDBs()
	if len(dropped) != 1 || dropped[0] != "plax_legacy" {
		t.Errorf("dropped = %v, want [plax_legacy]", dropped)
	}
}

func TestInstance_UpBackwardCompatible(t *testing.T) {
	deps, bm, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not registered")
	}

	if rec.DBName != "plax_i1" {
		t.Errorf("DBName = %q, want plax_i1", rec.DBName)
	}
	if rec.DBNames == nil {
		t.Fatal("DBNames is nil")
	}
	if len(rec.DBNames) != 1 {
		t.Errorf("DBNames has %d entries, want 1: %v", len(rec.DBNames), rec.DBNames)
	}
	if rec.DBNames[""] != "plax_i1" {
		t.Errorf("DBNames[\"\"] = %q, want plax_i1", rec.DBNames[""])
	}

	cloned := bm.clonedDBs()
	if len(cloned) != 1 || cloned[0] != "plax_i1" {
		t.Errorf("cloned = %v, want [plax_i1]", cloned)
	}
}

func TestInstance_UpWithRef(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	// Create a second branch in the test repo for the ref.
	createOtherBranch(t, deps.RepoRoot)
	otherRef := "other"

	deps.SourceRef = otherRef
	deps.ResolvedRef = otherRef

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up with ref: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not registered")
	}
	if rec.SourceRef != "other" {
		t.Errorf("SourceRef = %q, want %q", rec.SourceRef, "other")
	}
	otherCommit := gitRevParse(t, deps.RepoRoot, "other")
	if rec.BaseRef != "other" {
		t.Errorf("BaseRef = %q, want %q", rec.BaseRef, "other")
	}
	if rec.BaseCommit != otherCommit {
		t.Errorf("BaseCommit = %q, want %q", rec.BaseCommit, otherCommit)
	}
	if rec.State != registry.StateRunning {
		t.Errorf("state = %q, want running", rec.State)
	}
	if rec.Ports["PORT"] == 0 || rec.Ports["REDIS_PORT"] == 0 {
		t.Errorf("ports not allocated: %v", rec.Ports)
	}

	// Verify worktree is at the "other" branch's HEAD.
	_, wtCommit, err := worktree.WorktreeHead(rec.WorktreePath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if wtCommit != otherCommit {
		t.Errorf("worktree at %s, want other branch's HEAD (%s)", wtCommit, otherCommit)
	}

	if got := bm.clonedDBs(); len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("cloned = %v, want [plax_i1]", got)
	}
	if len(drv.createdNets) != 1 {
		t.Errorf("createdNets = %v", drv.createdNets)
	}
}

func TestInstance_UpWithoutRef(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not registered")
	}
	if rec.SourceRef != "" {
		t.Errorf("SourceRef should be empty without --ref, got %q", rec.SourceRef)
	}
	if rec.State != registry.StateRunning {
		t.Errorf("state = %q, want running", rec.State)
	}

	if got := bm.clonedDBs(); len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("cloned = %v, want [plax_i1]", got)
	}
	if len(drv.createdNets) != 1 {
		t.Errorf("createdNets = %v", drv.createdNets)
	}
}

func TestInstance_DownStalePGIDNotSignaled(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Corrupt the recorded start time to simulate PGID reuse: the recorded
	// process is gone and another process now owns the PGID.
	rec, _ := deps.Registry.GetInstance("i1")
	pgid := rec.PIDs["app"]
	if rec.PIDStarts["app"] == 0 {
		t.Skip("process start times unavailable on this platform")
	}
	rec.PIDStarts["app"]++
	deps.Registry.Instances["i1"] = rec

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Down: %v", err)
	}

	if !process.IsAlive(pgid) {
		t.Error("process with mismatched start time must not be signaled")
	}
	if _, found := deps.Registry.GetInstance("i1"); found {
		t.Error("registry entry should still be removed")
	}
}

// gitRevParse returns the commit SHA for a given ref.
func gitRevParse(t *testing.T, repo, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// createOtherBranch creates a "other" branch at the repo HEAD, advancing
// main past it so the branches diverge.
func createOtherBranch(t *testing.T, repoRoot string) {
	t.Helper()
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "other-commit")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit for other branch: %s", out)
	}
	cmd = exec.Command("git", "branch", "other")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("branch other: %s", out)
	}
	cmd = exec.Command("git", "reset", "--hard", "HEAD~1")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reset: %s", out)
	}
}
