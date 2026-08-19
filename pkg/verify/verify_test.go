package verify

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/registry"
)

type fakeBM struct {
	mu          sync.Mutex
	exists      map[string]bool
	provenances map[string]*postgres.ProvenanceRow
}

func (f *fakeBM) InstanceDBExists(_ context.Context, dbName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists != nil {
		return f.exists[dbName], nil
	}
	return true, nil
}

func (f *fakeBM) InstanceProvenance(_ context.Context, dbName string) (*postgres.ProvenanceRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provenances != nil {
		return f.provenances[dbName], nil
	}
	return &postgres.ProvenanceRow{Version: 1, Source: dbName, SchemaHash: "abc"}, nil
}

func TestVerify_EnvCompleteness_AllKeysPresent(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")

	writeFile(t, tmpl, "PORT=3000\nDB_NAME=test\n")
	writeFile(t, userEnv, "SECRET=abc\n")
	writeFile(t, derived, "PORT=3000\nDB_NAME=plax_i1\nSECRET=abc\n")

	results := CheckEnv(tmpl, userEnv, derived, nil, nil)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected failure: %s: %s", r.Check, r.Detail)
		}
	}
}

func TestVerify_EnvCompleteness_MissingKey(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")

	writeFile(t, tmpl, "PORT=3000\nDB_NAME=test\n")
	writeFile(t, userEnv, "SECRET=abc\n")
	writeFile(t, derived, "PORT=3000\n")

	results := CheckEnv(tmpl, userEnv, derived, nil, nil)
	found := false
	for _, r := range results {
		if r.Check == "env-completeness" && !r.Passed && r.Artifact == "DB_NAME" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected env-completeness failure for DB_NAME")
	}
}

func TestVerify_EnvCompleteness_HoleKeyExpected(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")

	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, userEnv, "")
	writeFile(t, derived, "PORT=3000\n")

	holes := map[string]string{"DB_NAME": "plax_i1"}
	results := CheckEnv(tmpl, userEnv, derived, holes, nil)
	found := false
	for _, r := range results {
		if r.Check == "env-completeness" && !r.Passed && r.Artifact == "DB_NAME" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected env-completeness failure for hole key DB_NAME")
	}
}

func TestVerify_EnvCompleteness_ScrubbedKeyExcluded(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")

	writeFile(t, tmpl, "PORT=3000\nSECRET=sensitive\n")
	writeFile(t, userEnv, "SECRET=supersecret\n")
	writeFile(t, derived, "PORT=3000\n")

	scrub := map[string]bool{"SECRET": true}
	results := CheckEnv(tmpl, userEnv, derived, nil, scrub)
	for _, r := range results {
		if r.Check == "env-completeness" && !r.Passed {
			t.Errorf("unexpected completeness failure: %s", r.Detail)
		}
	}
}

func TestVerify_EnvNoUnresolved_Clean(t *testing.T) {
	dir := t.TempDir()
	derived := filepath.Join(dir, "derived.env")
	writeFile(t, derived, "PORT=3000\nDB_NAME=plax_i1\n")

	results := checkEnvNoUnresolved(derived)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected failure: %s: %s", r.Check, r.Detail)
		}
	}
}

func TestVerify_EnvNoUnresolved_HoleLeftBehind(t *testing.T) {
	dir := t.TempDir()
	derived := filepath.Join(dir, "derived.env")
	writeFile(t, derived, "PORT={{PORT}}\nDB_NAME=plax_i1\n")

	results := checkEnvNoUnresolved(derived)
	found := false
	for _, r := range results {
		if r.Check == "env-unresolved-holes" && !r.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected env-unresolved-holes failure")
	}
}

func TestVerify_EnvNoUnresolved_CommentExcluded(t *testing.T) {
	dir := t.TempDir()
	derived := filepath.Join(dir, "derived.env")
	writeFile(t, derived, "# {{this_is_fine}}\nPORT=3000\n")

	results := checkEnvNoUnresolved(derived)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected failure: %s: %s", r.Check, r.Detail)
		}
	}
}

func TestVerify_EnvNoScrubbedLeaks_NoLeak(t *testing.T) {
	dir := t.TempDir()
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")
	tmpl := filepath.Join(dir, ".env.example")

	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, userEnv, "SECRET=supersecret\n")
	writeFile(t, derived, "PORT=3000\n")

	scrub := map[string]bool{"SECRET": true}
	results := checkEnvNoScrubbedLeaks(userEnv, derived, scrub, tmpl)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected leak detection: %s", r.Detail)
		}
	}
}

