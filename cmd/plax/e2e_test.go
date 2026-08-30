package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/testutil"
	"github.com/apollopower/plax/pkg/worktree"
	"github.com/jackc/pgx/v5"
)

// TestEndToEnd_TwoInstances exercises the built binary against real git,
// Postgres, and Docker: base reset, two parallel instances, HTTP checks,
// exec environment, failure rollback, and clean teardown. Skipped unless
// PLAX_TEST_POSTGRES_URL is set and the other tools are available.
func TestEndToEnd_TwoInstances(t *testing.T) {
	pgURL := e2ePrereqs(t)
	bin := buildPlax(t)
	repo := initFixtureRepo(t)

	suffix := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "#", "_"))
	t.Setenv("PLAX_BASE_NAME", "plax_e2e_"+suffix)

	// Base database.
	stdout, stderr, err := runPlax(bin, repo, "base", "reset", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("base reset: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	// Drop the base last: after the per-instance downs registered below.
	t.Cleanup(func() { dropBaseDB(t, pgURL) })

	for _, name := range []string{"i1", "i2", "i3"} {
		n := name
		t.Cleanup(func() {
			// Best effort; Down tolerates partially-created instances' records
			// being absent, so ignore "not found" errors.
			_, _, _ = runPlax(bin, repo, "down", n, "--pg-url", pgURL)
		})
	}

	// Two instances coexist.
	for _, name := range []string{"i1", "i2"} {
		_, stderr, err := runPlax(bin, repo, "up", name, "--pg-url", pgURL)
		if err != nil {
			t.Fatalf("up %s: %v\nstderr: %s", name, err, stderr)
		}
	}

	reg := openRegistryFile(t, repo)
	rec1, ok1 := reg.Instances["i1"]
	rec2, ok2 := reg.Instances["i2"]
	if !ok1 || !ok2 {
		t.Fatalf("registry missing instances: %v", reg.Instances)
	}
	if rec1.Ports["PORT"] == rec2.Ports["PORT"] {
		t.Fatalf("instances share app port %d", rec1.Ports["PORT"])
	}

	// Both serve HTTP on their own ports.
	waitHTTP200(t, rec1.Ports["PORT"])
	waitHTTP200(t, rec2.Ports["PORT"])

	// Databases exist.
	if !dbExists(t, pgURL, "plax_i1") || !dbExists(t, pgURL, "plax_i2") {
		t.Fatal("instance databases missing")
	}

	// Containers running, networks present.
	drv, err := docker.NewDriver()
	if err != nil {
		t.Fatalf("docker driver: %v", err)
	}
	defer func() { _ = drv.Close() }()
	for _, rec := range []registry.InstanceRecord{rec1, rec2} {
		running, err := drv.ServiceRunning(context.Background(), rec.ContainerIDs["redis"])
		if err != nil || !running {
			t.Errorf("redis container for %s: running=%v err=%v", rec.DBName, running, err)
		}
	}
	if !dockerNetworkExists(t, "plax-i1-net") || !dockerNetworkExists(t, "plax-i2-net") {
		t.Error("instance networks missing")
	}

	// exec: derived env is available, quoted secret round-trips.
	stdout, _, err = runPlax(bin, repo, "exec", "i1", "--", "printenv", "API_KEY")
	if err != nil {
		t.Fatalf("exec printenv: %v", err)
	}
	if strings.TrimSpace(stdout) != "sk-test # withhash" {
		t.Errorf("API_KEY = %q, want %q", strings.TrimSpace(stdout), "sk-test # withhash")
	}

	// The derived .env keeps the quoting.
	derived, err := os.ReadFile(filepath.Join(repo, ".plax", "worktrees", "i1", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(derived), `API_KEY="sk-test # withhash"`) {
		t.Errorf("derived .env lost quoting:\n%s", derived)
	}

	// exec propagates the command's exit code.
	_, _, err = runPlax(bin, repo, "exec", "i1", "--", "sh", "-c", "exit 3")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("exec exit code: err=%v, want exit code 3", err)
	}

	// exec exposes the allocated port.
	stdout, _, err = runPlax(bin, repo, "exec", "i1", "--", "printenv", "PORT")
	if err != nil {
		t.Fatalf("exec printenv PORT: %v", err)
	}
	if strings.TrimSpace(stdout) != fmt.Sprint(rec1.Ports["PORT"]) {
		t.Errorf("PORT = %q, want %d", strings.TrimSpace(stdout), rec1.Ports["PORT"])
	}

	// attach opens a login shell in the worktree; with no stdin it exits.
	if _, _, err := runPlax(bin, repo, "attach", "i1"); err != nil {
		t.Errorf("attach: %v", err)
	}

	// ls shows both instances, in table and JSON form.
	stdout, _, err = runPlax(bin, repo, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(stdout, "i1") || !strings.Contains(stdout, "i2") || !strings.Contains(stdout, "running") {
		t.Errorf("ls table missing instances:\n%s", stdout)
	}
	stdout, _, err = runPlax(bin, repo, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	var listed map[string]registry.InstanceRecord
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatalf("ls --json invalid JSON: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("ls --json shows %d instances, want 2", len(listed))
	}

	// A process that exits immediately fails up and leaves no residue.
	rewriteFixtureCommand(t, repo, "exit 1")
	_, stderr, err = runPlax(bin, repo, "up", "i3", "--pg-url", pgURL)
	if err == nil {
		t.Fatal("up i3 with an immediately-exiting process should fail")
	}
	if !strings.Contains(stderr, "exited immediately") {
		t.Errorf("stderr should name the failure, got:\n%s", stderr)
	}
	if worktree.BranchExists(repo, "i3") {
		t.Error("failed up left branch plax/i3")
	}
	if dbExists(t, pgURL, "plax_i3") {
		t.Error("failed up left database plax_i3")
	}
	if dockerNetworkExists(t, "plax-i3-net") {
		t.Error("failed up left network plax-i3-net")
	}

	// Clean teardown of the first instance.
	pid1 := rec1.PIDs["web"]
	if _, _, err := runPlax(bin, repo, "down", "i1", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i1: %v", err)
	}
	if worktree.BranchExists(repo, "i1") {
		t.Error("branch plax/i1 still exists after down")
	}
	if _, err := os.Stat(filepath.Join(repo, ".plax", "worktrees", "i1")); !os.IsNotExist(err) {
		t.Error("worktree i1 still exists after down")
	}
	if dbExists(t, pgURL, "plax_i1") {
		t.Error("database plax_i1 still exists after down")
	}
	if dockerNetworkExists(t, "plax-i1-net") {
		t.Error("network plax-i1-net still exists after down")
	}
	if process.IsAlive(pid1) {
		t.Error("web process for i1 still alive after down")
	}

	if _, _, err := runPlax(bin, repo, "down", "i2", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i2: %v", err)
	}

	// Registry is empty; ls reflects it.
	stdout, _, err = runPlax(bin, repo, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	if strings.TrimSpace(stdout) != "{}" {
		t.Errorf("ls --json = %q, want {}", stdout)
	}
	stdout, _, _ = runPlax(bin, repo, "ls")
	if strings.Contains(stdout, "i1") || !strings.Contains(stdout, "NAME") {
		t.Errorf("ls after teardown should print header only, got:\n%s", stdout)
	}

	// down on a nonexistent instance fails clearly.
	_, stderr, err = runPlax(bin, repo, "down", "nope", "--pg-url", pgURL)
	if err == nil || !strings.Contains(stderr, "not found") {
		t.Errorf("down nonexistent: err=%v stderr=%q", err, stderr)
	}
}

// TestEndToEnd_TwoInstancesWithTestDB verifies that a blueprint with
// multiple databases creates, uses, and cleans up all databases correctly.
func TestEndToEnd_TwoInstancesWithTestDB(t *testing.T) {
	pgURL := e2ePrereqs(t)
	bin := buildPlax(t)
	repo := initFixtureRepoWithTestDB(t)

	suffix := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "#", "_"))
	t.Setenv("PLAX_BASE_NAME", "plax_e2e_"+suffix)

	_, stderr, err := runPlax(bin, repo, "base", "reset", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("base reset: %v\nstderr: %s", err, stderr)
	}
	t.Cleanup(func() { dropBaseDB(t, pgURL) })

	for _, name := range []string{"i1", "i2"} {
		n := name
		t.Cleanup(func() {
			_, _, _ = runPlax(bin, repo, "down", n, "--pg-url", pgURL)
		})
	}

	// Create instance i1.
	_, stderr, err = runPlax(bin, repo, "up", "i1", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up i1: %v\nstderr: %s", err, stderr)
	}

	// Both plax_i1 and plax_i1_test exist on Postgres.
	if !dbExists(t, pgURL, "plax_i1") {
		t.Fatal("plax_i1 should exist")
	}
	if !dbExists(t, pgURL, "plax_i1_test") {
		t.Fatal("plax_i1_test should exist")
	}

	// DATABASE_TEST_URL resolves in the derived env.
	reg := openRegistryFile(t, repo)
	rec1, ok := reg.Instances["i1"]
	if !ok {
		t.Fatal("i1 not in registry")
	}
	if rec1.DBNames[""] != "plax_i1" {
		t.Errorf("DBNames[\"\"] = %q, want plax_i1", rec1.DBNames[""])
	}
	if rec1.DBNames["test"] != "plax_i1_test" {
		t.Errorf("DBNames[\"test\"] = %q, want plax_i1_test", rec1.DBNames["test"])
	}

	// Create instance i2 (concurrent).
	_, stderr, err = runPlax(bin, repo, "up", "i2", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up i2: %v\nstderr: %s", err, stderr)
	}

	if !dbExists(t, pgURL, "plax_i2") {
		t.Fatal("plax_i2 should exist")
	}
	if !dbExists(t, pgURL, "plax_i2_test") {
		t.Fatal("plax_i2_test should exist")
	}

	// Clean teardown: down i1 drops both databases.
	if _, _, err := runPlax(bin, repo, "down", "i1", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i1: %v", err)
	}
	if dbExists(t, pgURL, "plax_i1") {
		t.Error("plax_i1 still exists after down")
	}
	if dbExists(t, pgURL, "plax_i1_test") {
		t.Error("plax_i1_test still exists after down")
	}

	// Doctor after down reports no orphans for i1's databases.
	stdout, _, err := runPlax(bin, repo, "doctor", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if strings.Contains(stdout, "plax_i1") && strings.Contains(stdout, "unreferenced") {
		t.Error("doctor should not report plax_i1 databases as orphaned after down")
	}

	// Clean up i2.
	if _, _, err := runPlax(bin, repo, "down", "i2", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i2: %v", err)
	}
}

// --- helpers ---

func e2ePrereqs(t *testing.T) string {
	t.Helper()
	pgURL := os.Getenv("PLAX_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("skipping: PLAX_TEST_POSTGRES_URL not set")
	}
	for _, tool := range []string{"git", "python3", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("skipping: %s not available", tool)
		}
	}
	d, err := docker.NewDriver()
	if err != nil {
		t.Skipf("skipping: docker: %v", err)
	}
	if _, err := d.ServiceRunning(context.Background(), "plax-e2e-probe"); err != nil {
		_ = d.Close()
		t.Skipf("skipping: docker daemon: %v", err)
	}
	_ = d.Close()

	// This test resets and clones plax_base while pkg/derive/postgres tests
	// do the same in a parallel package binary; serialize on the server.
	testutil.LockPostgres(t, pgURL)
	return pgURL
}

func buildPlax(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "plax")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %s", out)
	}
	return bin
}

func initFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"plax.json": `{
  "version": 1,
  "name": "e2e",
  "port_pool": {"start": 26000, "end": 26100},
  "toolchain": ".tool-versions",
  "seed": {"migrate": "true", "command": "true", "workdir": "."},
  "services": {
    "db": {"isolation": "logical", "type": "postgres", "image": "postgres:16"},
    "redis": {"isolation": "dedicated", "image": "redis:7-alpine", "ports": {"6379": {"var": "REDIS_PORT"}}}
  },
  "processes": [
    {"name": "web", "isolation": "native", "command": "python3 -m http.server {{PORT}} --bind 127.0.0.1", "workdir": ".", "port_var": "PORT"}
  ],
  "env": {
    "template": ".env.example",
    "holes": {
      "PORT": "{{PORT}}",
      "REDIS_URL": "redis://localhost:{{REDIS_PORT}}/0",
      "DATABASE_URL": "postgres://localhost:5432/{{DB_NAME}}"
    }
  }
}
`,
		".env.example": `PORT=3000
REDIS_URL=redis://localhost:6379/0
DATABASE_URL=postgres://localhost:5432/e2e_dev
API_KEY=placeholder
`,
		// Quoted secret with a '#' exercises the override round-trip.
		".env":               "API_KEY=\"sk-test # withhash\"\n",
		".tool-versions":     "golang 1.26\n",
		"docker-compose.yml": "services: {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "e2e@test.com"},
		{"git", "config", "user.name", "E2E"},
		{"git", "add", "."},
		{"git", "commit", "-m", "fixture"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return dir
}

