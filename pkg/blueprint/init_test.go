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

func TestInit_ComposePortsBareNumber(t *testing.T) {
	root := t.TempDir()
	composeContent := `version: '3'
services:
  web:
    image: nginx
    ports:
      - "8080:80"
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(composeContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.example"), []byte("PORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	svc, ok := bp.Services["web"]
	if !ok {
		t.Fatal("expected web service")
	}
	if len(svc.Ports) == 0 {
		t.Fatal("expected ports")
	}
	pd, ok := svc.Ports["80"]
	if !ok {
		t.Fatal("expected container port 80")
	}
	if pd.Var != "SVC_PORT" {
		t.Errorf("bare port should get SVC_PORT, got %q", pd.Var)
	}
}

func TestInit_ComposePortsEnvVarDefault(t *testing.T) {
	root := t.TempDir()
	composeContent := `version: '3'
services:
  cache:
    image: redis
    ports:
      - "${MY_REDIS_PORT:-6379}:6379"
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(composeContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.example"), []byte("CACHE_URL=redis://localhost:6379\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	svc, ok := bp.Services["cache"]
	if !ok {
		t.Fatal("expected cache service")
	}

	pd, ok := svc.Ports["6379"]
	if !ok {
		t.Fatal("expected container port 6379")
	}
	if pd.Var != "MY_REDIS_PORT" {
		t.Errorf("expected MY_REDIS_PORT, got %q", pd.Var)
	}
	if pd.Default != "6379" {
		t.Errorf("expected default 6379, got %q", pd.Default)
	}
}

func TestInit_PostgresService_NotInPortMap(t *testing.T) {
	root := t.TempDir()
	composeContent := `version: '3'
services:
  db:
    image: postgres:16
    ports:
      - "${PGPORT:-5432}:5432"
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(composeContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.example"), []byte("DATABASE_URL=postgres://localhost:5432/dev\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	svc, ok := bp.Services["db"]
	if !ok {
		t.Fatal("expected db service")
	}
	if len(svc.Ports) > 0 {
		t.Errorf("postgres service should have no ports, got %v", svc.Ports)
	}
}

func TestInit_PostgresPort_NotHole(t *testing.T) {
	root := t.TempDir()
	composeContent := `version: '3'
services:
  db:
    image: pgvector:0.5
    ports:
      - "${PGPORT:-5432}:5432"
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(composeContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.example"), []byte("DATABASE_URL=postgres://localhost:5432/dev\nOTHER_KEY=localhost:3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	if _, exists := bp.Env.Holes["DATABASE_URL"]; exists {
		t.Error("DATABASE_URL with port 5432 should NOT be a hole when logical postgres exists")
	}
}

func TestInit_EaiRepo_PassesValidation(t *testing.T) {
	root := "testdata/eai"
	bp, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}

	bp.Env.Template = filepath.Join(root, bp.Env.Template)

	errs := ValidateBlueprint(bp)
	if len(errs) > 0 {
		t.Errorf("expected no validation errors, got: %v", errs)
	}
}
