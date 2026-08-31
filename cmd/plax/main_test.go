package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/instance"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/record"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/upgrade"
	"github.com/apollopower/plax/pkg/worktree"
)

// fakeUpgradeServer serves a canned latest-release payload (or HTTP status)
// for the GitHub API shape runUpgrade consumes.
func fakeUpgradeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runUpgradeAsChild re-executes the test binary in child mode so the
// os.Exit paths inside runUpgrade are exercised for real. Returns the exit
// code and combined output.
func runUpgradeAsChild(t *testing.T, apiBase, ver string) (int, string) {
	t.Helper()
	clean := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestUpgradeChild_CheckMode")
	cmd.Env = append(os.Environ(),
		"PLAX_TEST_UPGRADE_CHILD=1",
		"PLAX_TEST_UPGRADE_API="+apiBase,
		"PLAX_TEST_UPGRADE_VERSION="+ver,
		"PATH="+clean,
		"GOBIN=",
		"GOPATH=",
		"HOME="+clean,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running child: %v", err)
		}
		code = ee.ExitCode()
	}
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("child test failed:\n%s", out)
	}
	return code, string(out)
}

// TestUpgradeChild_CheckMode is the child-mode entry point driven by
// runUpgradeAsChild; skipped when run directly.
func TestUpgradeChild_CheckMode(t *testing.T) {
	if os.Getenv("PLAX_TEST_UPGRADE_CHILD") != "1" {
		t.Skip("child mode only")
	}
	t.Cleanup(func() { upgrade.APIBase = upgrade.DefaultAPIBase })
	upgrade.APIBase = os.Getenv("PLAX_TEST_UPGRADE_API")
	version = os.Getenv("PLAX_TEST_UPGRADE_VERSION")

	_ = runUpgrade(UpgradeCmd{Check: true})
}

func TestUpgrade_Check_Outdated_Exit1(t *testing.T) {
	srv := fakeUpgradeServer(t, 200, `{"tag_name":"v0.2.0","assets":[]}`)

	code, out := runUpgradeAsChild(t, srv.URL, "v0.1.1")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"current: v0.1.1", "latest:  v0.2.0", "method:  direct"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUpgrade_Check_Current_Exit0(t *testing.T) {
	srv := fakeUpgradeServer(t, 200, `{"tag_name":"v0.1.1","assets":[]}`)

	code, out := runUpgradeAsChild(t, srv.URL, "v0.1.1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "latest:  v0.1.1") {
		t.Fatalf("output missing latest line:\n%s", out)
	}
}

func TestUpgrade_Check_LookupFailure_Exit2(t *testing.T) {
	srv := fakeUpgradeServer(t, 500, "")

	code, out := runUpgradeAsChild(t, srv.URL, "v0.1.1")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", code, out)
	}
}

func TestUpgrade_UpgradeMode_DevBuild_Refuses(t *testing.T) {
	version = "dev"

	err := runUpgrade(UpgradeCmd{})
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Fatalf("runUpgrade on dev build = %v, want dev-build refusal", err)
	}
}

func TestUpgrade_UpgradeMode_DevBuild_Force_Proceeds(t *testing.T) {
	// --force on a dev build skips the refusal and runs the direct path. A
	// release without a matching asset must fail cleanly before touching
	// anything.
	srv := fakeUpgradeServer(t, 200, `{"tag_name":"v0.2.0","assets":[]}`)
	t.Cleanup(func() { upgrade.APIBase = upgrade.DefaultAPIBase })
	upgrade.APIBase = srv.URL
	version = "dev"

	err := runUpgrade(UpgradeCmd{Force: true})
	if err == nil || !strings.Contains(err.Error(), "no archive for") {
		t.Fatalf("runUpgrade(force) = %v, want no-archive error", err)
	}
}

