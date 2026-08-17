package status

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/worktree"
)

type fakeBM struct {
	info    postgres.BaseInfo
	prov    *postgres.ProvenanceRow
	provErr error
}

func (f *fakeBM) BaseStatus(context.Context) (postgres.BaseInfo, error) {
	return f.info, nil
}

func (f *fakeBM) InstanceProvenance(context.Context, string) (*postgres.ProvenanceRow, error) {
	if f.provErr != nil {
		return nil, f.provErr
	}
	return f.prov, nil
}

func initStatusRepo(t *testing.T) (repoRoot string, bp *blueprint.Blueprint, reg *registry.Registry, wtPath string) {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")

	files := map[string]string{
		".env.example":       "PORT=3000\n",
		".tool-versions":     "nodejs 22.19.0\n",
		"docker-compose.yml": "services: {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "init")

	wtPath, err := worktree.Create(dir, "i1", "")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	bp = &blueprint.Blueprint{
		Version:   1,
		Name:      "test",
		PortPool:  blueprint.PortPool{Start: 25000, End: 25100},
		Toolchain: ".tool-versions",
		Services:  map[string]blueprint.ServiceDef{},
		Processes: []blueprint.ProcessDef{},
		Env:       blueprint.EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}

	regPath := filepath.Join(dir, ".plax", "registry.json")
	reg, err = registry.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}

	stamp := hashStamp(dir, bp)
	reg.BlueprintStamp = stamp

	return dir, bp, reg, wtPath
}

func hashStamp(dir string, bp *blueprint.Blueprint) registry.BlueprintStamp {
	hashFile := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		h := sha256.Sum256(data)
		return fmt.Sprintf("%x", h[:])
	}
	return registry.BlueprintStamp{
		ComposeHash:    hashFile(filepath.Join(dir, "docker-compose.yml")),
		EnvExampleHash: hashFile(filepath.Join(dir, bp.Env.Template)),
		ToolchainHash:  hashFile(filepath.Join(dir, bp.Toolchain)),
	}
}

func TestStatus_BuildAllOK(t *testing.T) {
	dir, bp, reg, wtPath := initStatusRepo(t)

	bm := &fakeBM{
		info: postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 1},
		prov: &postgres.ProvenanceRow{Version: 1, SchemaHash: ""},
	}

	branch := worktree.BranchName("i1")
	rec := registry.InstanceRecord{
		ID:           "i1",
		Branch:       branch,
		WorktreePath: wtPath,
		State:        registry.StateRunning,
		DBName:       "plax_i1",
		BaseRef:      "main",
		Provenance: registry.Provenance{
			BaseVersion:  1,
			Toolchain:    "abc",
			ToolVersions: map[string]string{"nodejs": "v22.19.0"},
		},
	}
	if err := reg.AddInstance("i1", rec); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{
		Blueprint:    bp,
		Registry:     reg,
		BM:           bm,
		RepoRoot:     dir,
		CurrentStamp: reg.BlueprintStamp,
	}

	report, err := Build(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.State != string(registry.StateRunning) {
		t.Errorf("state = %s, want running", report.State)
	}
	if report.Code.Level != OK {
		t.Errorf("code = %s, want ok: %s", report.Code.Level, report.Code.Detail)
	}
	if report.Config.Level != OK {
		t.Errorf("config = %s, want ok: %s", report.Config.Level, report.Config.Detail)
	}
	if report.Host.Level == Unknown {
		t.Errorf("host = unknown, should have result: %s", report.Host.Detail)
	}
}

func TestStatus_BuildCodeDifferentBranch(t *testing.T) {
	dir, bp, reg, wtPath := initStatusRepo(t)

	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}

	run("git", "branch", "review/pr-123", "main")
	run("git", "-C", wtPath, "checkout", "review/pr-123")
	run("git", "-C", wtPath, "commit", "--allow-empty", "-m", "pr commit")

	bm := &fakeBM{
		info: postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 1},
		prov: &postgres.ProvenanceRow{Version: 1, SchemaHash: ""},
	}

	branch := worktree.BranchName("i1")
	rec := registry.InstanceRecord{
		ID:           "i1",
		Branch:       branch,
		WorktreePath: wtPath,
		State:        registry.StateRunning,
		DBName:       "plax_i1",
		BaseRef:      "main",
		Provenance: registry.Provenance{
			BaseVersion:  1,
			Toolchain:    "abc",
			ToolVersions: map[string]string{"nodejs": "v22.19.0"},
		},
	}
	if err := reg.AddInstance("i1", rec); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{
		Blueprint:    bp,
		Registry:     reg,
		BM:           bm,
		RepoRoot:     dir,
		CurrentStamp: reg.BlueprintStamp,
	}

	report, err := Build(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if report.Code.Level != Drift {
		t.Errorf("code = %s, want drift: %s", report.Code.Level, report.Code.Detail)
	}
	if want := "ahead 1, behind 0 (on review/pr-123)"; report.Code.Detail != want {
		t.Errorf("code detail = %q, want %q", report.Code.Detail, want)
	}
}

