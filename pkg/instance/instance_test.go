package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/worktree"
)

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

func (f *fakeBM) droppedDBs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dropped...)
}

type fakeDocker struct {
	runErr       error
	running      bool
	runningErr   error
	createNetErr error

	mu               sync.Mutex
	createdNets      []string
	removedNets      []string
	started          []string
	stopped          []string
	removed          []string
	removeNetCtxErrs []error // ctx.Err() observed at each RemoveNetwork call
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
	if f.runErr != nil {
		return "", f.runErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "cid-" + cfg.ServiceName
	f.started = append(f.started, cfg.ServiceName)
	return id, nil
}

func (f *fakeDocker) StopService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
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
		Seed:      blueprint.SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
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

	deps := &Deps{
		Blueprint: bp,
		Registry:  reg,
		Pool:      portpool.New(bp.PortPool.Start, bp.PortPool.End, reg),
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

func TestUp_Success(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not registered")
	}
	if rec.State != "running" {
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
	if !process.IsAlive(rec.PIDs["app"]) {
		t.Error("native process should be alive after up")
	}
}

func TestUp_DuplicateName(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := Up(context.Background(), deps, "i1"); err == nil {
		t.Fatal("second Up should fail")
	}
}

func TestUp_HyphenNameRejected(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	err := Up(context.Background(), deps, "foo-bar")
	if err == nil || !strings.Contains(err.Error(), "^[a-z][a-z0-9_]*$") {
		t.Fatalf("Up(foo-bar) = %v, want charset error", err)
	}
	assertNoResidue(t, deps, "foo-bar")
}

func TestUp_InvalidBlueprint_NoSideEffects(t *testing.T) {
	bp := testBlueprint()
	bp.Processes = append(bp.Processes, blueprint.ProcessDef{
		Name: "app", Isolation: blueprint.IsolationNative, Command: "sleep 60", Workdir: ".",
	})

	deps, bm, drv := testDeps(t, bp)

	err := Up(context.Background(), deps, "i1")
	if err == nil || !strings.Contains(err.Error(), "duplicate process") {
		t.Fatalf("Up = %v, want duplicate process error", err)
	}

	assertNoResidue(t, deps, "i1")
	if len(bm.clonedDBs()) > 0 || len(drv.createdNets) > 0 {
		t.Errorf("backends touched despite invalid blueprint: cloned=%v nets=%v", bm.clonedDBs(), drv.createdNets)
	}
}

func TestUp_RollbackOnCloneFailure(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())
	bm.cloneFunc = func(context.Context, string) error {
		return errors.New("boom")
	}

	err := Up(context.Background(), deps, "i1")
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

func TestUp_RollbackOnImmediateExit(t *testing.T) {
	bp := testBlueprint()
	bp.Processes[0].Command = "exit 1"

	deps, _, drv := testDeps(t, bp)

	err := Up(context.Background(), deps, "i1")
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

// bmDropped reaches the fake through the deps interface for assertions.
func bmDropped(deps *Deps, t *testing.T) []string {
	t.Helper()
	f, ok := deps.BM.(*fakeBM)
	if !ok {
		t.Fatal("deps.BM is not the fake")
	}
	return f.droppedDBs()
}

func TestUp_RollbackOnCancel(t *testing.T) {
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
		done <- Up(ctx, deps, "i1")
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

func TestDown_Success(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
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

func TestDown_NotFound(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Down(context.Background(), deps, "nope"); err == nil {
		t.Fatal("Down on nonexistent instance should fail")
	}
}

func TestDown_MissingWorktree_BranchDeleted(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1"); err != nil {
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

func TestDown_NilBackends_StillCleans(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
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

func TestDown_StalePGID_NotSignaled(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1"); err != nil {
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