func TestVerify_EnvNoScrubbedLeaks_LeakDetected(t *testing.T) {
	dir := t.TempDir()
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")
	tmpl := filepath.Join(dir, ".env.example")

	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, userEnv, "API_KEY=supersecret\n")
	writeFile(t, derived, "PORT=3000\nAPI_KEY=supersecret\n")

	scrub := map[string]bool{"API_KEY": true}
	results := checkEnvNoScrubbedLeaks(userEnv, derived, scrub, tmpl)
	found := false
	for _, r := range results {
		if r.Check == "env-scrubbed-leaks" && !r.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected env-scrubbed-leaks failure")
	}
}

func TestVerify_EnvNoScrubbedLeaks_PlaceholderValue_Ignored(t *testing.T) {
	dir := t.TempDir()
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")
	tmpl := filepath.Join(dir, ".env.example")

	writeFile(t, tmpl, "API_KEY=placeholder\n")
	writeFile(t, userEnv, "API_KEY=placeholder\n")
	writeFile(t, derived, "API_KEY=placeholder\n")

	scrub := map[string]bool{"API_KEY": true}
	results := checkEnvNoScrubbedLeaks(userEnv, derived, scrub, tmpl)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected leak detection for placeholder value: %s", r.Detail)
		}
	}
}

// listen opens a listener on a random loopback port and returns that port,
// closing the listener when the test finishes. It lets CheckServices probe a
// genuinely reachable endpoint.
func listen(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

func TestVerify_Services_TCPReachable(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	port := listen(t)

	services := map[string]blueprint.ServiceDef{
		"redis": {Isolation: blueprint.IsolationDedicated, Ports: map[string]blueprint.PortDef{"6379": {Var: "REDIS_PORT"}}},
	}
	allocated := map[string]int{"REDIS_PORT": port}

	results := CheckServices(context.Background(), services, nil, allocated)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected failure: %s: %s", r.Check, r.Detail)
		}
	}
}

func TestVerify_Services_TCPUnreachable(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	services := map[string]blueprint.ServiceDef{
		"redis": {Isolation: blueprint.IsolationDedicated, Ports: map[string]blueprint.PortDef{"6379": {Var: "REDIS_PORT"}}},
	}
	allocated := map[string]int{"REDIS_PORT": 19999}

	results := CheckServices(context.Background(), services, nil, allocated)
	found := false
	for _, r := range results {
		if r.Check == "tcp-reachability" && !r.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected tcp-reachability failure")
	}
}

func TestVerify_Services_NoDedicatedServices(t *testing.T) {
	services := map[string]blueprint.ServiceDef{
		"db": {Isolation: blueprint.IsolationLogical, Type: "postgres"},
	}
	results := CheckServices(context.Background(), services, nil, nil)
	if len(results) > 0 {
		t.Errorf("expected no results for logical services, got %v", results)
	}
}

func TestVerify_Services_ProcessPortReachable(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	port := listen(t)

	processes := []blueprint.ProcessDef{{Name: "app", PortVar: "PORT"}}
	allocated := map[string]int{"PORT": port}

	results := CheckServices(context.Background(), nil, processes, allocated)
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected one passing result, got %v", results)
	}
}

func TestVerify_Services_ProcessPortUnreachable(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	processes := []blueprint.ProcessDef{{Name: "app", PortVar: "PORT"}}
	allocated := map[string]int{"PORT": 19999}

	results := CheckServices(context.Background(), nil, processes, allocated)
	found := false
	for _, r := range results {
		if r.Check == "tcp-reachability" && !r.Passed && r.Artifact == "127.0.0.1:19999" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected tcp-reachability failure for process port")
	}
}

func TestVerify_Services_MixedServiceAndProcess(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	svcPort := listen(t)
	procPort := listen(t)

	services := map[string]blueprint.ServiceDef{
		"redis": {Isolation: blueprint.IsolationDedicated, Ports: map[string]blueprint.PortDef{"6379": {Var: "REDIS_PORT"}}},
	}
	processes := []blueprint.ProcessDef{{Name: "app", PortVar: "PORT"}}
	allocated := map[string]int{"REDIS_PORT": svcPort, "PORT": procPort}

	results := CheckServices(context.Background(), services, processes, allocated)
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected one passing result covering both endpoints, got %v", results)
	}
	if !strings.Contains(results[0].Detail, "2 endpoints") {
		t.Errorf("expected both endpoints counted, detail = %q", results[0].Detail)
	}
}

