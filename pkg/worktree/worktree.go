// Package worktree manages git branches and worktrees for Plax instances.
package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// Create creates a git branch (plax/<name>) and adds a worktree at
// .plax/worktrees/<name>. Returns the absolute worktree path.
//
// If sourceRef is empty, branches from the repo root's current HEAD
// (existing behaviour). Otherwise, branches from the resolved commit
// of sourceRef (which should already be resolved by ResolveRef).
func Create(repoRoot, name, sourceRef string) (string, error) {
	branch := BranchName(name)
	relPath := WorktreeRelPath(name)
	absPath := filepath.Join(repoRoot, relPath)

	if BranchExists(repoRoot, name) {
		return "", fmt.Errorf("worktree: branch %q already exists", branch)
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", fmt.Errorf("worktree: mkdir: %w", err)
	}

	var cmd *exec.Cmd
	if sourceRef == "" {
		cmd = exec.Command("git", "branch", branch)
	} else {
		cmd = exec.Command("git", "branch", branch, sourceRef)
	}
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree: create branch %s: %s", branch, strings.TrimSpace(string(out)))
	}

	cmd = exec.Command("git", "worktree", "add", relPath, branch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup := exec.Command("git", "branch", "-D", branch)
		cleanup.Dir = repoRoot
		_ = cleanup.Run()
		return "", fmt.Errorf("worktree: add %s: %s", relPath, strings.TrimSpace(string(out)))
	}

	return absPath, nil
}

// ResolveRef resolves a user-supplied ref string to a git ref that can be
// passed to `git branch <name> <ref>`.
//
//	""                   → "" (caller uses repo-root HEAD)
//	"123" or "pr/123"    → "refs/pull/123/head" (fetched if needed)
//	"refs/..."           → verified as-is
//	branch/tag/SHA       → verified via git rev-parse --verify
func ResolveRef(repoRoot, ref string) (string, error) {
	// Step 1: empty string — caller uses repo-root HEAD.
	if ref == "" {
		return "", nil
	}

	// PR number (bare integer or pr/N).
	n := strings.TrimPrefix(ref, "pr/")
	isPR := strings.HasPrefix(ref, "pr/")
	if !isPR {
		// Check if the entire ref is a bare integer.
		if _, err := strconv.Atoi(ref); err == nil {
			isPR = true
			n = ref
		}
	}
	if isPR {
		if _, err := strconv.Atoi(n); err != nil {
			return "", fmt.Errorf("--ref %q: expecting a PR number, got %q", ref, n)
		}
		fetchRef := "refs/pull/" + n + "/head"
		if !RefExists(repoRoot, fetchRef) {
			cmd := exec.Command("git", "fetch", "origin", fetchRef+":"+fetchRef)
			cmd.Dir = repoRoot
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("cannot fetch PR #%s — does the remote have a PR with that number? Try 'gh pr view %s' to confirm: %s", n, n, strings.TrimSpace(string(out)))
			}
		}
		return fetchRef, nil
	}

	// Explicit refs/ prefix.
	if strings.HasPrefix(ref, "refs/") {
		if !RefExists(repoRoot, ref) {
			return "", fmt.Errorf("ref %q not found", ref)
		}
		return ref, nil
	}

	// Branch, tag, or short SHA.
	if RefExists(repoRoot, ref) {
		return ref, nil
	}
	if RefExists(repoRoot, "origin/"+ref) {
		return "origin/" + ref, nil
	}
	return "", fmt.Errorf("ref %q not found — check the branch name or run 'git fetch'", ref)
}

// AddExclude appends a pattern to a linked worktree's local exclude file
// (.git/worktrees/<name>/info/exclude). It never touches the repo's tracked
// .gitignore. The resolve uses --git-path so it lands in the worktree-local
// gitdir in every layout, including --separate-git-dir.
func AddExclude(worktreePath, pattern string) error {
	cmd := exec.Command("git", "rev-parse", "--git-path", "info/exclude")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("worktree: resolve exclude path: %w", err)
	}
	excludePath := strings.TrimSpace(string(out))

	data, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: read exclude file: %w", err)
	}
	if bytes.Contains(data, []byte(pattern+"\n")) {
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte(pattern+"\n")...)
	if err := os.WriteFile(excludePath, data, 0644); err != nil {
		return fmt.Errorf("worktree: write exclude file: %w", err)
	}
	return nil
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
