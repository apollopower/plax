package instance

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/worktree"
)

func TestParseSkip_AcceptsCommaSeparatedNames(t *testing.T) {
	set, err := ParseSkip([]string{"migrate,verify", "verify"})
	if err != nil {
		t.Fatalf("ParseSkip: %v", err)
	}
	want := map[string]bool{"migrate": true, "verify": true}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("set = %v, want %v", set, want)
	}
}

func TestParseSkip_RejectsUnknownName(t *testing.T) {
	_, err := ParseSkip([]string{"migrate,bogus"})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("ParseSkip = %v, want unknown-step error naming bogus", err)
	}
	if !strings.Contains(err.Error(), "migrate, verify") {
		t.Errorf("error should list valid steps: %v", err)
	}
}

func TestParseSkip_RejectsEmptyName(t *testing.T) {
	for _, names := range [][]string{{"migrate,"}, {""}, {"migrate, ,verify"}} {
		if _, err := ParseSkip(names); err == nil {
			t.Errorf("ParseSkip(%q) should fail", names)
		}
	}
}

func TestMigrationCount_DiffersAppliedSets(t *testing.T) {
	before := map[string][]string{
		"plax_i1":      {"001", "002"},
		"plax_i1_test": {"001"},
	}
	after := map[string][]string{
		"plax_i1":      {"001", "002", "003"},
		"plax_i1_test": {"001", "003"},
	}
	counts := migrationCounts(before, after)
	want := map[string]int{"plax_i1": 1, "plax_i1_test": 1}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %v, want %v", counts, want)
	}
	if got := migrateReport(counts); got != "2 applied (plax_i1: 1, plax_i1_test: 1)" {
		t.Errorf("report = %q, want per-database detail sorted deterministically", got)
	}
	if got := migrateReport(map[string]int{"plax_i1": 0}); got != "0 applied" {
		t.Errorf("report = %q, want measured 0 applied", got)
	}
}

func TestMigrationCount_UnconfiguredHasNoFabricatedCount(t *testing.T) {
	if got := migrateReport(nil); got != "complete (applied count unavailable)" {
		t.Errorf("report = %q, want no fabricated count", got)
	}
}

// worktreeFile returns the absolute path of a file inside an instance worktree.
func worktreeFile(deps *Deps, name string, elems ...string) string {
	return filepath.Join(append([]string{deps.RepoRoot, worktree.WorktreeRelPath(name)}, elems...)...)
}

func TestInstance_Up_MigratesBeforeWorkloads(t *testing.T) {
	bp := testBlueprint()
	// Each stage only succeeds if the previous one left its marker: clone
	// writes clone-marker, migrate requires it and writes migrate-marker,
	// the app process requires migrate-marker to stay alive. A successful
	// Up therefore proves clone → migrate → workload ordering.
	bp.Seed.Migrate = "test -f clone-marker && echo migrated > migrate-marker"
	bp.Processes[0].Command = "test -f migrate-marker && sleep 60"

	deps, bm, _ := testDeps(t, bp)
	bm.cloneFunc = func(ctx context.Context, targetDB string) error {
		return os.WriteFile(worktreeFile(deps, "i1", "clone-marker"), []byte("x"), 0644)
	}
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if _, err := os.Stat(worktreeFile(deps, "i1", "migrate-marker")); err != nil {
		t.Errorf("migrate did not run after clone: %v", err)
	}
}

func TestInstance_Up_MigrationRunsOnceForMultipleDatabases(t *testing.T) {
	bp := testBlueprint()
	bp.Services["db"] = blueprint.ServiceDef{
		Isolation: blueprint.IsolationLogical,
		Type:      "postgres",
		Image:     "postgres:16",
		Databases: []blueprint.DatabaseDef{{Name: "test", From: "base"}},
	}
	bp.Seed.Migrate = `printf '%s\n' "$DATABASE_URL" > urls.out && printf '%s\n' "$DATABASE_TEST_URL" >> urls.out && echo ran >> runs.out`
	bp.Env.Holes = map[string]string{
		"PORT":              "{{PORT}}",
		"DATABASE_URL":      "postgres://localhost:5432/{{DB_NAME}}",
		"DATABASE_TEST_URL": "postgres://localhost:5432/{{DB_NAME_test}}",
	}

	deps, _, _ := testDeps(t, bp)
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	runs, err := os.ReadFile(worktreeFile(deps, "i1", "runs.out"))
	if err != nil {
		t.Fatalf("read runs.out: %v", err)
	}
	if got := strings.Count(string(runs), "ran"); got != 1 {
		t.Errorf("migrate ran %d times, want exactly once", got)
	}

	urls, err := os.ReadFile(worktreeFile(deps, "i1", "urls.out"))
	if err != nil {
		t.Fatalf("read urls.out: %v", err)
	}
	for _, want := range []string{"plax_i1", "plax_i1_test"} {
		if !strings.Contains(string(urls), want) {
			t.Errorf("migrate env missing %s:\n%s", want, urls)
		}
	}
}