func TestVerify_Services_UnallocatedPortSkipped(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	services := map[string]blueprint.ServiceDef{
		"redis": {Isolation: blueprint.IsolationDedicated, Ports: map[string]blueprint.PortDef{"6379": {Var: "REDIS_PORT"}}},
	}
	results := CheckServices(context.Background(), services, nil, nil)
	if len(results) > 0 {
		t.Errorf("expected no results for unallocated ports, got %v", results)
	}
}

func TestVerify_Services_CtxCancelReturnsFailures(t *testing.T) {
	CheckDeadline = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processes := []blueprint.ProcessDef{{Name: "app", PortVar: "PORT"}}
	allocated := map[string]int{"PORT": 19999}

	results := CheckServices(ctx, nil, processes, allocated)
	if len(results) == 0 || results[0].Passed {
		t.Errorf("expected failure result on cancelled context, got %v", results)
	}
}

func TestVerify_Processes_AllAlive(t *testing.T) {
	pids := map[string]int{"app": 999999999}
	results := CheckProcesses(pids, func(int) bool { return true })
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected failure: %s", r.Detail)
		}
	}
}

func TestVerify_Processes_SomeDead(t *testing.T) {
	pids := map[string]int{"app": 1, "worker": 2}
	results := CheckProcesses(pids, func(pgid int) bool { return pgid == 1 })
	found := false
	for _, r := range results {
		if r.Check == "process-liveness" && !r.Passed && r.Artifact == "worker" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected process-liveness failure for worker")
	}
}

func TestVerify_Processes_NoProcesses(t *testing.T) {
	results := CheckProcesses(map[string]int{}, func(int) bool { return true })
	if len(results) > 0 {
		t.Errorf("expected no results for empty PIDs, got %v", results)
	}
}

func TestVerify_Databases_AllExist_WithProvenance(t *testing.T) {
	bm := &fakeBM{}
	results := CheckDatabases(context.Background(), []string{"plax_i1"}, bm)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("unexpected failure: %s: %s", r.Check, r.Detail)
		}
	}
}

func TestVerify_Databases_MissingDB(t *testing.T) {
	bm := &fakeBM{exists: map[string]bool{"plax_i1": false}}
	results := CheckDatabases(context.Background(), []string{"plax_i1"}, bm)
	found := false
	for _, r := range results {
		if r.Check == "db-existence" && !r.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected db-existence failure")
	}
}

func TestVerify_Databases_MissingProvenance(t *testing.T) {
	bm := &fakeBM{
		exists:      map[string]bool{"plax_i1": true},
		provenances: map[string]*postgres.ProvenanceRow{"plax_i1": nil},
	}
	results := CheckDatabases(context.Background(), []string{"plax_i1"}, bm)
	found := false
	for _, r := range results {
		if r.Check == "db-provenance" && !r.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected db-provenance failure")
	}
}

func TestVerify_RunVerify_AggregatesResults(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	envPath := filepath.Join(dir, ".env")
	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, envPath, "PORT=3000\n")

	reg := openTestRegistry(t, dir)
	_ = reg.AddInstance("i1", registry.InstanceRecord{
		State:        registry.StateRunning,
		WorktreePath: dir,
		Ports:        map[string]int{},
		PIDs:         map[string]int{},
	})
	_ = reg.Save()

	deps := &Deps{
		Blueprint: &blueprint.Blueprint{Env: blueprint.EnvConfig{Template: filepath.Base(tmpl)}},
		Registry:  reg,
		BM:        &fakeBM{},
		RepoRoot:  dir,
	}

	results, err := RunVerify(context.Background(), deps, "i1")
	if err != nil {
		var vErr *VerificationError
		if !asVerificationError(err, &vErr) {
			t.Fatalf("RunVerify: %v", err)
		}
	}
	_ = results

	rec, _ := reg.GetInstance("i1")
	if rec.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", rec.Health)
	}
	if rec.VerifiedAt == nil {
		t.Error("VerifiedAt should be set")
	}
}

