package status

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	wtPath, err := worktree.Create(dir, "i1")
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

func TestBuild_AllOK(t *testing.T) {
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

func TestBuild_CodeDifferentBranch(t *testing.T) {
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

func TestBuild_NotFound(t *testing.T) {
	_, bp, reg, _ := initStatusRepo(t)
	deps := &Deps{Blueprint: bp, Registry: reg, CurrentStamp: registry.BlueprintStamp{}}
	_, err := Build(context.Background(), deps, "nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBuild_DataUnknown_NoBM(t *testing.T) {
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

func TestBuild_HostUnknown_Phase3Record(t *testing.T) {
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

func TestBuild_ConfigDrift(t *testing.T) {
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