// initFixtureRepoWithTestDB returns a repo whose blueprint declares a _test
// database alongside the primary.
func initFixtureRepoWithTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"plax.json": `{
  "version": 1,
  "name": "e2e",
  "port_pool": {"start": 26200, "end": 26300},
  "toolchain": ".tool-versions",
  "seed": {"migrate": "true", "command": "true", "workdir": "."},
  "services": {
    "db": {
      "isolation": "logical",
      "type": "postgres",
      "image": "postgres:16",
      "databases": [{"name": "test", "from": "base"}]
    },
    "redis": {"isolation": "dedicated", "image": "redis:7-alpine", "ports": {"6379": {"var": "REDIS_PORT"}}}
  },
  "processes": [
    {"name": "web", "isolation": "native", "command": "python3 -m http.server {{PORT}} --bind 127.0.0.1", "workdir": ".", "port_var": "PORT"}
  ],
  "env": {
    "template": ".env.example",
    "holes": {
      "PORT": "{{PORT}}",
      "REDIS_URL": "redis://localhost:{{REDIS_PORT}}/0",
      "DATABASE_URL": "postgres://localhost:5432/{{DB_NAME}}",
      "DATABASE_TEST_URL": "postgres://localhost:5432/{{DB_NAME_test}}"
    }
  }
}
`,
		".env.example": `PORT=3000
REDIS_URL=redis://localhost:6379/0
DATABASE_URL=postgres://localhost:5432/e2e_dev
DATABASE_TEST_URL=postgres://localhost:5432/e2e_dev_test
API_KEY=placeholder
`,
		".env":               "API_KEY=\"sk-test # withhash\"\n",
		".tool-versions":     "golang 1.26\n",
		"docker-compose.yml": "services: {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "e2e@test.com"},
		{"git", "config", "user.name", "E2E"},
		{"git", "add", "."},
		{"git", "commit", "-m", "fixture"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return dir
}

