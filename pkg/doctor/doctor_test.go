package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/registry"
)

type fakeBM struct {
	info    postgres.BaseInfo
	dbs     map[string]bool
	plaxDBs []string
	listErr error
}

func (f *fakeBM) BaseStatus(context.Context) (postgres.BaseInfo, error) {
	return f.info, nil
}

func (f *fakeBM) InstanceDBExists(_ context.Context, dbName string) (bool, error) {
	return f.dbs[dbName], nil
}

func (f *fakeBM) ListPlaxDatabases(_ context.Context) ([]string, error) {
	return f.plaxDBs, f.listErr
}

type fakeDocker struct {
	containers map[string]bool
	running    map[string]bool
	reachable  bool
}

func (f *fakeDocker) ServiceExists(_ context.Context, containerID string) (bool, error) {
	return f.containers[containerID], nil
}

func (f *fakeDocker) ServiceRunning(_ context.Context, containerID string) (bool, error) {
	return f.running[containerID], nil
}

func initDoctorRepo(t *testing.T) (string, *blueprint.Blueprint, *registry.Registry) {
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
		"docker-compose.yml": "services:\n  redis:\n    image: redis:7\n",
		".env.example":       "PORT=3000\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "init")

	bp := &blueprint.Blueprint{
		Version:   1,
		Name:      "test",
		PortPool:  blueprint.PortPool{Start: 25000, End: 25100},
		Toolchain: "",
		Services: map[string]blueprint.ServiceDef{
			"redis": {Isolation: blueprint.IsolationDedicated, Image: "redis:7", Ports: map[string]blueprint.PortDef{"6379": {Var: "REDIS_PORT"}}},
		},
		Processes: []blueprint.ProcessDef{
			{Name: "app", Isolation: blueprint.IsolationNative, Command: "sleep 60", Workdir: ".", PortVar: "PORT"},
		},
		Seed: blueprint.SeedConfig{Migrate: "echo", Command: "echo", Workdir: "."},
		Env:  blueprint.EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}

	regPath := filepath.Join(dir, ".plax", "registry.json")
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}

	return dir, bp, reg
}

func TestDoctor_AllPass(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bm := &fakeBM{
		info: postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 5},
		dbs:  map[string]bool{},
	}
	drv := &fakeDocker{reachable: true, containers: map[string]bool{}, running: map[string]bool{}}

	deps := &Deps{
		Blueprint: bp,
		Registry:  reg,
		BM:        bm,
		Docker:    drv,
		RepoRoot:  dir,
	}

	report := Run(context.Background(), deps)
	if report.Failed() {
		for _, c := range report.Checks {
			if c.Level == Fail {
				t.Errorf("unexpected failure: %s", c.Message)
			}
		}
	}
}

func TestDoctor_PortForUnknownInstance(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	reg.PortAllocations[3000] = registry.PortAllocation{Instance: "ghost", Service: "redis"}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Level == Fail && strings.Contains(c.Message, "ghost") {
			found = true
		}
	}
	if !found {
		t.Error("should report port allocated to unknown instance")
	}
}

func TestDoctor_ComposeServiceNotInBlueprint(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	composeContent := "services:\n  redis:\n    image: redis:7\n  postgres:\n    image: postgres:16\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeContent), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Level == Warn && strings.Contains(c.Message, "compose service") && strings.Contains(c.Message, "postgres") {
			found = true
		}
	}
	if !found {
		for _, c := range report.Checks {
			t.Logf("[%s] %s", c.Level, c.Message)
		}
		t.Error("should warn that compose service postgres is not in blueprint")
	}
}

func TestDoctor_BaseUnlocked(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	bm := &fakeBM{
		info: postgres.BaseInfo{Exists: true, Locked: false, ProvenanceVer: 5},
		dbs:  map[string]bool{},
	}

	deps := &Deps{Blueprint: bp, Registry: reg, BM: bm, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Level == Fail && strings.Contains(c.Message, "not locked") {
			found = true
		}
	}
	if !found {
		t.Error("should fail when base is not locked")
	}
}

func TestDoctor_BaseMissing(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	bm := &fakeBM{
		info: postgres.BaseInfo{Exists: false},
		dbs:  map[string]bool{},
	}

	deps := &Deps{Blueprint: bp, Registry: reg, BM: bm, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Level == Fail && strings.Contains(c.Message, "does not exist") {
			found = true
		}
	}
	if !found {
		t.Error("should fail when base is missing")
	}
}