func TestVerify_RunVerify_RuntimeChecksGatesTCPProbe(t *testing.T) {
	CheckDeadline = 200 * time.Millisecond
	bp := &blueprint.Blueprint{
		Env:       blueprint.EnvConfig{Template: ".env.example"},
		Processes: []blueprint.ProcessDef{{Name: "app", PortVar: "PORT"}},
	}

	run := func(runtime bool) []CheckResult {
		runDir := t.TempDir()
		writeFile(t, filepath.Join(runDir, ".env.example"), "PORT=3000\n")
		writeFile(t, filepath.Join(runDir, ".env"), "PORT=3000\n")
		reg := openTestRegistry(t, runDir)
		_ = reg.AddInstance("i1", registry.InstanceRecord{
			State:        registry.StateRunning,
			WorktreePath: runDir,
			Ports:        map[string]int{"PORT": 19999},
			PIDs:         map[string]int{"app": 999999999},
		})
		_ = reg.Save()
		t.Cleanup(reg.Close)
		deps := &Deps{
			Blueprint:     bp,
			Registry:      reg,
			BM:            &fakeBM{},
			RepoRoot:      runDir,
			RuntimeChecks: runtime,
		}
		results, _ := RunVerify(context.Background(), deps, "i1")
		return results
	}

	for _, r := range run(false) {
		if r.Check == "tcp-reachability" {
			t.Fatalf("tcp probe should be skipped when RuntimeChecks is false: %v", r)
		}
	}
	found := false
	for _, r := range run(true) {
		if r.Check == "tcp-reachability" && !r.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected tcp-reachability failure when RuntimeChecks is true")
	}
}

func TestVerify_RunVerify_SkipsRuntimeChecks_WhenSuspended(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	envPath := filepath.Join(dir, ".env")
	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, envPath, "PORT=3000\n")

	reg := openTestRegistry(t, dir)
	_ = reg.AddInstance("i1", registry.InstanceRecord{
		State:        registry.StateSuspended,
		WorktreePath: dir,
		Ports:        map[string]int{},
		PIDs:         map[string]int{},
	})
	_ = reg.Save()

	deps := &Deps{
		Blueprint: &blueprint.Blueprint{Env: blueprint.EnvConfig{Template: filepath.Base(tmpl)}},
		Registry:  reg,
		BM:        &fakeBM{},
		RepoRoot:  dir,
	}

	results, err := RunVerify(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("RunVerify: %v", err)
	}

	for _, r := range results {
		if r.Check == "tcp-reachability" || r.Check == "process-liveness" {
			t.Errorf("runtime check should be skipped for suspended instance: %s", r.Check)
		}
	}

	rec, _ := reg.GetInstance("i1")
	if rec.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy (suspended should not be marked unhealthy)", rec.Health)
	}
}

func TestVerify_RunVerify_SkipsDBChecks_WhenBMNil(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	envPath := filepath.Join(dir, ".env")
	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, envPath, "PORT=3000\n")

	reg := openTestRegistry(t, dir)
	_ = reg.AddInstance("i1", registry.InstanceRecord{
		State:        registry.StateRunning,
		WorktreePath: dir,
		Ports:        map[string]int{},
		PIDs:         map[string]int{},
	})
	_ = reg.Save()

	deps := &Deps{
		Blueprint: &blueprint.Blueprint{Env: blueprint.EnvConfig{Template: filepath.Base(tmpl)}},
		Registry:  reg,
		BM:        nil,
		RepoRoot:  dir,
	}

	results, err := RunVerify(context.Background(), deps, "i1")
	if err != nil {
		t.Fatalf("RunVerify: %v", err)
	}

	for _, r := range results {
		if r.Check == "db-existence" || r.Check == "db-provenance" {
			t.Errorf("DB checks should be skipped when BM is nil: %s", r.Check)
		}
	}
}

