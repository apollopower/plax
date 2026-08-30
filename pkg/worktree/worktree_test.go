package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
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

func TestWorktree_BranchName(t *testing.T) {
	got := BranchName("i1")
	if got != "plax/i1" {
		t.Errorf("BranchName(i1) = %q, want %q", got, "plax/i1")
	}
}

func TestWorktree_WorktreeRelPath(t *testing.T) {
	got := WorktreeRelPath("i1")
	want := filepath.Join(".plax", "worktrees", "i1")
	if got != want {
		t.Errorf("WorktreeRelPath(i1) = %q, want %q", got, want)
	}
}

func TestWorktree_CreateSuccess(t *testing.T) {
	repo := initRepo(t)

	absPath, err := Create(repo, "i1", "")
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

func TestWorktree_CreateBranchExists(t *testing.T) {
	repo := initRepo(t)

	if _, err := Create(repo, "i1", ""); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err := Create(repo, "i1", "")
	if err == nil {
		t.Fatal("second Create should fail")
	}
}

func TestWorktree_CreateWithRef(t *testing.T) {
	repo := initRepoWithOtherBranch(t)

	absPath, err := Create(repo, "i1", "other")
	if err != nil {
		t.Fatalf("Create with ref: %v", err)
	}

	if !BranchExists(repo, "i1") {
		t.Error("branch plax/i1 should exist")
	}

	// The worktree should be at other's HEAD.
	wtRef, wtCommit, err := WorktreeHead(absPath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if wtRef != "plax/i1" {
		t.Errorf("worktree ref = %q, want plax/i1", wtRef)
	}

	otherCommit := revParse(t, repo, "other")
	if wtCommit != otherCommit {
		t.Errorf("worktree at %s, want other branch's HEAD (%s)", wtCommit, otherCommit)
	}
}

func TestWorktree_CreateWithDetachedRef(t *testing.T) {
	repo := initRepo(t)

	// Get the HEAD commit SHA.
	_, sha, err := HeadRef(repo)
	if err != nil {
		t.Fatalf("HeadRef: %v", err)
	}

	absPath, err := Create(repo, "i1", sha)
	if err != nil {
		t.Fatalf("Create with sha: %v", err)
	}

	// The worktree should be on branch plax/i1 at the given SHA.
	wtRef, wtCommit, err := WorktreeHead(absPath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if wtRef != "plax/i1" {
		t.Errorf("worktree ref = %q, want plax/i1", wtRef)
	}
	if wtCommit != sha {
		t.Errorf("worktree at %s, want %s", wtCommit, sha)
	}
}

func TestWorktree_RemoveSuccess(t *testing.T) {
	repo := initRepo(t)

	absPath, err := Create(repo, "i1", "")
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

func TestWorktree_RemoveMissingWorktreeStillDeletesBranch(t *testing.T) {
	repo := initRepo(t)

	absPath, err := Create(repo, "i1", "")
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

func TestWorktree_BranchExistsFalse(t *testing.T) {
	repo := initRepo(t)
	if BranchExists(repo, "nope") {
		t.Error("BranchExists should return false for missing branch")
	}
}

func TestWorktree_SchemaFilesAtRefMissingRef(t *testing.T) {
	repo := initRepo(t)
	_, err := SchemaFilesAtRef(repo, "nonexistent-ref", "src/db/migrations")
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}

func TestWorktree_BranchExistsTrue(t *testing.T) {
	repo := initRepo(t)
	if _, err := Create(repo, "i1", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !BranchExists(repo, "i1") {
		t.Error("BranchExists should return true after Create")
	}
}

func TestWorktree_WorktreeHeadNormalBranch(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1", "")
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

func TestWorktree_WorktreeHeadDetachedHead(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1", "")
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

func TestWorktree_WorktreeHeadMissingPath(t *testing.T) {
	_, _, err := WorktreeHead("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing worktree path")
	}
}

// --- ResolveRef tests ---

func TestWorktree_ResolveRefEmpty(t *testing.T) {
	repo := initRepo(t)
	got, err := ResolveRef(repo, "")
	if err != nil {
		t.Fatalf("ResolveRef(\"\"): %v", err)
	}
	if got != "" {
		t.Errorf("ResolveRef(\"\") = %q, want \"\"", got)
	}
}

func TestWorktree_ResolveRefPRNumber(t *testing.T) {
	repo := initRepo(t)
	// Seed a local ref to avoid a real fetch.
	seedPRRef(t, repo, "42")

	got, err := ResolveRef(repo, "42")
	if err != nil {
		t.Fatalf("ResolveRef(\"42\"): %v", err)
	}
	if got != "refs/pull/42/head" {
		t.Errorf("ResolveRef(\"42\") = %q, want refs/pull/42/head", got)
	}
}

func TestWorktree_ResolveRefPRNumberPrefixed(t *testing.T) {
	repo := initRepo(t)
	seedPRRef(t, repo, "42")

	got, err := ResolveRef(repo, "pr/42")
	if err != nil {
		t.Fatalf("ResolveRef(\"pr/42\"): %v", err)
	}
	if got != "refs/pull/42/head" {
		t.Errorf("ResolveRef(\"pr/42\") = %q, want refs/pull/42/head", got)
	}
}

func TestWorktree_ResolveRefBranchName(t *testing.T) {
	repo := initRepo(t)

	got, err := ResolveRef(repo, "main")
	if err != nil {
		t.Fatalf("ResolveRef(\"main\"): %v", err)
	}
	if got != "main" {
		t.Errorf("ResolveRef(\"main\") = %q, want \"main\"", got)
	}
}

func TestWorktree_ResolveRefOriginBranch(t *testing.T) {
	repo := initRepo(t)
	// Seed an origin/main ref locally.
	seedOriginRef(t, repo, "main")

	got, err := ResolveRef(repo, "origin/main")
	if err != nil {
		t.Fatalf("ResolveRef(\"origin/main\"): %v", err)
	}
	if got != "origin/main" {
		t.Errorf("ResolveRef(\"origin/main\") = %q, want \"origin/main\"", got)
	}
}

func TestWorktree_ResolveRefCommitSHA(t *testing.T) {
	repo := initRepo(t)

	_, sha, err := HeadRef(repo)
	if err != nil {
		t.Fatalf("HeadRef: %v", err)
	}

	got, err := ResolveRef(repo, sha)
	if err != nil {
		t.Fatalf("ResolveRef(%q): %v", sha, err)
	}
	if got != sha {
		t.Errorf("ResolveRef(%q) = %q, want %q", sha, got, sha)
	}
}

func TestWorktree_ResolveRefExplicitRef(t *testing.T) {
	repo := initRepo(t)

	got, err := ResolveRef(repo, "refs/heads/main")
	if err != nil {
		t.Fatalf("ResolveRef(\"refs/heads/main\"): %v", err)
	}
	if got != "refs/heads/main" {
		t.Errorf("ResolveRef(\"refs/heads/main\") = %q, want \"refs/heads/main\"", got)
	}
}

func TestWorktree_ResolveRefNotFound(t *testing.T) {
	repo := initRepo(t)

	_, err := ResolveRef(repo, "nonexistent-branch")
	if err == nil {
		t.Fatal("ResolveRef(\"nonexistent-branch\") should fail")
	}
}

func TestWorktree_ResolveRefBareIntegerFetched(t *testing.T) {
	// When a PR ref is not present locally, ResolveRef attempts git fetch.
	// Without a remote, this fails gracefully.
	repo := initRepo(t)

	_, err := ResolveRef(repo, "99999")
	if err == nil {
		t.Fatal("ResolveRef(\"99999\") should fail without a remote")
	}
}

// --- helpers ---

func initRepoWithOtherBranch(t *testing.T) string {
	t.Helper()
	dir := initRepo(t)

	// Create a second commit and branch "other" at it.
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "other-commit")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second commit: %s", out)
	}

	cmd = exec.Command("git", "branch", "other")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch other: %s", out)
	}

	// Reset main back to the first commit so the branches diverge.
	cmd = exec.Command("git", "reset", "--hard", "HEAD~1")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %s", out)
	}

	return dir
}

func seedPRRef(t *testing.T, repo, n string) {
	t.Helper()
	cmd := exec.Command("git", "update-ref", "refs/pull/"+n+"/head", "HEAD")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed PR ref: %s", out)
	}
}

func revParse(t *testing.T, repo, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func seedOriginRef(t *testing.T, repo, name string) {
	t.Helper()
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/"+name, "HEAD")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed origin ref: %s", out)
	}
}

func infoExcludePath(t *testing.T, worktreePath string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--git-path", "info/exclude")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-path: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestWorktree_CreateFromCommit_BasesBranchAtExactCommit(t *testing.T) {
	repo := initRepo(t)
	if _, err := Create(repo, "parent", ""); err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	// Advance the parent worktree so its HEAD differs from the repo root's.
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "parent work")
	cmd.Dir = filepath.Join(repo, ".plax", "worktrees", "parent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit in parent worktree: %s", out)
	}
	parentCommit := revParse(t, filepath.Join(repo, ".plax", "worktrees", "parent"), "HEAD")
	if parentCommit == revParse(t, repo, "HEAD") {
		t.Fatal("fixture setup failed: parent HEAD should differ from repo HEAD")
	}

	wtPath, err := CreateFromCommit(repo, "i1", parentCommit)
	if err != nil {
		t.Fatalf("CreateFromCommit: %v", err)
	}
	_, wtCommit, err := WorktreeHead(wtPath)
	if err != nil {
		t.Fatalf("WorktreeHead: %v", err)
	}
	if wtCommit != parentCommit {
		t.Errorf("child worktree at %s, want parent HEAD (%s)", wtCommit, parentCommit)
	}
	if got := revParse(t, repo, "plax/i1"); got != parentCommit {
		t.Errorf("child branch plax/i1 at %s, want %s", got, parentCommit)
	}
}