func TestDoctor_NilBackends(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasDockerFail := false
	hasPgFail := false
	for _, c := range report.Checks {
		if c.Level == Fail && strings.Contains(c.Message, "docker") {
			hasDockerFail = true
		}
		if c.Level == Fail && strings.Contains(c.Message, "postgres") {
			hasPgFail = true
		}
	}
	if !hasDockerFail {
		t.Error("should report docker unreachable when nil")
	}
	if !hasPgFail {
		t.Error("should report postgres unreachable when nil")
	}
}

func TestDoctor_BaseNextStaged(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	bm := &fakeBM{
		info: postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 5, HasBaseNext: true},
		dbs:  map[string]bool{},
	}

	deps := &Deps{Blueprint: bp, Registry: reg, BM: bm, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Level == Warn && strings.Contains(c.Message, "plax_base_next") {
			found = true
		}
	}
	if !found {
		t.Error("should warn about staged base_next")
	}
}

func TestDoctor_OrphanDatabase(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	bm := &fakeBM{
		info:    postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 5},
		dbs:     map[string]bool{},
		plaxDBs: []string{"plax_orphan_test", "plax_i1"},
	}

	deps := &Deps{Blueprint: bp, Registry: reg, BM: bm, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Area == "orphan-databases" && strings.Contains(c.Message, "plax_orphan_test") {
			found = true
		}
	}
	if !found {
		for _, c := range report.Checks {
			t.Logf("[%s/%s] %s", c.Area, c.Level, c.Message)
		}
		t.Error("should report orphan database plax_orphan_test")
	}

	// plax_i1 should also be reported as orphan since no registry entry declares it.
	foundI1 := false
	for _, c := range report.Checks {
		if c.Area == "orphan-databases" && strings.Contains(c.Message, "plax_i1") {
			foundI1 = true
		}
	}
	if !foundI1 {
		t.Error("plax_i1 should be reported as orphan since no registry entry declares it")
	}
}

func TestDoctor_NoOrphans(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	_ = reg.AddInstance("i1", registry.InstanceRecord{
		DBName:  "plax_i1",
		DBNames: map[string]string{"": "plax_i1"},
	})

	bm := &fakeBM{
		info:    postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 5},
		dbs:     map[string]bool{},
		plaxDBs: []string{"plax_i1"},
	}

	deps := &Deps{Blueprint: bp, Registry: reg, BM: bm, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "orphan-databases" {
			if c.Level == Pass {
				hasPass = true
			} else {
				t.Errorf("unexpected check: [%s] %s", c.Level, c.Message)
			}
		}
	}
	if !hasPass {
		t.Error("expected pass for no orphans")
	}
}

func TestDoctor_OldRecord_Fallback(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)
	_ = dir

	_ = reg.AddInstance("i1", registry.InstanceRecord{
		DBName: "plax_i1_old",
	})

	bm := &fakeBM{
		info:    postgres.BaseInfo{Exists: true, Locked: true, ProvenanceVer: 5},
		dbs:     map[string]bool{},
		plaxDBs: []string{"plax_i1_old"},
	}

	deps := &Deps{Blueprint: bp, Registry: reg, BM: bm, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "orphan-databases" {
			if c.Level == Pass {
				hasPass = true
			}
		}
	}
	if !hasPass {
		for _, c := range report.Checks {
			t.Logf("[%s/%s] %s", c.Area, c.Level, c.Message)
		}
		t.Error("expected pass for old-format record with DBName matching server DB")
	}
}

func TestDoctor_UserEnvKeysMissingFromTemplate_Warns(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("NEXTAUTH_SECRET=real-secret\nANTHROPIC_KEY=sk-ant\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	var warn string
	for _, c := range report.Checks {
		if c.Area == "user-env-vs-template" && c.Level == Warn {
			warn = c.Message
		}
	}
	if warn == "" {
		t.Fatal("expected a warn-level check for user-env-vs-template")
	}
	if !strings.Contains(warn, "NEXTAUTH_SECRET") {
		t.Error("should warn about NEXTAUTH_SECRET missing from template")
	}
	if !strings.Contains(warn, "ANTHROPIC_KEY") {
		t.Error("should warn about ANTHROPIC_KEY missing from template")
	}
}

func TestDoctor_UserEnvKeysMissingFromTemplate_Truncation(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	var lines []byte
	for i := range 15 {
		lines = append(lines, []byte(fmt.Sprintf("KEY_%02d=value%d\n", i, i))...)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), lines, 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	var warn string
	for _, c := range report.Checks {
		if c.Area == "user-env-vs-template" && c.Level == Warn {
			warn = c.Message
		}
	}
	if warn == "" {
		t.Fatal("expected a warn-level check for user-env-vs-template")
	}
	if !strings.Contains(warn, "(+5 more)") {
		t.Errorf("expected truncation suffix (+5 more), got: %s", warn)
	}
	if !strings.Contains(warn, "\"KEY_09\"") {
		t.Errorf("expected KEY_09 in displayed keys, got: %s", warn)
	}
	if strings.Contains(warn, "\"KEY_10\"") {
		t.Errorf("KEY_10 should be truncated, got: %s", warn)
	}
}

func TestDoctor_UserEnvKeysAllInTemplate_Passes(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("PORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "user-env-vs-template" {
			if c.Level == Pass {
				hasPass = true
			} else {
				t.Errorf("unexpected check: [%s] %s", c.Level, c.Message)
			}
		}
	}
	if !hasPass {
		t.Error("expected pass when user env keys match template")
	}
}