// rewriteFixtureCommand replaces the web process command in the fixture's
// plax.json, simulating a blueprint edit.
func rewriteFixtureCommand(t *testing.T, repo, command string) {
	t.Helper()
	path := filepath.Join(repo, "plax.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := "python3 -m http.server {{PORT}} --bind 127.0.0.1"
	if !strings.Contains(string(data), old) {
		t.Fatal("fixture command not found")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), old, command, 1)), 0600); err != nil {
		t.Fatal(err)
	}
}

func runPlax(bin, repo string, args ...string) (string, string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = repo
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func openRegistryFile(t *testing.T, repo string) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(filepath.Join(repo, ".plax", "registry.json"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	return reg
}

func waitHTTP200(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no HTTP 200 from %s", url)
}

func dbExists(t *testing.T, pgURL, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var got string
	err = conn.QueryRow(ctx, "SELECT datname FROM pg_database WHERE datname=$1", name).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	return true
}

func dockerNetworkExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("docker", "network", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatalf("docker network ls: %v", err)
	}
	return strings.TrimSpace(string(out)) == name
}

// TestEndToEnd_UpWithRef verifies that --ref creates an instance branched
// from the specified ref, that both instances coexist on different ports,
// and that the worktree content matches the ref's branch.
func TestEndToEnd_UpWithRef(t *testing.T) {
	pgURL := e2ePrereqs(t)
	bin := buildPlax(t)
	repo := initFixtureRepoWithBranch(t)

	suffix := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "#", "_"))
	t.Setenv("PLAX_BASE_NAME", "plax_e2e_"+suffix)

	_, stderr, err := runPlax(bin, repo, "base", "reset", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("base reset: %v\nstderr: %s", err, stderr)
	}
	t.Cleanup(func() { dropBaseDB(t, pgURL) })

	for _, name := range []string{"r1", "r2"} {
		n := name
		t.Cleanup(func() {
			_, _, _ = runPlax(bin, repo, "down", n, "--pg-url", pgURL)
		})
	}

	// Instance from other-branch.
	_, stderr, err = runPlax(bin, repo, "up", "r1", "--ref", "other-branch", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up r1 --ref other-branch: %v\nstderr: %s", err, stderr)
	}

	// Verify the worktree contains the other-branch content.
	reg := openRegistryFile(t, repo)
	rec1, ok := reg.Instances["r1"]
	if !ok {
		t.Fatal("r1 not in registry")
	}
	if rec1.SourceRef != "other-branch" {
		t.Errorf("SourceRef = %q, want other-branch", rec1.SourceRef)
	}

	indexContent, err := os.ReadFile(filepath.Join(rec1.WorktreePath, "index.html"))
	if err != nil {
		t.Fatalf("read index.html from worktree: %v", err)
	}
	if strings.TrimSpace(string(indexContent)) != "other-branch" {
		t.Errorf("worktree index.html = %q, want other-branch", strings.TrimSpace(string(indexContent)))
	}

	// waitHTTP200 on r1's port.
	waitHTTP200(t, rec1.Ports["PORT"])

	// Instance from current HEAD (no --ref).
	_, stderr, err = runPlax(bin, repo, "up", "r2", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up r2: %v\nstderr: %s", err, stderr)
	}

	reg = openRegistryFile(t, repo)
	rec2, ok := reg.Instances["r2"]
	if !ok {
		t.Fatal("r2 not in registry")
	}
	if rec2.SourceRef != "" {
		t.Errorf("SourceRef should be empty, got %q", rec2.SourceRef)
	}

	indexContent2, err := os.ReadFile(filepath.Join(rec2.WorktreePath, "index.html"))
	if err != nil {
		t.Fatalf("read index.html from r2 worktree: %v", err)
	}
	if strings.TrimSpace(string(indexContent2)) != "main" {
		t.Errorf("r2 worktree index.html = %q, want main", strings.TrimSpace(string(indexContent2)))
	}

	// Both serve HTTP on different ports.
	waitHTTP200(t, rec2.Ports["PORT"])
	if rec1.Ports["PORT"] == rec2.Ports["PORT"] {
		t.Fatalf("instances share app port %d", rec1.Ports["PORT"])
	}

	// Cleanup.
	if _, _, err := runPlax(bin, repo, "down", "r1", "--pg-url", pgURL); err != nil {
		t.Fatalf("down r1: %v", err)
	}
	if _, _, err := runPlax(bin, repo, "down", "r2", "--pg-url", pgURL); err != nil {
		t.Fatalf("down r2: %v", err)
	}
}

