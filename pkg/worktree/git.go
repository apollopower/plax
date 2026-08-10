package worktree

import (
	"fmt"
	"os/exec"
	"strings"
)

func HeadRef(repoRoot string) (ref, commit string, err error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("worktree: head ref: %w", err)
	}
	ref = strings.TrimSpace(string(out))
	if ref == "HEAD" {
		ref = ""
	}

	var commitOut []byte
	commitCmd := exec.Command("git", "rev-parse", "HEAD")
	commitCmd.Dir = repoRoot
	commitOut, err = commitCmd.Output()
	if err != nil {
		return ref, "", fmt.Errorf("worktree: head commit: %w", err)
	}
	commit = strings.TrimSpace(string(commitOut))
	return ref, commit, nil
}

func WorktreeHead(worktreePath string) (ref, commit string, err error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("worktree: worktree head ref: %w", err)
	}
	ref = strings.TrimSpace(string(out))
	if ref == "HEAD" {
		ref = ""
	}

	commitCmd := exec.Command("git", "rev-parse", "HEAD")
	commitCmd.Dir = worktreePath
	commitOut, err := commitCmd.Output()
	if err != nil {
		return ref, "", fmt.Errorf("worktree: worktree head commit: %w", err)
	}
	commit = strings.TrimSpace(string(commitOut))
	return ref, commit, nil
}

func RefExists(repoRoot, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}

func AheadBehind(repoRoot, baseRef, branch string) (ahead, behind int, err error) {
	rangeSpec := baseRef + "..." + branch
	cmd := exec.Command("git", "rev-list", "--left-right", "--count", rangeSpec)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("worktree: ahead-behind %s...%s: %w", baseRef, branch, err)
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 2 {
		return 0, 0, nil
	}
	_, err = fmt.Sscanf(parts[0]+" "+parts[1], "%d %d", &behind, &ahead)
	if err != nil {
		return 0, 0, fmt.Errorf("worktree: parse ahead-behind: %w", err)
	}
	return ahead, behind, nil
}

func SchemaFilesAtRef(repoRoot, ref, dir string) ([]string, error) {
	cmd := exec.Command("git", "ls-tree", ref, "--", dir+"/")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", ref, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var names []string
	prefix := dir + "/"
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		if parts[1] != "blob" {
			continue
		}
		name := strings.TrimPrefix(parts[3], prefix)
		if name == "" || name == parts[3] {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}