// makeSchemaRepo builds a repo with a migration directory, returning the root
// and the default branch name. mainMigrations are committed to main.
func makeSchemaRepo(t *testing.T, mainMigrations []string) (repoRoot, baseRef string) {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")

	migDir := filepath.Join(dir, "src", "db", "migrations")
	if err := os.MkdirAll(migDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, m := range mainMigrations {
		if err := os.WriteFile(filepath.Join(migDir, m), []byte("-- "+m+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "base migrations")

	return dir, "main"
}

// schemaRepoWorktree creates a worktree on a feature branch whose migrations
// match wantMigrations and returns its path. Files named in wantMigrations
// that are not on main are added; files on main that are absent from
// wantMigrations are removed.
func schemaRepoWorktree(t *testing.T, repoRoot string, wantMigrations []string) string {
	t.Helper()

	wtPath, err := worktree.Create(repoRoot, "schema", "")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	migDir := filepath.Join(repoRoot, "src", "db", "migrations")
	wtMigDir := filepath.Join(wtPath, "src", "db", "migrations")
	if err := os.MkdirAll(wtMigDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, m := range wantMigrations {
		if _, err := os.Stat(filepath.Join(migDir, m)); err == nil {
			continue // already present from main
		}
		if err := os.WriteFile(filepath.Join(wtMigDir, m), []byte("-- "+m+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Remove migrations that should not be in the branch.
	entries, err := os.ReadDir(wtMigDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !contains(wantMigrations, e.Name()) {
			if err := os.Remove(filepath.Join(wtMigDir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}

	wtRun := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	wtRun("git", "checkout", "-b", "feat/schema")
	wtRun("git", "add", ".")
	wtRun("git", "commit", "-m", "schema branch")
	return wtPath
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func schemaDriftResult(t *testing.T, repoRoot, baseRef string, prov *postgres.ProvenanceRow, wtPath string) Dimension {
	t.Helper()
	rec := &registry.InstanceRecord{
		ID:           "schema",
		Branch:       "feat/schema",
		WorktreePath: wtPath,
	}
	return schemaDrift(repoRoot, baseRef, "src/db/migrations", &fakeBM{}, rec, prov)
}

func TestStatus_SchemaDriftBaseAhead(t *testing.T) {
	repoRoot, base := makeSchemaRepo(t, []string{"0001_init.sql", "0002_add_users.sql"})
	wtPath := schemaRepoWorktree(t, repoRoot, []string{"0001_init.sql"})

	// Base (and hence the instance database, cloned from it) carries both
	// migrations; the branch predates the second one.
	prov := &postgres.ProvenanceRow{Version: 1, SchemaHash: postgres.HashMigrationNames([]string{"0001_init.sql", "0002_add_users.sql"})}

	d := schemaDriftResult(t, repoRoot, base, prov, wtPath)
	if d.Level != Drift {
		t.Fatalf("level = %s, want drift", d.Level)
	}
	for _, want := range []string{"database has 1 migration", "0002_add_users.sql", "worktree does not declare", "re-migrat"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q missing %q", d.Detail, want)
		}
	}
	if strings.Contains(d.Detail, "down' + 'plax up") {
		t.Errorf("detail %q wrongly recommends rebuild for a base-ahead drift", d.Detail)
	}
}

func TestStatus_SchemaDriftBranchAhead(t *testing.T) {
	repoRoot, base := makeSchemaRepo(t, []string{"0001_init.sql"})
	wtPath := schemaRepoWorktree(t, repoRoot, []string{"0001_init.sql", "0002_add_users.sql"})

	// The database, cloned from the base, predates the branch's new migration.
	prov := &postgres.ProvenanceRow{Version: 1, SchemaHash: postgres.HashMigrationNames([]string{"0001_init.sql"})}

	d := schemaDriftResult(t, repoRoot, base, prov, wtPath)
	if d.Level != Drift {
		t.Fatalf("level = %s, want drift", d.Level)
	}
	for _, want := range []string{"worktree declares 1 migration", "0002_add_users.sql", "re-migrate"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q missing %q", d.Detail, want)
		}
	}
}

func TestStatus_SchemaDriftDiverged(t *testing.T) {
	repoRoot, base := makeSchemaRepo(t, []string{"0001_init.sql", "0002_shared.sql"})
	wtPath := schemaRepoWorktree(t, repoRoot, []string{"0001_init.sql", "0003_branch.sql"})

	// Neither set is a subset of the other: the database carries 0002, the
	// branch carries 0003.
	prov := &postgres.ProvenanceRow{Version: 1, SchemaHash: postgres.HashMigrationNames([]string{"0001_init.sql", "0002_shared.sql"})}

	d := schemaDriftResult(t, repoRoot, base, prov, wtPath)
	if d.Level != Drift {
		t.Fatalf("level = %s, want drift", d.Level)
	}
	for _, want := range []string{"diverged", "0002_shared.sql", "0003_branch.sql"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q missing %q", d.Detail, want)
		}
	}
}

func TestStatus_BuildNotFound(t *testing.T) {
	_, bp, reg, _ := initStatusRepo(t)
	deps := &Deps{Blueprint: bp, Registry: reg, CurrentStamp: registry.BlueprintStamp{}}
	_, err := Build(context.Background(), deps, "nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestStatus_BuildDataUnknownNoBM(t *testing.T) {
	dir, bp, reg, wtPath := initStatusRepo(t)
	rec := registry.InstanceRecord{
		ID:           "i1",
		Branch:       "plax/i1",
		WorktreePath: wtPath,
		State:        registry.StateRunning,
		DBName:       "plax_i1",
		BaseRef:      "main",
		Provenance: registry.Provenance{
			BaseVersion:  1,
			ToolVersions: map[string]string{"nodejs": "v22.19.0"},
		},
	}
	if err := reg.AddInstance("i1", rec); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{
		Blueprint:    bp,
		Registry:     reg,
		RepoRoot:     dir,
		CurrentStamp: reg.BlueprintStamp,
	}

	report, err := Build(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.Data.Level != Unknown {
		t.Errorf("data = %s, want unknown: %s", report.Data.Level, report.Data.Detail)
	}
}

func TestStatus_BuildHostUnknownPhase3Record(t *testing.T) {
	dir, bp, reg, wtPath := initStatusRepo(t)
	rec := registry.InstanceRecord{
		ID:           "i1",
		Branch:       "plax/i1",
		WorktreePath: wtPath,
		State:        registry.StateRunning,
		DBName:       "plax_i1",
		BaseRef:      "main",
		Provenance:   registry.Provenance{BaseVersion: 1},
	}
	if err := reg.AddInstance("i1", rec); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{
		Blueprint:    bp,
		Registry:     reg,
		RepoRoot:     dir,
		CurrentStamp: reg.BlueprintStamp,
	}

	report, err := Build(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.Host.Level != Unknown {
		t.Errorf("host = %s, want unknown: %s", report.Host.Level, report.Host.Detail)
	}
}

func TestStatus_BuildConfigDrift(t *testing.T) {
	dir, bp, reg, wtPath := initStatusRepo(t)
	rec := registry.InstanceRecord{
		ID:           "i1",
		Branch:       "plax/i1",
		WorktreePath: wtPath,
		State:        registry.StateRunning,
		BaseRef:      "main",
		Provenance: registry.Provenance{
			BaseVersion:  1,
			ToolVersions: map[string]string{"nodejs": "v22.19.0"},
		},
	}
	if err := reg.AddInstance("i1", rec); err != nil {
		t.Fatal(err)
	}

	reg.BlueprintStamp = registry.BlueprintStamp{ComposeHash: "old"}

	currentStamp := hashStamp(dir, bp)
	deps := &Deps{
		Blueprint:    bp,
		Registry:     reg,
		RepoRoot:     dir,
		CurrentStamp: currentStamp,
	}

	report, err := Build(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.Config.Level != Drift {
		t.Errorf("config = %s, want drift: %s", report.Config.Level, report.Config.Detail)
	}
}
