// Package worktree manages git branches and worktrees for Plax instances.
package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BranchName returns the git branch name for an instance.
func BranchName(name string) string {
	return "plax/" + name
}

// WorktreeRelPath returns the worktree path relative to repo root.
func WorktreeRelPath(name string) string {
	return filepath.Join(".plax", "worktrees", name)
}

// BranchExists reports whether the instance branch exists.
func BranchExists(repoRoot, name string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+BranchName(name))
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}

// Create creates a git branch (plax/<name>) at current HEAD and adds a
// worktree at .plax/worktrees/<name>. Returns the absolute worktree path.
// Fails if the branch already exists.
func Create(repoRoot, name string) (string, error) {
	branch := BranchName(name)
	relPath := WorktreeRelPath(name)
	absPath := filepath.Join(repoRoot, relPath)

	if BranchExists(repoRoot, name) {
		return "", fmt.Errorf("worktree: branch %q already exists", branch)
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", fmt.Errorf("worktree: mkdir: %w", err)
	}

	cmd := exec.Command("git", "branch", branch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree: create branch %s: %s", branch, strings.TrimSpace(string(out)))
	}

	cmd = exec.Command("git", "worktree", "add", relPath, branch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		// Clean up the branch we just created.
		cleanup := exec.Command("git", "branch", "-D", branch)
		cleanup.Dir = repoRoot
		_ = cleanup.Run()
		return "", fmt.Errorf("worktree: add %s: %s", relPath, strings.TrimSpace(string(out)))
	}

	return absPath, nil
}

// Remove removes the worktree and force-deletes the branch.
// Plax owns the branch — unmerged commits are the user's responsibility
// to push before running down.
//
// The branch deletion runs even when worktree removal fails (e.g. the
// worktree was deleted manually): a stranded branch blocks the next create,
// and Down treats a missing worktree as an already-cleaned resource.
func Remove(repoRoot, name string) error {
	relPath := WorktreeRelPath(name)
	branch := BranchName(name)

	var wtErr error
	cmd := exec.Command("git", "worktree", "remove", "--force", relPath)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		// Prune stale administrative entries so a manually deleted worktree
		// does not wedge future operations.
		prune := exec.Command("git", "worktree", "prune")
		prune.Dir = repoRoot
		_ = prune.Run()
		wtErr = fmt.Errorf("worktree: remove %s: %s", relPath, strings.TrimSpace(string(out)))
	}

	cmd = exec.Command("git", "branch", "-D", branch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		branchErr := fmt.Errorf("worktree: delete branch %s: %s", branch, strings.TrimSpace(string(out)))
		return errors.Join(wtErr, branchErr)
	}

	return wtErr
}