func TestInstance_Up_MigrationFailureRollsBack(t *testing.T) {
	bp := testBlueprint()
	bp.Seed.Migrate = "echo fail-message; exit 1"

	deps, bm, drv := testDeps(t, bp)

	err := Up(context.Background(), deps, "i1", UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "fail-message") {
		t.Fatalf("Up = %v, want migration output in error", err)
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Errorf("error should name the migration step: %v", err)
	}

	assertNoResidue(t, deps, "i1")
	if got := bm.droppedDBs(); len(got) != 1 || got[0] != "plax_i1" {
		t.Errorf("rollback should drop the clone, dropped=%v", got)
	}
	if len(drv.removedNets) != 1 {
		t.Errorf("rollback should remove the network: %v", drv.removedNets)
	}
	if len(drv.started) > 0 {
		t.Errorf("workloads must not start after a migration failure: %v", drv.started)
	}
}

func TestInstance_Up_SkipMigrateDoesNotRunCommand(t *testing.T) {
	bp := testBlueprint()
	bp.Seed.Migrate = "echo ran > ran.out && exit 1"

	deps, _, _ := testDeps(t, bp)
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{Skip: map[string]bool{"migrate": true}}); err != nil {
		t.Fatalf("Up with skip migrate: %v", err)
	}
	if _, err := os.Stat(worktreeFile(deps, "i1", "ran.out")); !os.IsNotExist(err) {
		t.Errorf("migrate command ran despite --skip migrate")
	}
}

func TestInstance_Up_SkipVerifyOmitsVerification(t *testing.T) {
	bp := testBlueprint()
	deps, _, drv := testDeps(t, bp)
	// Without bound TCP listeners the verification TCP probe would fail;
	// skipping verification must not run it.
	drv.skipBindListeners = true
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{Skip: map[string]bool{"verify": true}}); err != nil {
		t.Fatalf("Up with skip verify: %v", err)
	}
	if _, found := deps.Registry.GetInstance("i1"); !found {
		t.Fatal("instance should be registered")
	}
}

func TestInstance_Up_SkipVerifyRetainsSettleCheck(t *testing.T) {
	bp := testBlueprint()
	bp.Processes[0].Command = "exit 1"

	deps, _, _ := testDeps(t, bp)

	err := Up(context.Background(), deps, "i1", UpOptions{Skip: map[string]bool{"verify": true}})
	if err == nil || !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("Up = %v, want immediate-exit error from the retained settle check", err)
	}
	assertNoResidue(t, deps, "i1")
}

func TestInstance_Up_UnknownSkipHasNoSideEffects(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())

	err := Up(context.Background(), deps, "i1", UpOptions{Skip: map[string]bool{"bogus": true}})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Up = %v, want unknown-step error", err)
	}

	assertNoResidue(t, deps, "i1")
	if len(bm.clonedDBs()) > 0 || len(drv.createdNets) > 0 {
		t.Errorf("side effects despite invalid skip: cloned=%v nets=%v", bm.clonedDBs(), drv.createdNets)
	}
}

func TestInstance_Up_MigrationCountReported_WhenConfigured(t *testing.T) {
	bp := testBlueprint()
	bp.Seed.AppliedMigrations = &blueprint.AppliedMigrations{Table: "migrations", Column: "name"}
	bp.Seed.Migrate = "true"

	deps, bm, _ := testDeps(t, bp)
	calls := 0
	bm.appliedFunc = func(ctx context.Context, dbName string) ([]string, error) {
		calls++
		if calls == 1 {
			return []string{"001"}, nil
		}
		return []string{"001", "002"}, nil
	}
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	stderr := captureStderr(t, func() error {
		return Up(context.Background(), deps, "i1", UpOptions{})
	})
	if !strings.Contains(stderr, "migrations: 1 applied") {
		t.Errorf("up output missing measured count:\n%s", stderr)
	}
	if calls != 2 {
		t.Errorf("applied-migration reads = %d, want 2 (before + after)", calls)
	}
}

func TestInstance_Up_MigrationRunsInSeedWorkdir(t *testing.T) {
	bp := testBlueprint()
	bp.Seed.Workdir = "app"
	bp.Seed.Migrate = "pwd > migrated-from.out"

	deps, _, _ := testDeps(t, bp)
	// The worktree is a git checkout, so the subdir must be committed to
	// exist there.
	appDir := filepath.Join(deps.RepoRoot, "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "placeholder"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "app"},
		{"git", "commit", "-m", "add app dir"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = deps.RepoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	t.Cleanup(func() { cleanupInstance(t, deps, "i1") })

	if err := Up(context.Background(), deps, "i1", UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	data, err := os.ReadFile(worktreeFile(deps, "i1", "app", "migrated-from.out"))
	if err != nil {
		t.Fatalf("read migrated-from.out: %v", err)
	}
	want := filepath.Join(deps.RepoRoot, worktree.WorktreeRelPath("i1"), "app")
	if strings.TrimSpace(string(data)) != want {
		t.Errorf("migrate ran in %q, want %q", strings.TrimSpace(string(data)), want)
	}
}

// captureStderr runs fn with os.Stderr piped and returns what was written.
func captureStderr(t *testing.T, fn func() error) string {
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
	if runErr != nil {
		t.Fatalf("captured call: %v", runErr)
	}
	if readErr != nil {
		t.Fatalf("read stderr: %v", readErr)
	}
	return string(data)
}
