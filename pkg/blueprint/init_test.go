package blueprint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestBlueprint_InitSampleRepoGoldenMatch(t *testing.T) {
	root := "testdata/sample"
	bp, _, err := InitFromRepo(root)
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

	if diff := cmp.Diff(string(golden), string(got)); diff != "" {
		t.Errorf("output does not match golden file (-want +got):\n%s", diff)
	}
}

func TestBlueprint_InitMissingCompose(t *testing.T) {
	root := t.TempDir()
	_, _, err := InitFromRepo(root)
	if err == nil {
		t.Fatal("expected error for missing compose")
	}
	if !strings.Contains(err.Error(), "docker-compose.yml not found") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestBlueprint_InitMissingEnvExample(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("version: '3'\nservices:\n  web:\n    image: nginx\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, _, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	if len(bp.Env.Holes) != 0 {
		t.Errorf("expected empty holes, got %v", bp.Env.Holes)
	}
}

func TestBlueprint_InitComposeWithNoImage(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("version: '3'\nservices:\n  noimg:\n    ports:\n      - '3000:3000'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(root, ".env.example")
	if err := os.WriteFile(envPath, []byte("KEY=value\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp, _, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	if _, exists := bp.Services["noimg"]; exists {
		t.Error("service without image should be skipped")
	}
}

func TestBlueprint_InitComposePortsBareNumber(t *testing.T) {
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

	bp, _, err := InitFromRepo(root)
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
	if pd.Var != "WEB_PORT" {
		t.Errorf("bare port should get WEB_PORT (derived from svc name), got %q", pd.Var)
	}
	if pd.Default != "8080" {
		t.Errorf("bare port should get Default 8080 (from host port), got %q", pd.Default)
	}
}

func TestBlueprint_InitComposePortsEnvVarDefault(t *testing.T) {
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

	bp, _, err := InitFromRepo(root)
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

func TestBlueprint_InitPostgresServiceNotInPortMap(t *testing.T) {
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

	bp, _, err := InitFromRepo(root)
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

func TestBlueprint_InitPostgresPortNotHole(t *testing.T) {
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

	bp, _, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	if _, exists := bp.Env.Holes["DATABASE_URL"]; exists {
		t.Error("DATABASE_URL with port 5432 should NOT be a hole when logical postgres exists")
	}
}

func TestBlueprint_InitSampleRepoPassesValidation(t *testing.T) {
	root := "testdata/sample"
	bp, _, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}

	bp.Env.Template = filepath.Join(root, bp.Env.Template)
	bp.Seed.Migrate = "bun run db migrate"
	bp.Seed.Command = "bun run db fixtures"

	errs := ValidateBlueprint(bp)
	if len(errs) > 0 {
		t.Errorf("expected no validation errors, got: %v", errs)
	}
}

func TestBlueprint_InitInfersAppliedMigrationsFromDependency(t *testing.T) {
	root := t.TempDir()
	writeInitFixture(t, root, `{
  "name": "app",
  "dependencies": {"knex": "^3.0.0"}
}`)

	bp, warnings, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	am := bp.Seed.AppliedMigrations
	if am == nil {
		t.Fatal("expected applied_migrations to be inferred")
	}
	if am.Table != "knex_migrations" || am.Column != "name" {
		t.Errorf("got %s/%s, want knex_migrations/name", am.Table, am.Column)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "verify the table and column names") {
		t.Errorf("expected a verify warning, got %v", warnings)
	}
}

func TestBlueprint_InitInfersAppliedMigrationsFromDevDependency(t *testing.T) {
	root := t.TempDir()
	writeInitFixture(t, root, `{
  "name": "app",
  "devDependencies": {"node-pg-migrate": "^7.0.0"}
}`)

	bp, _, err := InitFromRepo(root)
	if err != nil {
		t.Fatalf("InitFromRepo: %v", err)
	}
	am := bp.Seed.AppliedMigrations
	if am == nil {
		t.Fatal("expected applied_migrations to be inferred")
	}
	if am.Table != "pgmigrations" || am.Column != "name" {
		t.Errorf("got %s/%s, want pgmigrations/name", am.Table, am.Column)
	}
}

func TestBlueprint_InitNoFrameworkNoInference(t *testing.T) {
	for name, pkgJSON := range map[string]string{
		"no package.json": "",
		"no known framework": `{
  "name": "app",
  "dependencies": {"next": "^15.0.0", "react": "^19.0.0"}
}`,
		"unparseable package.json": `{ not json`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeInitFixture(t, root, pkgJSON)

			bp, warnings, err := InitFromRepo(root)
			if err != nil {
				t.Fatalf("InitFromRepo: %v", err)
			}
			if bp.Seed.AppliedMigrations != nil {
				t.Errorf("expected no applied_migrations, got %+v", bp.Seed.AppliedMigrations)
			}
			if strings.Contains(strings.Join(warnings, "\n"), "applied_migrations") {
				t.Errorf("unexpected applied_migrations warning: %v", warnings)
			}
		})
	}
}

// writeInitFixture writes the minimal files InitFromRepo needs: a compose
// file, an env example, and an optional package.json.
func writeInitFixture(t *testing.T, root, pkgJSON string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.example"), []byte("DATABASE_URL=postgres://localhost:5432/dev\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if pkgJSON != "" {
		if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(pkgJSON), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
