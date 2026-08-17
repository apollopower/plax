package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
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
	return dir
}

// commitPlax writes plax.json, commits it, and returns repo root.
func commitPlax(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "plax.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write plax.json: %v", err)
	}
	cmd := exec.Command("git", "add", "plax.json")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s", out)
	}
	cmd = exec.Command("git", "commit", "-m", "add plax.json")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s", out)
	}
}

func addWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	cmd := exec.Command("git", "branch", name)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %s", out)
	}
	rel := filepath.Join(".plax", "worktrees", name)
	cmd = exec.Command("git", "worktree", "add", rel, name)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %s", out)
	}
	return filepath.Join(repo, rel)
}

func TestDiscoverRoot_FromRepoRoot(t *testing.T) {
	repo := initGitRepo(t)
	commitPlax(t, repo)

	root, found, err := discoverRoot(repo)
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if !found {
		t.Fatal("expected plax repo root found")
	}
	if root != repo {
		t.Errorf("root = %q, want %q", root, repo)
	}
}

func TestDiscoverRoot_FromSubdirectory(t *testing.T) {
	repo := initGitRepo(t)
	commitPlax(t, repo)
	sub := filepath.Join(repo, "deep", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	root, found, err := discoverRoot(sub)
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if !found {
		t.Fatal("expected plax repo root found")
	}
	if root != repo {
		t.Errorf("root = %q, want %q", root, repo)
	}
}

// A committed plax.json is checked out into every worktree. discoverRoot must
// bypass the worktree-local copy and resolve to the real root that owns
// .plax/registry.json.
func TestDiscoverRoot_FromInsideWorktree(t *testing.T) {
	repo := initGitRepo(t)
	commitPlax(t, repo)
	wt := addWorktree(t, repo, "plax/i1")

	root, found, err := discoverRoot(wt)
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if !found {
		t.Fatal("expected plax repo root found")
	}
	if root != repo {
		t.Errorf("root = %q, want real repo root %q (got worktree copy)", root, repo)
	}
}

func TestDiscoverRoot_NoPlax(t *testing.T) {
	dir := t.TempDir()
	_, found, err := discoverRoot(dir)
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if found {
		t.Fatal("expected no plax repo root found")
	}
}