func TestDoctor_UserEnvNoFile_Skips(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "user-env-vs-template" {
			if c.Level == Pass {
				hasPass = true
			}
		}
	}
	if !hasPass {
		t.Error("expected pass when user .env does not exist")
	}
}

func TestDoctor_UserEnvHoleKeysExcluded(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bp.Env.Holes = map[string]string{"REDIS_PORT": "{{REDIS_PORT}}"}

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("REDIS_PORT=9999\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "user-env-vs-template" && c.Level == Warn {
			t.Errorf("should not warn about hole key REDIS_PORT: %s", c.Message)
		}
		if c.Area == "user-env-vs-template" && c.Level == Pass {
			hasPass = true
		}
	}
	if !hasPass {
		t.Error("expected a pass-level check for user-env-vs-template area")
	}
}

func TestDoctor_ScrubbedKeyHasRealValue_Warns(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bp.Env.Scrub = []string{"SENDGRID_API_KEY"}
	secret := "SG.real-secret-value"
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("SENDGRID_API_KEY=placeholder\nPORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SENDGRID_API_KEY="+secret+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	found := false
	for _, c := range report.Checks {
		if c.Area == "scrubbed-env-keys" && c.Level == Warn && strings.Contains(c.Message, "SENDGRID_API_KEY") {
			found = true
			if strings.Contains(c.Message, secret) {
				t.Errorf("warn message must not contain the secret value; got: %s", c.Message)
			}
		}
	}
	if !found {
		t.Error("should warn about scrubbed key with real value")
	}
}

func TestDoctor_ScrubbedKeyMatchesTemplate_Passes(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bp.Env.Scrub = []string{"SENDGRID_API_KEY"}
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("SENDGRID_API_KEY=placeholder\nPORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SENDGRID_API_KEY=placeholder\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "scrubbed-env-keys" {
			if c.Level == Pass {
				hasPass = true
			} else {
				t.Errorf("unexpected check: [%s] %s", c.Level, c.Message)
			}
		}
	}
	if !hasPass {
		t.Error("expected pass when scrubbed key matches template")
	}
}

func TestDoctor_ScrubbedKeyEmptyInBoth_Passes(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bp.Env.Scrub = []string{"SENDGRID_API_KEY"}
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("SENDGRID_API_KEY=\nPORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SENDGRID_API_KEY=\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "scrubbed-env-keys" {
			if c.Level == Pass {
				hasPass = true
			} else {
				t.Errorf("unexpected check: [%s] %s", c.Level, c.Message)
			}
		}
	}
	if !hasPass {
		t.Error("expected pass when both empty")
	}
}

func TestDoctor_ScrubbedKeyNoUserEnv_Skips(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bp.Env.Scrub = []string{"SENDGRID_API_KEY"}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "scrubbed-env-keys" {
			if c.Level == Pass {
				hasPass = true
			}
		}
	}
	if !hasPass {
		t.Error("expected pass when user .env does not exist")
	}
}

func TestDoctor_ScrubbedKeyHoleExcluded(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	bp.Env.Scrub = []string{"REDIS_PORT"}
	bp.Env.Holes = map[string]string{"REDIS_PORT": "{{REDIS_PORT}}"}
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("PORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("REDIS_PORT=9999\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	for _, c := range report.Checks {
		if c.Area == "scrubbed-env-keys" && c.Level == Warn {
			t.Errorf("should not warn about hole key REDIS_PORT: %s", c.Message)
		}
	}
}

func TestDoctor_NoScrubList_Passes(t *testing.T) {
	dir, bp, reg := initDoctorRepo(t)

	deps := &Deps{Blueprint: bp, Registry: reg, RepoRoot: dir}
	report := Run(context.Background(), deps)

	hasPass := false
	for _, c := range report.Checks {
		if c.Area == "scrubbed-env-keys" {
			if c.Level == Pass {
				hasPass = true
			} else {
				t.Errorf("unexpected check: [%s] %s", c.Level, c.Message)
			}
		}
	}
	if !hasPass {
		t.Error("expected pass when no scrub list")
	}
}

var _ = strings.TrimSpace // keep import alive
