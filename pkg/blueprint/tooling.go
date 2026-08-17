package blueprint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// toolingCategory describes one family of root-globbing tooling whose config
// would traverse every instance under .plax/worktrees/**. When a config file
// is present but does not mention ".plax", plax assumes the tool globs from
// the repo root and warns. The entry text is the exact ignore directive to add.
type toolingCategory struct {
	name  string
	globs []string
	entry string
}

var toolingCategories = []toolingCategory{
	{
		name:  "type checker",
		globs: []string{"tsconfig*.json"},
		entry: `".plax" in the "exclude" array`,
	},
	{
		name:  "test runner",
		globs: []string{"jest.config.*", "vitest.config.*", "playwright.config.*"},
		entry: `".plax/worktrees/**" in the ignore list`,
	},
	{
		name:  "linter or formatter",
		globs: []string{".eslintrc*", "eslint.config.*", ".prettierrc*", "prettier.config.*"},
		entry: `".plax/**" in the ignore list`,
	},
}

// toolingWarnings reports tooling configs that glob from the repo root and so
// would traverse every instance worktree. A config already mentioning ".plax"
// is assumed to have handled it and is not flagged.
func toolingWarnings(root string) []string {
	var warnings []string
	for _, cat := range toolingCategories {
		matched := map[string]bool{}
		for _, g := range cat.globs {
			files, _ := filepath.Glob(filepath.Join(root, g))
			for _, f := range files {
				matched[f] = true
			}
		}
		if len(matched) == 0 {
			continue
		}

		names := make([]string, 0, len(matched))
		handled := false
		for f := range matched {
			names = append(names, filepath.Base(f))
			if configMentionsPlax(f) {
				handled = true
			}
		}
		if handled {
			continue
		}

		sort.Strings(names)
		warnings = append(warnings, fmt.Sprintf(
			"init: %s config (%s) globs from the repo root and will traverse every instance under .plax/worktrees/; add %s",
			cat.name, strings.Join(names, ", "), cat.entry,
		))
	}
	return warnings
}

// configMentionsPlax reports whether a config file already references ".plax",
// taken as evidence the tool already ignores the worktree directory.
func configMentionsPlax(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), ".plax")
}

// EnsureIgnore appends ".plax/" to the repo's .gitignore when the directory is
// a git repo and the path is not already ignored. Returns whether it made a
// change. A non-git directory is left untouched.
func EnsureIgnore(root string) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return false, nil
	}

	ignored := exec.Command("git", "check-ignore", "--quiet", ".plax/")
	ignored.Dir = root
	if err := ignored.Run(); err == nil {
		return false, nil
	}

	path := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}

	// Avoid merging ".plax/" onto a final line that lacks a trailing newline.
	entry := ".plax/\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		entry = "\n" + entry
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(entry); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, nil
}