func TestVerify_RunVerify_InstanceNotFound(t *testing.T) {
	dir := t.TempDir()
	reg := openTestRegistry(t, dir)
	deps := &Deps{
		Blueprint: &blueprint.Blueprint{},
		Registry:  reg,
		RepoRoot:  dir,
	}
	_, err := RunVerify(context.Background(), deps, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func openTestRegistry(t *testing.T, dir string) *registry.Registry {
	t.Helper()
	r, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

func asVerificationError(err error, target **VerificationError) bool {
	if err == nil {
		return false
	}
	var v *VerificationError
	if !errors.As(err, &v) {
		return false
	}
	*target = v
	return true
}

func TestVerify_EnvCompleteness_DerivedEnvMissing(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	userEnv := filepath.Join(dir, ".env")
	derived := filepath.Join(dir, "derived.env")

	writeFile(t, tmpl, "PORT=3000\n")
	writeFile(t, userEnv, "")

	results := CheckEnv(tmpl, userEnv, derived, nil, nil)
	found := false
	for _, r := range results {
		if r.Check == "env-completeness" && !r.Passed && strings.Contains(r.Detail, "not found") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected env-completeness failure for missing derived .env")
	}
}

func TestVerify_VerificationError_Format(t *testing.T) {
	err := &VerificationError{
		Results: []CheckResult{
			{Check: "env-completeness", Passed: false, Artifact: "SENDGRID_API_KEY"},
			{Check: "tcp-reachability", Passed: false, Artifact: "127.0.0.1:26380"},
		},
		Layer: 1,
	}
	want := "verification failed: env-completeness (SENDGRID_API_KEY), tcp-reachability (127.0.0.1:26380)"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// depIsolationDirs returns a repo root (with a shared node_modules) and a
// worktree path inside it, both empty of manifests.
func depIsolationDirs(t *testing.T) (repoRoot, wt string) {
	t.Helper()
	repoRoot = t.TempDir()
	wt = filepath.Join(repoRoot, ".plax", "worktrees", "i1")
	if err := os.MkdirAll(filepath.Join(repoRoot, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	return repoRoot, wt
}

func findDepResult(results []CheckResult) *CheckResult {
	for i := range results {
		if results[i].Check == "dependency-isolation" {
			return &results[i]
		}
	}
	return nil
}

func TestVerify_DependencyIsolation_SharedTree_ManifestsMatch(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	writeFile(t, filepath.Join(repoRoot, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(repoRoot, "package-lock.json"), "lock1")
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(wt, "package-lock.json"), "lock1")

	results := CheckDependencyIsolation(repoRoot, wt)
	r := findDepResult(results)
	if r == nil {
		t.Fatalf("no dependency-isolation result in %v", results)
	}
	if !r.Passed {
		t.Errorf("expected pass, got %+v", r)
	}
	if len(results) != 1 {
		t.Errorf("results = %v, want exactly 1", results)
	}
	if r.Layer != 1 {
		t.Errorf("Layer = %d, want 1", r.Layer)
	}
}

func TestVerify_DependencyIsolation_SharedTree_ManifestDiffers(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	writeFile(t, filepath.Join(repoRoot, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(repoRoot, "package-lock.json"), "lock1")
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"2"}`)
	writeFile(t, filepath.Join(wt, "package-lock.json"), "lock1")

	results := CheckDependencyIsolation(repoRoot, wt)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly 1 failure", results)
	}
	if results[0].Passed || results[0].Artifact != "package.json" {
		t.Errorf("unexpected result: %+v", results[0])
	}
}

func TestVerify_DependencyIsolation_SharedTree_LockfileDiffers(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	writeFile(t, filepath.Join(repoRoot, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(repoRoot, "package-lock.json"), "lock1")
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(wt, "package-lock.json"), "lock2")

	results := CheckDependencyIsolation(repoRoot, wt)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly 1 failure", results)
	}
	if results[0].Passed || results[0].Artifact != "package-lock.json" {
		t.Errorf("unexpected result: %+v", results[0])
	}
}

func TestVerify_DependencyIsolation_LocalNodeModules_Silent(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	writeFile(t, filepath.Join(repoRoot, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"2"}`)
	if err := os.MkdirAll(filepath.Join(wt, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}

	if results := CheckDependencyIsolation(repoRoot, wt); len(results) != 0 {
		t.Errorf("results = %v, want none (own node_modules installed)", results)
	}
}

func TestVerify_DependencyIsolation_NoManifests_NoResults(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	if results := CheckDependencyIsolation(repoRoot, wt); len(results) != 0 {
		t.Errorf("results = %v, want none (nothing declared)", results)
	}
}

func TestVerify_DependencyIsolation_ParentManifestMissing_Fails(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"1"}`)

	results := CheckDependencyIsolation(repoRoot, wt)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly 1 failure", results)
	}
	if results[0].Passed || results[0].Artifact != "package.json" {
		t.Errorf("unexpected result: %+v", results[0])
	}
}

func TestVerify_DependencyIsolation_UnreadableManifest_Fails(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	manifest := filepath.Join(wt, "package.json")
	writeFile(t, manifest, `{"a":"1"}`)
	if err := os.Chmod(manifest, 0000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(manifest, 0644) }()

	results := CheckDependencyIsolation(repoRoot, wt)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly 1 failure", results)
	}
	if results[0].Passed || results[0].Artifact != "package.json" {
		t.Errorf("unexpected result: %+v", results[0])
	}
}

func TestVerify_DependencyIsolation_UnreadableParentManifest_Fails(t *testing.T) {
	repoRoot, wt := depIsolationDirs(t)
	parentManifest := filepath.Join(repoRoot, "package.json")
	writeFile(t, parentManifest, `{"a":"1"}`)
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"1"}`)
	if err := os.Chmod(parentManifest, 0000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(parentManifest, 0644) }()

	results := CheckDependencyIsolation(repoRoot, wt)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly 1 failure", results)
	}
	if results[0].Passed || results[0].Artifact != "package.json" {
		t.Errorf("unexpected result: %+v", results[0])
	}
}

func TestVerify_DependencyIsolation_NoSharedTree_NoResults(t *testing.T) {
	repoRoot := t.TempDir()
	wt := filepath.Join(repoRoot, ".plax", "worktrees", "i1")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoRoot, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"2"}`)

	if results := CheckDependencyIsolation(repoRoot, wt); len(results) != 0 {
		t.Errorf("results = %v, want none (no shared tree)", results)
	}
}

func TestVerify_RunVerify_IncludesDependencyCheck(t *testing.T) {
	repoRoot := t.TempDir()
	wt := filepath.Join(repoRoot, ".plax", "worktrees", "i1")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoRoot, ".env.example"), "PORT=3000\n")
	writeFile(t, filepath.Join(repoRoot, ".env"), "")
	writeFile(t, filepath.Join(wt, ".env"), "PORT=3000\n")
	writeFile(t, filepath.Join(repoRoot, "package.json"), `{"a":"1"}`)
	writeFile(t, filepath.Join(wt, "package.json"), `{"a":"2"}`)

	reg := openTestRegistry(t, repoRoot)
	_ = reg.AddInstance("i1", registry.InstanceRecord{
		State:        registry.StateRunning,
		WorktreePath: wt,
		Ports:        map[string]int{},
		PIDs:         map[string]int{},
	})
	_ = reg.Save()

	deps := &Deps{
		Blueprint: &blueprint.Blueprint{Env: blueprint.EnvConfig{Template: ".env.example"}},
		Registry:  reg,
		BM:        nil,
		RepoRoot:  repoRoot,
	}

	_, err := RunVerify(context.Background(), deps, "i1")
	var vErr *VerificationError
	if !asVerificationError(err, &vErr) {
		t.Fatalf("RunVerify: %v, want *VerificationError", err)
	}
	r := findDepResult(vErr.Results)
	if r == nil || r.Passed {
		t.Errorf("expected failed dependency-isolation in %v", vErr.Results)
	}

	rec, _ := reg.GetInstance("i1")
	if rec.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy", rec.Health)
	}
}

func TestVerify_RunVerify_HealthUnhealthyPersisted(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, ".env.example")
	envPath := filepath.Join(dir, ".env")
	writeFile(t, tmpl, "PORT=3000\nREQUIRED_KEY=val\n")
	writeFile(t, envPath, "PORT=3000\n")

	reg := openTestRegistry(t, dir)
	_ = reg.AddInstance("i1", registry.InstanceRecord{
		State:        registry.StateRunning,
		WorktreePath: dir,
		Ports:        map[string]int{},
		PIDs:         map[string]int{},
	})
	_ = reg.Save()

	deps := &Deps{
		Blueprint: &blueprint.Blueprint{Env: blueprint.EnvConfig{Template: filepath.Base(tmpl)}},
		Registry:  reg,
		BM:        nil,
		RepoRoot:  dir,
	}

	_, err := RunVerify(context.Background(), deps, "i1")
	if err == nil {
		t.Fatal("expected verification error")
	}

	rec, _ := reg.GetInstance("i1")
	if rec.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy", rec.Health)
	}
	if rec.VerifiedAt == nil {
		t.Error("VerifiedAt should be set even on failure")
	}
}
