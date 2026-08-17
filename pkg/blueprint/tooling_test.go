package blueprint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlueprint_ToolingWarnings_TypeChecker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	got := toolingWarnings(root)
	if len(got) != 1 {
		t.Fatalf("expected 1 warning, got %v", got)
	}
	if !strings.Contains(got[0], "type checker") || !strings.Contains(got[0], ".plax") {
		t.Errorf("warning should name type checker and .plax, got: %s", got[0])
	}
}

func TestBlueprint_ToolingWarnings_AlreadyIgnored(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte(`{ "exclude": [".plax"] }`), 0644); err != nil {
		t.Fatal(err)
	}

	if got := toolingWarnings(root); len(got) != 0 {
		t.Errorf("expected no warnings when config already references .plax, got %v", got)
	}
}

func TestBlueprint_ToolingWarnings_NoConfig(t *testing.T) {
	root := t.TempDir()
	if got := toolingWarnings(root); len(got) != 0 {
		t.Errorf("expected no warnings with no config, got %v", got)
	}
}

func TestBlueprint_ToolingWarnings_TestRunnerAndLinter(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"vitest.config.ts", "eslint.config.mjs"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("export default {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got := toolingWarnings(root)
	if len(got) != 2 {
		t.Fatalf("expected 2 warnings, got %v", got)
	}
}

func TestBlueprint_EnsureIgnore_NonGitDir(t *testing.T) {
	root := t.TempDir()
	changed, err := EnsureIgnore(root)
	if err != nil {
		t.Fatalf("EnsureIgnore: %v", err)
	}
	if changed {
		t.Error("expected no change for a non-git directory")
	}
}

func TestBlueprint_EnsureIgnore_Appends(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)

	changed, err := EnsureIgnore(root)
	if err != nil {
		t.Fatalf("EnsureIgnore: %v", err)
	}
	if !changed {
		t.Fatal("expected .gitignore to be appended")
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), ".plax/") {
		t.Errorf(".gitignore should contain .plax/, got: %q", data)
	}

	if changed, err := EnsureIgnore(root); err != nil {
		t.Fatalf("EnsureIgnore: %v", err)
	} else if changed {
		t.Error("second call should be a no-op")
	}
}

func TestBlueprint_EnsureIgnore_PreservesExisting(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureIgnore(root)
	if err != nil {
		t.Fatalf("EnsureIgnore: %v", err)
	}
	if !changed {
		t.Fatal("expected .gitignore to be appended")
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "node_modules/") || !strings.Contains(string(data), ".plax/") {
		t.Errorf(".gitignore should keep existing entries and add .plax/, got: %q", data)
	}
}

func TestBlueprint_EnsureIgnore_AlreadyIgnored(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".plax/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureIgnore(root)
	if err != nil {
		t.Fatalf("EnsureIgnore: %v", err)
	}
	if changed {
		t.Error("expected no change when .plax/ is already ignored")
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
}