// TestEndToEnd_ScratchDirectory verifies that up creates a scratch directory
// that is ignored by git, and that down removes it.
func TestEndToEnd_ScratchDirectory(t *testing.T) {
	pgURL := e2ePrereqs(t)
	bin := buildPlax(t)
	repo := initFixtureRepo(t)

	suffix := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "#", "_"))
	t.Setenv("PLAX_BASE_NAME", "plax_e2e_"+suffix)

	_, stderr, err := runPlax(bin, repo, "base", "reset", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("base reset: %v\nstderr: %s", err, stderr)
	}
	t.Cleanup(func() { dropBaseDB(t, pgURL) })
	t.Cleanup(func() { _, _, _ = runPlax(bin, repo, "down", "i1", "--pg-url", pgURL) })

	_, stderr, err = runPlax(bin, repo, "up", "i1", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up i1: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "scratch:") {
		t.Errorf("up summary missing scratch line:\n%s", stderr)
	}
	if !strings.Contains(stderr, "psql -d plax_i1") {
		t.Errorf("up summary missing psql hint:\n%s", stderr)
	}

	wtPath := filepath.Join(repo, ".plax", "worktrees", "i1")
	if info, err := os.Stat(filepath.Join(wtPath, "scratch")); err != nil || !info.IsDir() {
		t.Fatalf("scratch dir after up: info=%v err=%v", info, err)
	}

	// The fixture's tracked .env legitimately shows as modified (the derived
	// .env differs); scratch must not appear in the status.
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = wtPath
	statusOut, err := status.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.Contains(string(statusOut), "scratch") {
		t.Errorf("scratch appears in git status:\n%s", statusOut)
	}

	if _, _, err := runPlax(bin, repo, "down", "i1", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "scratch")); !os.IsNotExist(err) {
		t.Errorf("scratch residue after down: %v", err)
	}
}

