package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "init"},
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

func TestBranchName(t *testing.T) {
	got := BranchName("i1")
	if got != "plax/i1" {
		t.Errorf("BranchName(i1) = %q, want %q", got, "plax/i1")
	}
}

func TestWorktreeRelPath(t *testing.T) {
	got := WorktreeRelPath("i1")
	want := filepath.Join(".plax", "worktrees", "i1")
	if got != want {
		t.Errorf("WorktreeRelPath(i1) = %q, want %q", got, want)
	}
}

func TestCreate_Success(t *testing.T) {
	repo := initRepo(t)

	absPath, err := Create(repo, "i1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := filepath.Join(repo, ".plax", "worktrees", "i1")
	if absPath != want {
		t.Errorf("absPath = %q, want %q", absPath, want)
	}

	if !BranchExists(repo, "i1") {
		t.Error("branch should exist after Create")
	}

	if _, err := os.Stat(absPath); err != nil {
		t.Errorf("worktree dir should exist: %v", err)
	}
}

func TestCreate_BranchExists(t *testing.T) {
	repo := initRepo(t)

	if _, err := Create(repo, "i1"); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err := Create(repo, "i1")
	if err == nil {
		t.Fatal("second Create should fail")
	}
}

func TestRemove_Success(t *testing.T) {
	repo := initRepo(t)

	absPath, err := Create(repo, "i1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := Remove(repo, "i1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed")
	}

	if BranchExists(repo, "i1") {
		t.Error("branch should be deleted after Remove")
	}
}

func TestRemove_MissingWorktreeStillDeletesBranch(t *testing.T) {
	repo := initRepo(t)

	absPath, err := Create(repo, "i1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate manual cleanup outside plax: the directory and git's
	// administrative entry are gone, but the branch remains. In this state
	// `git worktree remove` fails — the branch must still be deleted.
	if err := os.RemoveAll(absPath); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	prune := exec.Command("git", "worktree", "prune")
	prune.Dir = repo
	if out, err := prune.CombinedOutput(); err != nil {
		t.Fatalf("prune: %s", out)
	}

	err = Remove(repo, "i1")
	if err == nil {
		t.Error("Remove should report the worktree removal failure")
	}
	if BranchExists(repo, "i1") {
		t.Error("branch should be deleted even when the worktree is already gone")
	}
}

func TestBranchExists_False(t *testing.T) {
	repo := initRepo(t)
	if BranchExists(repo, "nope") {
		t.Error("BranchExists should return false for missing branch")
	}
}

func TestSchemaFilesAtRef_MissingRef(t *testing.T) {
	repo := initRepo(t)
	_, err := SchemaFilesAtRef(repo, "nonexistent-ref", "src/db/migrations")
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}

func TestBranchExists_True(t *testing.T) {
	repo := initRepo(t)
	if _, err := Create(repo, "i1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !BranchExists(repo, "i1") {
		t.Error("BranchExists should return true after Create")
	}
}

func TestWorktreeHead_NormalBranch(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ref, commit, err := WorktreeHead(wtPath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if ref != "plax/i1" {
		t.Errorf("ref = %q, want %q", ref, "plax/i1")
	}
	if commit == "" {
		t.Error("commit should not be empty")
	}
}

func TestWorktreeHead_DetachedHead(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cmd := exec.Command("git", "checkout", "--detach", "HEAD")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout --detach: %s", out)
	}

	ref, commit, err := WorktreeHead(wtPath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if ref != "" {
		t.Errorf("ref for detached HEAD should be empty, got %q", ref)
	}
	if commit == "" {
		t.Error("commit should not be empty for detached HEAD")
	}
}

func TestWorktreeHead_MissingPath(t *testing.T) {
	_, _, err := WorktreeHead("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing worktree path")
	}
}