func TestWorktree_CreateFromCommit_RejectsMissingCommit(t *testing.T) {
	repo := initRepo(t)
	// A well-formed SHA that exists nowhere in the repository: no fallback
	// to HEAD, no branch, no worktree.
	missing := strings.Repeat("0", 40)
	_, err := CreateFromCommit(repo, "i1", missing)
	if err == nil {
		t.Fatal("CreateFromCommit with a missing commit should fail")
	}
	if BranchExists(repo, "i1") {
		t.Error("missing commit must not create the branch")
	}
	if _, err := os.Stat(filepath.Join(repo, WorktreeRelPath("i1"))); !os.IsNotExist(err) {
		t.Error("missing commit must not create the worktree")
	}
}

func TestWorktree_CreateFromCommit_RejectsNonSHA(t *testing.T) {
	repo := initRepo(t)
	for _, ref := range []string{"main", "HEAD", "abc1234"} {
		if _, err := CreateFromCommit(repo, "i1", ref); err == nil {
			t.Errorf("CreateFromCommit(%q) should reject a non-SHA ref", ref)
		}
	}
	if BranchExists(repo, "i1") {
		t.Error("non-SHA input must not create the branch")
	}
}

func TestWorktree_IsDirty_FiltersDerivedEnvOnly(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	dirty, err := IsDirty(wtPath)
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if dirty {
		t.Error("fresh worktree should be clean")
	}

	// The derived .env is plax's own output and must not count as dirt.
	if err := os.WriteFile(filepath.Join(wtPath, ".env"), []byte("PORT=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dirty, err = IsDirty(wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("modified .env should be filtered as a plax-managed artifact")
	}

	// A modified tracked file is operator work and must count.
	if err := os.WriteFile(filepath.Join(wtPath, ".env.example"), []byte("PORT=9999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dirty, err = IsDirty(wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Error("modified tracked file should be reported dirty")
	}
}

func TestWorktree_AddExclude_AppendsPattern(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	gitignorePath := filepath.Join(repo, ".gitignore")
	before := readFileOrEmpty(t, gitignorePath)

	if err := AddExclude(wtPath, "scratch/"); err != nil {
		t.Fatalf("AddExclude: %v", err)
	}

	data := readFileOrEmpty(t, infoExcludePath(t, wtPath))
	if !strings.Contains(data, "scratch/") {
		t.Errorf("exclude file missing pattern:\n%s", data)
	}
	if got := readFileOrEmpty(t, gitignorePath); got != before {
		t.Errorf("repo .gitignore was modified:\n%q != %q", got, before)
	}
}

func TestWorktree_AddExclude_Idempotent(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := AddExclude(wtPath, "scratch/"); err != nil {
		t.Fatalf("first AddExclude: %v", err)
	}
	excludePath := infoExcludePath(t, wtPath)
	after := readFileOrEmpty(t, excludePath)

	if err := AddExclude(wtPath, "scratch/"); err != nil {
		t.Fatalf("second AddExclude: %v", err)
	}
	if got := readFileOrEmpty(t, excludePath); got != after {
		t.Errorf("second call modified the file:\n%s", got)
	}
}

func TestWorktree_AddExclude_MissingGitDir(t *testing.T) {
	if err := AddExclude(t.TempDir(), "scratch/"); err == nil {
		t.Fatal("expected error for a path that is not a git worktree")
	}
}

func TestWorktree_AddExclude_PreservesExistingEntries(t *testing.T) {
	repo := initRepo(t)
	wtPath, err := Create(repo, "i1", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	excludePath := infoExcludePath(t, wtPath)
	if err := os.WriteFile(excludePath, []byte("existing/\n"), 0644); err != nil {
		t.Fatalf("write exclude file: %v", err)
	}

	if err := AddExclude(wtPath, "scratch/"); err != nil {
		t.Fatalf("AddExclude: %v", err)
	}

	data := readFileOrEmpty(t, excludePath)
	if !strings.Contains(data, "existing/") {
		t.Errorf("existing entry lost:\n%s", data)
	}
	if !strings.Contains(data, "scratch/") {
		t.Errorf("pattern not appended:\n%s", data)
	}
}