// initFixtureRepoWithBranch returns a fixture repo that has an "other-branch"
// with distinct content from main.
func initFixtureRepoWithBranch(t *testing.T) string {
	t.Helper()
	dir := initFixtureRepo(t)

	// Add index.html on main.
	mainHTML := filepath.Join(dir, "index.html")
	if err := os.WriteFile(mainHTML, []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "index.html")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add index.html: %s", out)
	}
	cmd = exec.Command("git", "commit", "-m", "add index.html on main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s", out)
	}

	// Create other-branch with different content.
	cmd = exec.Command("git", "checkout", "-b", "other-branch")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b other-branch: %s", out)
	}
	if err := os.WriteFile(mainHTML, []byte("other-branch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "add", "index.html")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add index.html: %s", out)
	}
	cmd = exec.Command("git", "commit", "-m", "other-branch content")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit on other-branch: %s", out)
	}

	// Go back to main.
	cmd = exec.Command("git", "checkout", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout main: %s", out)
	}

	return dir
}

// TestEndToEnd_UpMigrations verifies that plain `plax up` applies pending
// migrations to the instance databases (and not the base), that
// `--skip migrate` leaves the instance on the cloned base schema, and that
// a failing migration rolls back with no registered instance.
func TestEndToEnd_UpMigrations(t *testing.T) {
	pgURL := e2ePrereqs(t)
	bin := buildPlax(t)
	repo := initFixtureRepoWithMigrations(t)

	suffix := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "#", "_"))
	t.Setenv("PLAX_BASE_NAME", "plax_e2e_"+suffix)

	_, stderr, err := runPlax(bin, repo, "base", "reset", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("base reset: %v\nstderr: %s", err, stderr)
	}
	t.Cleanup(func() { dropBaseDB(t, pgURL) })

	for _, name := range []string{"i1", "i2", "i3"} {
		n := name
		t.Cleanup(func() { _, _, _ = runPlax(bin, repo, "down", n, "--pg-url", pgURL) })
	}

	// Commit a migration absent from the base, then up applies it to the
	// instance only.
	commitFixtureMigration(t, repo, "002_items_size.sql", "ALTER TABLE items ADD COLUMN size integer;\n")

	_, stderr, err = runPlax(bin, repo, "up", "i1", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up i1: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "applying migrations...") {
		t.Errorf("up output missing migration step:\n%s", stderr)
	}
	if !strings.Contains(stderr, "migrations: 1 applied") {
		t.Errorf("up output missing measured migration count:\n%s", stderr)
	}
	if !columnExists(t, pgURL, "plax_i1", "size") {
		t.Fatal("plax_i1 should have the size column")
	}
	if columnExistsInLockedDB(t, pgURL, baseName(), "size") {
		t.Error("base must not receive instance migrations")
	}

	// --skip migrate: instance stays on the cloned base schema.
	_, stderr, err = runPlax(bin, repo, "up", "i2", "--skip", "migrate", "--pg-url", pgURL)
	if err != nil {
		t.Fatalf("up i2 --skip migrate: %v\nstderr: %s", err, stderr)
	}
	if strings.Contains(stderr, "applying migrations") {
		t.Errorf("--skip migrate still ran migrations:\n%s", stderr)
	}
	if columnExists(t, pgURL, "plax_i2", "size") {
		t.Error("plax_i2 should not have the size column")
	}

	// A failing migration fails up, rolls back, and registers nothing.
	commitFixtureMigration(t, repo, "003_broken.sql", "ALTER TABLE items ADD COLUMN;\n")
	_, stderr, err = runPlax(bin, repo, "up", "i3", "--pg-url", pgURL)
	if err == nil {
		t.Fatal("up i3 with a broken migration should fail")
	}
	if !strings.Contains(stderr, "migrate") || !strings.Contains(stderr, "syntax error") {
		t.Errorf("stderr should name the failing migration step:\n%s", stderr)
	}
	if worktree.BranchExists(repo, "i3") {
		t.Error("failed migration left branch plax/i3")
	}
	if dbExists(t, pgURL, "plax_i3") {
		t.Error("failed migration left database plax_i3")
	}
	if dockerNetworkExists(t, "plax-i3-net") {
		t.Error("failed migration left network plax-i3-net")
	}
	reg := openRegistryFile(t, repo)
	if _, found := reg.Instances["i3"]; found {
		t.Error("failed migration left a registry entry for i3")
	}

	if _, _, err := runPlax(bin, repo, "down", "i1", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i1: %v", err)
	}
	if _, _, err := runPlax(bin, repo, "down", "i2", "--pg-url", pgURL); err != nil {
		t.Fatalf("down i2: %v", err)
	}
}