// TestUp_KongParsesSkip verifies both --skip forms (comma-separated and
// repeated) reach the Up command struct for the canonical parse set.
func TestUp_KongParsesSkip(t *testing.T) {
	var cli CLI
	k, err := kong.New(&cli)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := k.Parse([]string{"up", "--skip", "migrate,verify", "--skip", "verify", "i1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Kong splits comma-separated slice values; repeated flags accumulate.
	// Both forms resolve to the same set in ParseSkip.
	if len(cli.Up.Skip) != 3 || cli.Up.Skip[0] != "migrate" || cli.Up.Skip[1] != "verify" || cli.Up.Skip[2] != "verify" {
		t.Errorf("Skip = %v, want [migrate verify verify]", cli.Up.Skip)
	}
}

func TestUp_UnknownSkipStep_FailsBeforeSideEffects(t *testing.T) {
	err := runUp(UpCmd{Name: "i1", Skip: []string{"migrate,bogus"}})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("runUp = %v, want unknown-step error", err)
	}
	if !strings.Contains(err.Error(), "migrate, verify") {
		t.Errorf("error should list valid steps: %v", err)
	}
}

func TestUp_EmptySkipStep_FailsBeforeSideEffects(t *testing.T) {
	if err := runUp(UpCmd{Name: "i1", Skip: []string{"migrate,"}}); err == nil {
		t.Fatal("runUp with an empty skip step should fail")
	}
}

// --- work record tests (plan 22) ---

// initRecordRepo returns a git repo with a minimal plax.json, enough for the
// record commands (log/record/verdict/down), which resolve records from
// .plax/records/<name>.md rather than the registry.
func initRecordRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plax.json"),
		[]byte(`{"version":1,"name":"test","port_pool":{"start":25000,"end":25100}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "add", "."},
		{"git", "commit", "-m", "init"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return dir
}

// initUpRepo returns a committed git repo with an env template, the fixture
// for faked instance.Up runs.
func initUpRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
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
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "add", "."},
		{"git", "commit", "-m", "init"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return dir
}

func recordTestBlueprint() *blueprint.Blueprint {
	return &blueprint.Blueprint{
		Version:   1,
		Name:      "test",
		PortPool:  blueprint.PortPool{Start: 25000, End: 25100},
		Toolchain: ".tool-versions",
		Seed:      blueprint.SeedConfig{Migrate: "true", Command: "c", Workdir: "."},
		Services: map[string]blueprint.ServiceDef{
			"db": {Isolation: blueprint.IsolationLogical, Type: "postgres"},
		},
		Processes: []blueprint.ProcessDef{
			{Name: "app", Isolation: blueprint.IsolationNative, Command: "sleep 60", Workdir: ".", PortVar: "PORT"},
		},
		Env: blueprint.EnvConfig{Template: ".env.example", Holes: map[string]string{"PORT": "{{PORT}}"}},
	}
}

// recFakeBM records clone/drop calls; all queries answer healthy.
type recFakeBM struct {
	mu      sync.Mutex
	cloned  []string
	dropped []string
}

func (f *recFakeBM) BaseStatus(context.Context) (postgres.BaseInfo, error) {
	return postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 1}, nil
}

func (f *recFakeBM) CloneBase(_ context.Context, db string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cloned = append(f.cloned, db)
	return nil
}

func (f *recFakeBM) DropInstanceDB(_ context.Context, db string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, db)
	return nil
}

func (f *recFakeBM) InstanceProvenance(_ context.Context, db string) (*postgres.ProvenanceRow, error) {
	return &postgres.ProvenanceRow{Version: 1, Source: db, SchemaHash: "abc"}, nil
}

func (f *recFakeBM) InstanceDBExists(context.Context, string) (bool, error) { return true, nil }

func (f *recFakeBM) AppliedMigrations(context.Context, string) ([]string, error) { return nil, nil }

// recFakeDocker records lifecycle calls; everything answers success.
type recFakeDocker struct {
	mu          sync.Mutex
	createdNets []string
	removedNets []string
	started     []string
	stopped     []string
	removed     []string
}

func (f *recFakeDocker) CreateNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdNets = append(f.createdNets, name)
	return nil
}

func (f *recFakeDocker) RemoveNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedNets = append(f.removedNets, name)
	return nil
}

func (f *recFakeDocker) RunService(_ context.Context, cfg docker.ServiceConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, cfg.ServiceName)
	return "cid-" + cfg.ServiceName, nil
}

func (f *recFakeDocker) StartService(context.Context, string) (bool, error) { return false, nil }

func (f *recFakeDocker) StopService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *recFakeDocker) RemoveService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *recFakeDocker) ServiceRunning(context.Context, string) (bool, error) { return true, nil }

func (f *recFakeDocker) ServiceExists(context.Context, string) (bool, error) { return true, nil }

// recordUpDeps assembles instance.Deps with fakes, a real git repo, a real
// registry, and a real port pool — the unit-level equivalent of runUp's
// buildDeps, so record integration can be tested without Docker/Postgres.
func recordUpDeps(t *testing.T, bp *blueprint.Blueprint) (*instance.Deps, *recFakeBM, *recFakeDocker) {
	t.Helper()
	repo := initUpRepo(t)
	reg, err := registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	bm := &recFakeBM{}
	drv := &recFakeDocker{}
	pool, err := portpool.New(bp.PortPool.Start, bp.PortPool.End, reg)
	if err != nil {
		reg.Close()
		t.Fatalf("portpool.New: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		reg.Close()
	})
	return &instance.Deps{
		Blueprint: bp,
		Registry:  reg,
		Pool:      pool,
		BM:        bm,
		Docker:    drv,
		RepoRoot:  repo,
	}, bm, drv
}

func cleanupRecordInstance(t *testing.T, deps *instance.Deps, name string) {
	t.Helper()
	if _, found := deps.Registry.GetInstance(name); found {
		if err := instance.Down(context.Background(), deps, name); err != nil {
			t.Logf("cleanup: down %s: %v", name, err)
		}
	}
}

// commitInWorktree writes a file and commits it inside a worktree, returning
// the new HEAD commit.
func commitInWorktree(t *testing.T, wtPath, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wtPath, file), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", file},
		{"git", "commit", "-m", "work: " + file},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return gitRevParse(t, wtPath, "HEAD")
}

// gitRevParse returns the commit SHA a ref resolves to in a repo/worktree.
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

func writeIntentFile(t *testing.T, repo, name, content string) string {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUp_WithIntent_CreatesRootRecord(t *testing.T) {
	deps, _, _ := recordUpDeps(t, recordTestBlueprint())
	t.Cleanup(func() { cleanupRecordInstance(t, deps, "i1") })

	err := instance.Up(context.Background(), deps, "i1", instance.UpOptions{
		Record: &record.CreateInput{Instance: "i1", Intent: "root task"},
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, err := record.Read(deps.RepoRoot, "i1")
	if err != nil {
		t.Fatalf("record.Read: %v", err)
	}
	if rec.Instance != "i1" || rec.Intent != "root task" {
		t.Errorf("record = %+v, want i1 with the supplied intent", rec)
	}
	if rec.Parent != "" || rec.BaseCommit != "" {
		t.Errorf("root record must not carry lineage: %+v", rec)
	}
	if _, found := deps.Registry.GetInstance("i1"); !found {
		t.Error("instance should be registered")
	}
}

func TestUp_WithParentAndIntent_CreatesStackedChild(t *testing.T) {
	deps, _, _ := recordUpDeps(t, recordTestBlueprint())
	t.Cleanup(func() {
		cleanupRecordInstance(t, deps, "i0")
		cleanupRecordInstance(t, deps, "i1")
	})

	if err := instance.Up(context.Background(), deps, "i0", instance.UpOptions{
		Record: &record.CreateInput{Instance: "i0", Intent: "root task"},
	}); err != nil {
		t.Fatalf("Up i0: %v", err)
	}
	rec0, _ := deps.Registry.GetInstance("i0")
	c0 := commitInWorktree(t, rec0.WorktreePath, "work.txt", "i0 work\n")

	err := instance.Up(context.Background(), deps, "i1", instance.UpOptions{
		Record: &record.CreateInput{Instance: "i1", Parent: "i0", BaseCommit: c0, Intent: "child task"},
	})
	if err != nil {
		t.Fatalf("Up i1: %v", err)
	}

	rec1, err := record.Read(deps.RepoRoot, "i1")
	if err != nil {
		t.Fatalf("record.Read: %v", err)
	}
	if rec1.Parent != "i0" {
		t.Errorf("record parent = %q, want i0", rec1.Parent)
	}
	if rec1.BaseCommit != c0 {
		t.Errorf("record base_commit = %q, want %q", rec1.BaseCommit, c0)
	}

	// The child branch and worktree start at the parent's exact HEAD.
	childRec, _ := deps.Registry.GetInstance("i1")
	if got := gitRevParse(t, deps.RepoRoot, worktree.BranchName("i1")); got != c0 {
		t.Errorf("branch %s at %s, want %s", worktree.BranchName("i1"), got, c0)
	}
	_, wtCommit, err := worktree.WorktreeHead(childRec.WorktreePath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if wtCommit != c0 {
		t.Errorf("child worktree at %s, want %s", wtCommit, c0)
	}
}

// registerParent creates a registered, tracked parent instance with a real
// worktree in the repo and returns the worktree path.
func registerParent(t *testing.T, repo string) string {
	t.Helper()
	wtPath, err := worktree.Create(repo, "i0", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Create(repo, record.CreateInput{Instance: "i0", Intent: "parent task"}); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddInstance("i0", registry.InstanceRecord{
		Branch:       worktree.BranchName("i0"),
		WorktreePath: wtPath,
		CreatedAt:    time.Now(),
		State:        registry.StateRunning,
		Ports:        map[string]int{},
		ContainerIDs: map[string]string{},
		PIDs:         map[string]int{},
		PIDStarts:    map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg.Close()
	return wtPath
}

func TestUp_ParentResolution_ReturnsExactWorktreeHead(t *testing.T) {
	repo := initUpRepo(t)
	wtPath := registerParent(t, repo)
	c0 := commitInWorktree(t, wtPath, "work.txt", "parent work\n")
	intent := writeIntentFile(t, repo, "intent.md", "child task\n")

	rec, err := buildRecordInput(repo, UpCmd{Name: "i1", Root: repo, Parent: "i0", Intent: intent})
	if err != nil {
		t.Fatalf("buildRecordInput: %v", err)
	}
	if rec == nil {
		t.Fatal("record input missing")
	}
	if rec.Parent != "i0" {
		t.Errorf("record parent = %q, want i0", rec.Parent)
	}
	if rec.BaseCommit != c0 {
		t.Errorf("record base_commit = %q, want the parent worktree HEAD %q", rec.BaseCommit, c0)
	}
	if rec.Intent != "child task\n" {
		t.Errorf("record intent = %q, want the intent file content", rec.Intent)
	}
}

func TestUp_ParentResolution_RejectsDirtyParent(t *testing.T) {
	repo := initUpRepo(t)
	wtPath := registerParent(t, repo)
	// Operator work — a modified tracked file — makes the parent unusable
	// as an exact base; only the plax-derived root .env is tolerated.
	if err := os.WriteFile(filepath.Join(wtPath, ".env.example"), []byte("PORT=9999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	intent := writeIntentFile(t, repo, "intent.md", "child task\n")

	_, err := buildRecordInput(repo, UpCmd{Name: "i1", Root: repo, Parent: "i0", Intent: intent})
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("buildRecordInput = %v, want dirty-parent rejection", err)
	}
}

func TestUp_IntentFileWithSectionMarker_RejectedBeforeSideEffects(t *testing.T) {
	repo := initRecordRepo(t)
	// A markdown heading in the intent would be read as a record section;
	// up must reject it before any side effect instead of writing a record
	// its own readers cannot parse.
	intent := writeIntentFile(t, repo, "intent.md", "task\n## Requirements\none\n")
	err := runUp(UpCmd{Name: "i1", Root: repo, Intent: intent})
	if err == nil || !strings.Contains(err.Error(), "## ") {
		t.Fatalf("runUp = %v, want section-marker rejection", err)
	}
	if worktree.BranchExists(repo, "i1") {
		t.Error("rejected intent must not create a branch or worktree")
	}
}

func TestUp_ParentWithMalformedRecord_Fails(t *testing.T) {
	repo := initUpRepo(t)
	registerParent(t, repo)
	// Corrupt the parent's record: the child must not be created, and the
	// diagnostic must say the record is unreadable, not untracked.
	if err := os.WriteFile(record.Path(repo, "i0"), []byte("garbage\n"), 0600); err != nil {
		t.Fatal(err)
	}
	intent := writeIntentFile(t, repo, "intent.md", "child task\n")

	_, err := buildRecordInput(repo, UpCmd{Name: "i1", Root: repo, Parent: "i0", Intent: intent})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("buildRecordInput = %v, want unreadable-record diagnostic", err)
	}
	if strings.Contains(err.Error(), "no work record") {
		t.Errorf("error must not mislabel a corrupted record as untracked: %v", err)
	}
	if worktree.BranchExists(repo, "i1") {
		t.Error("malformed parent must not create a branch or worktree")
	}
}

func TestUp_ParentAndRefMutuallyExclusive(t *testing.T) {
	repo := initRecordRepo(t)
	intent := writeIntentFile(t, repo, "intent.md", "task\n")

	err := runUp(UpCmd{Name: "i1", Root: repo, Parent: "i0", Ref: "main", Intent: intent})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("runUp = %v, want mutually-exclusive error", err)
	}
	if worktree.BranchExists(repo, "i1") {
		t.Error("invalid combination must not create a branch")
	}
}

func TestUp_ParentMissing_FailsBeforeCreatingChild(t *testing.T) {
	repo := initRecordRepo(t)
	intent := writeIntentFile(t, repo, "intent.md", "task\n")

	err := runUp(UpCmd{Name: "i1", Root: repo, Parent: "i0", Intent: intent})
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "registered") {
		t.Fatalf("runUp = %v, want missing-parent error naming the registry", err)
	}
	if worktree.BranchExists(repo, "i1") {
		t.Error("missing parent must not create a branch or worktree")
	}
}

func TestUp_ParentWithoutRecord_Fails(t *testing.T) {
	repo := initRecordRepo(t)
	intent := writeIntentFile(t, repo, "intent.md", "task\n")

	reg, err := registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddInstance("i0", registry.InstanceRecord{
		Branch:       "plax/i0",
		WorktreePath: filepath.Join(repo, ".plax", "worktrees", "i0"),
		CreatedAt:    time.Now(),
		State:        registry.StateRunning,
		Ports:        map[string]int{},
		ContainerIDs: map[string]string{},
		PIDs:         map[string]int{},
		PIDStarts:    map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	err = runUp(UpCmd{Name: "i1", Root: repo, Parent: "i0", Intent: intent})
	if err == nil || !strings.Contains(err.Error(), "no work record") {
		t.Fatalf("runUp = %v, want untracked-parent error", err)
	}
	if worktree.BranchExists(repo, "i1") {
		t.Error("untracked parent must not create a branch or worktree")
	}
}

func TestUp_RecordFailure_RollsBack(t *testing.T) {
	deps, bm, drv := recordUpDeps(t, recordTestBlueprint())

	// A record already exists for i1: the required record phase fails, and
	// every resource created by up must be rolled back while the existing
	// record is preserved.
	if err := record.Create(deps.RepoRoot, record.CreateInput{Instance: "i1", Intent: "pre-existing"}); err != nil {
		t.Fatal(err)
	}

	err := instance.Up(context.Background(), deps, "i1", instance.UpOptions{
		Record: &record.CreateInput{Instance: "i1", Intent: "new intent"},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Up = %v, want record-collision error", err)
	}

	if worktree.BranchExists(deps.RepoRoot, "i1") {
		t.Error("rollback left the branch")
	}
	if _, err := os.Stat(filepath.Join(deps.RepoRoot, worktree.WorktreeRelPath("i1"))); !os.IsNotExist(err) {
		t.Error("rollback left the worktree")
	}
	if _, found := deps.Registry.GetInstance("i1"); found {
		t.Error("rollback left the registry entry")
	}
	if len(drv.removedNets) != 1 {
		t.Errorf("rollback did not remove the network: %v", drv.removedNets)
	}
	if got := bm.dropped; len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("rollback did not drop the clone: %v", got)
	}
	rec, err := record.Read(deps.RepoRoot, "i1")
	if err != nil {
		t.Fatalf("existing record should be preserved: %v", err)
	}
	if rec.Intent != "pre-existing" {
		t.Errorf("existing record was clobbered: intent = %q", rec.Intent)
	}
}

func TestUp_NoIntentParent_WarnsUntracked(t *testing.T) {
	repo := initRecordRepo(t)
	// runUp proceeds (and later fails for missing Postgres); the warning
	// must already be on stderr.
	stderr, _ := captureStderrErr(t, func() error {
		return runUp(UpCmd{Name: "i1", Root: repo})
	})
	if !strings.Contains(stderr, "no work record will be created") {
		t.Errorf("untracked up should warn on stderr, got:\n%s", stderr)
	}
}

func TestLog_AppendsToExistingRecord(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}

	err := runLog(LogCmd{Name: "i1", Root: repo, Text: []string{"--", "Found the failing retry path"}})
	if err != nil {
		t.Fatalf("runLog: %v", err)
	}
	text, err := record.ReadText(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "## log\nat: ") || !strings.Contains(text, "Found the failing retry path") {
		t.Errorf("log entry missing:\n%s", text)
	}
}

func TestLog_MissingRecord_Fails(t *testing.T) {
	repo := initRecordRepo(t)
	err := runLog(LogCmd{Name: "i1", Root: repo, Text: []string{"note"}})
	if err == nil || !strings.Contains(err.Error(), "no record") {
		t.Fatalf("runLog = %v, want missing-record error (log never creates a record)", err)
	}
}

func TestLog_AfterDown_UsesPreservedRecord(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	// No registry entry at all — the record resolves by path.
	err := runLog(LogCmd{Name: "i1", Root: repo, Text: []string{"note after down"}})
	if err != nil {
		t.Fatalf("runLog after down: %v", err)
	}
	rec, err := record.Read(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Log) != 1 || rec.Log[0].Text != "note after down" {
		t.Errorf("log = %+v, want the preserved-record note", rec.Log)
	}
}

func TestVerdict_AppendsStructuredVerdict(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task", Contract: []string{"tests"}}); err != nil {
		t.Fatal(err)
	}

	err := runVerdict(VerdictCmd{Name: "i1", Root: repo, Status: "pass", Contract: "pass", Summary: []string{"--", "Tests and typecheck pass."}})
	if err != nil {
		t.Fatalf("runVerdict: %v", err)
	}
	rec, err := record.Read(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict == nil {
		t.Fatal("verdict missing")
	}
	if rec.Verdict.Status != "pass" || rec.Verdict.Contract != "pass" || rec.Verdict.Summary != "Tests and typecheck pass." {
		t.Errorf("verdict = %+v, want status/contract/summary authored", rec.Verdict)
	}
	if rec.Verdict.At.IsZero() {
		t.Error("verdict timestamp missing")
	}
}

func TestVerdict_RejectsSecondVerdict(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := runVerdict(VerdictCmd{Name: "i1", Root: repo, Status: "pass"}); err != nil {
		t.Fatalf("first verdict: %v", err)
	}

	err := runVerdict(VerdictCmd{Name: "i1", Root: repo, Status: "fail"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second verdict = %v, want author-once rejection", err)
	}
	rec, err := record.Read(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict == nil || rec.Verdict.Status != "pass" {
		t.Errorf("first verdict not preserved: %+v", rec.Verdict)
	}
}

func TestVerdict_DoesNotClaimVerifyResults(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	// A registry entry marked unhealthy must remain untouched: the verdict
	// is the operator's declaration, not a verify result.
	regPath := filepath.Join(repo, ".plax", "registry.json")
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddInstance("i1", registry.InstanceRecord{
		Branch:       "plax/i1",
		WorktreePath: filepath.Join(repo, ".plax", "worktrees", "i1"),
		CreatedAt:    time.Now(),
		State:        registry.StateRunning,
		Ports:        map[string]int{},
		ContainerIDs: map[string]string{},
		PIDs:         map[string]int{},
		PIDStarts:    map[string]int64{},
		Health:       registry.HealthUnhealthy,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg.Close()
	before, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := runVerdict(VerdictCmd{Name: "i1", Root: repo, Status: "pass"}); err != nil {
		t.Fatalf("runVerdict: %v", err)
	}
	after, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("verdict must not touch registry/verification state")
	}
}

func TestVerdict_AfterDown_UsesPreservedRecord(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	err := runVerdict(VerdictCmd{Name: "i1", Root: repo, Status: "pass", Summary: []string{"done"}})
	if err != nil {
		t.Fatalf("runVerdict after down: %v", err)
	}
	rec, err := record.Read(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict == nil || rec.Verdict.Status != "pass" {
		t.Errorf("verdict after down missing: %+v", rec.Verdict)
	}
}

func TestRecord_DefaultPrintsOriginalText(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task", Contract: []string{"tests"}}); err != nil {
		t.Fatal(err)
	}
	want, err := record.ReadText(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}

	got := captureStdout(t, func() error {
		return runRecord(RecordCmd{Name: "i1", Root: repo})
	})
	if got != want {
		t.Errorf("default output is not the complete record text:\n%q != %q", got, want)
	}
}

func TestRecord_JSON_ProjectsParsedRecord(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{
		Instance:   "i1",
		Parent:     "i0",
		BaseCommit: "0123456789abcdef0123456789abcdef01234567",
		Intent:     "task",
		Contract:   []string{"tests"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := record.Append(repo, "i1", "note", time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteVerdict(repo, "i1", record.Verdict{Status: "pass", Contract: "pass", Summary: "ok"}, time.Date(2026, 8, 29, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		return runRecord(RecordCmd{Name: "i1", Root: repo, JSON: true})
	})
	var got record.Record
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if got.Instance != "i1" || got.Parent != "i0" || got.BaseCommit != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("lineage projection wrong: %+v", got)
	}
	if got.Intent != "task" || len(got.Contract) != 1 || got.Contract[0] != "tests" {
		t.Errorf("intent/contract projection wrong: %+v", got)
	}
	if len(got.Log) != 1 || got.Log[0].Text != "note" || !got.Log[0].At.Equal(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("log projection wrong: %+v", got.Log)
	}
	if got.Verdict == nil || got.Verdict.Status != "pass" || got.Verdict.Contract != "pass" || got.Verdict.Summary != "ok" {
		t.Errorf("verdict projection wrong: %+v", got.Verdict)
	}
}

func TestDown_PreservesRecord(t *testing.T) {
	repo := initRecordRepo(t)
	if err := record.Create(repo, record.CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddInstance("i1", registry.InstanceRecord{
		Branch:       "plax/i1",
		WorktreePath: filepath.Join(repo, ".plax", "worktrees", "i1"),
		CreatedAt:    time.Now(),
		State:        registry.StateRunning,
		Ports:        map[string]int{},
		ContainerIDs: map[string]string{},
		PIDs:         map[string]int{},
		PIDStarts:    map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	if err := runDown(DownCmd{Name: "i1", Root: repo}); err != nil {
		t.Fatalf("runDown: %v", err)
	}

	reg, err = registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, found := reg.GetInstance("i1"); found {
		t.Error("instance should be removed from the registry")
	}
	reg.Close()

	if _, err := record.Read(repo, "i1"); err != nil {
		t.Errorf("record must survive down: %v", err)
	}
}

// captureStdout runs fn with os.Stdout piped and returns what was written.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	out, err := captureStdoutErr(fn)
	if err != nil {
		t.Fatalf("captured call: %v", err)
	}
	return out
}

func captureStdoutErr(fn func() error) (string, error) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		return "", readErr
	}
	return string(data), runErr
}

// captureStderrErr runs fn with os.Stderr piped, returning both streams'
// result and the call's error.
func captureStderrErr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	runErr := fn()
	_ = w.Close()
	os.Stderr = old
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), runErr
}
