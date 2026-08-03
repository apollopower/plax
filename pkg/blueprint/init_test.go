package blueprint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInit_EaiRepo_GoldenMatch(t *testing.T) {
	root := "testdata/eai"
	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}

	got, err := json.MarshalIndent(bp, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	golden, err := os.ReadFile(filepath.Join(root, "plax.json.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if string(got) != string(golden) {
		t.Errorf("output does not match golden file\n--- got:\n%s\n--- golden:\n%s", string(got), string(golden))
	}
}

func TestInit_MissingCompose(t *testing.T) {
	root := t.TempDir()
	_, err := InitFromRepo(root)
	if err == nil {
		t.Fatal("expected error for missing compose")
	}
	if !strings.Contains(err.Error(), "docker-compose.yml not found") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestInit_MissingEnvExample(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("version: '3'\nservices:\n  web:\n    image: nginx\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	if len(bp.Env.Holes) != 0 {
		t.Errorf("expected empty holes, got %v", bp.Env.Holes)
	}
}

func TestInit_ComposeWithNoImage(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("version: '3'\nservices:\n  noimg:\n    ports:\n      - '3000:3000'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(root, ".env.example")
	if err := os.WriteFile(envPath, []byte("KEY=value\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	if _, exists := bp.Services["noimg"]; exists {
		t.Error("service without image should be skipped")
	}
}

func TestInit_EaiRepo_PassesValidation(t *testing.T) {
	root := "testdata/eai"
	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}

	errs := ValidateBlueprint(bp)
	if len(errs) > 0 {
		t.Errorf("expected no validation errors, got: %v", errs)
	}
}