// baseName returns the current PLAX_BASE_NAME or the default.
func baseName() string {
	if prefix := os.Getenv("PLAX_BASE_NAME"); prefix != "" {
		return prefix
	}
	return "plax_base"
}

// columnExists reports whether db has a column named column in the public
// schema. information_schema is scoped to the connected database, so the
// connection targets db itself.
func columnExists(t *testing.T, pgURL, db, column string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := url.Parse(pgURL)
	if err != nil {
		t.Fatalf("parse pgURL: %v", err)
	}
	u.Path = "/" + db
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to %s: %v", db, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var got string
	err = conn.QueryRow(ctx,
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema='public' AND table_name='items' AND column_name=$1`,
		column).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	return true
}

// columnExistsInLockedDB is columnExists for a database that refuses
// connections while locked (the base). The lock is briefly lifted for the
// read and restored, mirroring the base manager's readProvenance window.
func columnExistsInLockedDB(t *testing.T, pgURL, db, column string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	unlock := fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS true", db)
	relock := fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS false", db)
	if _, err := conn.Exec(ctx, unlock); err != nil {
		t.Fatalf("unlock %s: %v", db, err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), relock) }()

	u, err := url.Parse(pgURL)
	if err != nil {
		t.Fatalf("parse pgURL: %v", err)
	}
	u.Path = "/" + db
	qconn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to %s: %v", db, err)
	}
	defer func() { _ = qconn.Close(context.Background()) }()

	var got string
	err = qconn.QueryRow(ctx,
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema='public' AND table_name='items' AND column_name=$1`,
		column).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	return true
}

// initFixtureRepoWithMigrations returns a repo whose migrations directory
// holds one tracked migration and whose seed.migrate applies pending .sql
// files to $DATABASE_URL, recording each in a migrations table.
func initFixtureRepoWithMigrations(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"plax.json": `{
  "version": 1,
  "name": "e2e",
  "port_pool": {"start": 26400, "end": 26500},
  "toolchain": ".tool-versions",
  "seed": {
    "migrate": "for f in migrations/*.sql; do name=$(basename \"$f\"); applied=$(psql \"$DATABASE_URL\" -tA -c \"SELECT 1 FROM migrations WHERE name='$name'\" 2>/dev/null); if [ \"$applied\" != \"1\" ]; then psql \"$DATABASE_URL\" -q -v ON_ERROR_STOP=1 -f \"$f\" || exit 1; psql \"$DATABASE_URL\" -q -c \"INSERT INTO migrations (name) VALUES ('$name')\" || exit 1; fi; done",
    "command": "true",
    "workdir": ".",
    "applied_migrations": {"table": "migrations", "column": "name"}
  },
  "services": {
    "db": {"isolation": "logical", "type": "postgres", "image": "postgres:16"},
    "redis": {"isolation": "dedicated", "image": "redis:7-alpine", "ports": {"6379": {"var": "REDIS_PORT"}}}
  },
  "processes": [
    {"name": "web", "isolation": "native", "command": "python3 -m http.server {{PORT}} --bind 127.0.0.1", "workdir": ".", "port_var": "PORT"}
  ],
  "env": {
    "template": ".env.example",
    "holes": {
      "PORT": "{{PORT}}",
      "REDIS_URL": "redis://localhost:{{REDIS_PORT}}/0",
      "DATABASE_URL": "postgres://postgres:postgres@localhost:5432/{{DB_NAME}}"
    }
  }
}
`,
		".env.example": `PORT=3000
REDIS_URL=redis://localhost:6379/0
DATABASE_URL=postgres://postgres:postgres@localhost:5432/e2e_dev
API_KEY=placeholder
`,
		".env":               "API_KEY=\"sk-test # withhash\"\n",
		".tool-versions":     "golang 1.26\n",
		"docker-compose.yml": "services: {}\n",
		"migrations/001_init.sql": `CREATE TABLE migrations (name text PRIMARY KEY);
CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL);
`,
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "e2e@test.com"},
		{"git", "config", "user.name", "E2E"},
		{"git", "add", "."},
		{"git", "commit", "-m", "fixture"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return dir
}

// commitFixtureMigration writes a migration file into the fixture repo's
// migrations directory and commits it.
func commitFixtureMigration(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "migrations", name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "migrations/"+name)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s", out)
	}
	cmd = exec.Command("git", "commit", "-m", "add "+name)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s", out)
	}
}

func dropBaseDB(t *testing.T, pgURL string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), pgURL)
	if err != nil {
		t.Logf("cleanup connect: %v", err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	prefix := os.Getenv("PLAX_BASE_NAME")
	if prefix == "" {
		prefix = "plax_base"
	}
	for _, db := range []string{prefix, prefix + "_next"} {
		_, _ = conn.Exec(context.Background(),
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", db)
		_, _ = conn.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s", db))
	}
}
