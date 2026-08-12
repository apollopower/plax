package blueprint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newValidBP() *Blueprint {
	return &Blueprint{
		Version:   1,
		Name:      "test",
		PortPool:  PortPool{Start: 3000, End: 4000},
		Toolchain: ".tool-versions",
		Seed:      SeedConfig{Migrate: "bun run db migrate", Command: "bun run db fixtures", Workdir: "."},
		Services: map[string]ServiceDef{
			"db": {
				Isolation: IsolationLogical,
				Type:      "postgres",
				Image:     "postgres:16",
			},
			"redis": {
				Isolation: IsolationDedicated,
				Image:     "redis:7",
				Ports:     map[string]PortDef{"6379": {Var: "REDIS_PORT"}},
			},
		},
		Processes: []ProcessDef{
			{Name: "app", Isolation: "native", Command: "next dev", Workdir: ".", PortVar: "PORT", DefaultPort: 3000},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
}

func TestBlueprint_ValidateValidBlueprint(t *testing.T) {
	errs := ValidateBlueprint(newValidBP())
	if len(errs) > 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestBlueprint_ValidateVersionNotOne(t *testing.T) {
	bp := newValidBP()
	bp.Version = 2
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "unsupported version 2") {
		t.Errorf("expected unsupported version error, got %v", errs)
	}
}

func TestBlueprint_ValidateMissingName(t *testing.T) {
	bp := newValidBP()
	bp.Name = ""
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "name is required") {
		t.Errorf("expected name required error, got %v", errs)
	}
}

func TestBlueprint_ValidatePortPoolInvalid(t *testing.T) {
	tests := []struct {
		name string
		pp   PortPool
	}{
		{"start less than 1024", PortPool{Start: 100, End: 4000}},
		{"start >= end", PortPool{Start: 5000, End: 4000}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bp := newValidBP()
			bp.PortPool = tc.pp
			errs := ValidateBlueprint(bp)
			if !containsErr(t, errs, "port_pool") {
				t.Errorf("expected range invalid, got %v", errs)
			}
		})
	}
}

func TestBlueprint_ValidateMapOverwriteNoDuplicateErrors(t *testing.T) {
	bp := newValidBP()
	bp.Services["db"] = ServiceDef{Isolation: IsolationLogical, Type: "postgres"}
	errs := ValidateBlueprint(bp)
	if len(errs) != 0 {
		t.Errorf("overwriting a map key should not produce a duplicate error, got %d: %v", len(errs), errs)
	}
}

func TestBlueprint_ValidateLogicalMissingType(t *testing.T) {
	bp := newValidBP()
	bp.Services["other"] = ServiceDef{
		Isolation: IsolationLogical,
		Image:     "postgres:16",
	}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "is logical but missing type") {
		t.Errorf("expected missing type error, got %v", errs)
	}
}

func TestBlueprint_ValidateLogicalHasPorts(t *testing.T) {
	bp := newValidBP()
	bp.Services["bad"] = ServiceDef{
		Isolation: IsolationLogical,
		Type:      "postgres",
		Ports:     map[string]PortDef{"5432": {Var: "PGPORT"}},
	}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "is logical but declares ports") {
		t.Errorf("expected declares ports error, got %v", errs)
	}
}

func TestBlueprint_ValidatePortVarConflict(t *testing.T) {
	bp := newValidBP()
	bp.Services["svc2"] = ServiceDef{
		Isolation: IsolationDedicated,
		Image:     "some-image",
		Ports:     map[string]PortDef{"9000": {Var: "REDIS_PORT"}},
	}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "port var") {
		t.Errorf("expected port var collision error, got %v", errs)
	}
}

func TestBlueprint_ValidateDuplicateProcess(t *testing.T) {
	bp := newValidBP()
	bp.Processes = append(bp.Processes, ProcessDef{Name: "app"})
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "duplicate process") {
		t.Errorf("expected duplicate process error, got %v", errs)
	}
}

func TestBlueprint_ValidatePortVarProcessCollision(t *testing.T) {
	bp := newValidBP()
	bp.Services["compress"] = ServiceDef{
		Isolation: IsolationDedicated,
		Image:     "img",
		Ports:     map[string]PortDef{"5000": {Var: "PORT"}},
	}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "port var") {
		t.Errorf("expected port var collision, got %v", errs)
	}
}

func TestBlueprint_ValidateDependsOnMissing(t *testing.T) {
	bp := newValidBP()
	bp.Processes = append(bp.Processes, ProcessDef{Name: "cron", Isolation: "native", Command: "echo hi", DependsOn: []string{"nonexistent"}})
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "depends_on") {
		t.Errorf("expected depends_on error, got %v", errs)
	}
}

func TestBlueprint_ValidateSeedMigrateMissing(t *testing.T) {
	bp := newValidBP()
	bp.Seed.Migrate = ""
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "seed.migrate is required") {
		t.Errorf("expected seed.migrate error, got %v", errs)
	}
}

func TestBlueprint_ValidateSeedCommandMissing(t *testing.T) {
	bp := newValidBP()
	bp.Seed.Command = ""
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "seed.command is required") {
		t.Errorf("expected seed.command error, got %v", errs)
	}
}

func TestBlueprint_ValidateSeedWorkdirMissing(t *testing.T) {
	bp := newValidBP()
	bp.Seed.Workdir = ""
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "seed.workdir is required") {
		t.Errorf("expected seed.workdir error, got %v", errs)
	}
}

func TestBlueprint_ValidateEnvTemplateMissing(t *testing.T) {
	bp := newValidBP()
	bp.Env.Template = ""
	bp.Env.Holes = map[string]string{"K": "v"}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "env.template is required") {
		t.Errorf("expected env.template error, got %v", errs)
	}
}

func TestBlueprint_ValidateHoleNotInTemplate(t *testing.T) {
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, ".env.example")
	if err := os.WriteFile(tmplPath, []byte("EXISTING_KEY=hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp := newValidBP()
	bp.Env.Template = tmplPath
	bp.Env.Holes = map[string]string{
		"EXISTING_KEY": "postgres://localhost:{{PORT}}/db",
		"MISSING_KEY":  "redis://localhost:{{REDIS_PORT}}/0",
	}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "hole") {
		t.Errorf("expected hole not found warning, got %v", errs)
	}
}

func TestBlueprint_ValidateStructuralIgnoresMissingHoles(t *testing.T) {
	// Minimal fresh blueprint: other tests mutate validBP's shared maps.
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Env: EnvConfig{
			Template: "testdata/does-not-matter.env.example",
			Holes:    map[string]string{"MISSING_KEY": "redis://localhost:{{REDIS_PORT}}/0"},
		},
	}

	if errs := ValidateStructural(bp); len(errs) > 0 {
		t.Errorf("ValidateStructural should not check holes in template, got %v", errs)
	}
}

func TestBlueprint_ValidateDockerNameCollision(t *testing.T) {
	bp := newValidBP()
	bp.Services["foo_bar"] = ServiceDef{Isolation: IsolationDedicated, Image: "img"}
	bp.Services["foo-bar"] = ServiceDef{Isolation: IsolationDedicated, Image: "img"}

	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "both map to docker name") {
		t.Errorf("expected docker name collision error, got %v", errs)
	}
}

func TestBlueprint_ValidateBadServiceName(t *testing.T) {
	for _, name := range []string{"Bad", "bad.name", "bad/name", "-bad"} {
		bp := newValidBP()
		bp.Services[name] = ServiceDef{Isolation: IsolationDedicated, Image: "img"}

		errs := ValidateStructural(bp)
		if !containsErr(t, errs, "must match ^[a-z0-9]") {
			t.Errorf("service %q: expected charset error, got %v", name, errs)
		}
	}
}

func TestBlueprint_ValidateBadProcessName(t *testing.T) {
	bp := newValidBP()
	bp.Processes = append(bp.Processes, ProcessDef{Name: "../escape", Command: "true", Workdir: "."})

	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "must match ^[a-z0-9]") {
		t.Errorf("expected charset error, got %v", errs)
	}
}

func TestBlueprint_ValidateEmptyPortVar(t *testing.T) {
	bp := newValidBP()
	bp.Services["bad"] = ServiceDef{
		Isolation: IsolationDedicated,
		Image:     "img",
		Ports:     map[string]PortDef{"8080": {Var: ""}},
	}
	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "empty var name") {
		t.Errorf("expected empty var name error, got %v", errs)
	}
}

func TestBlueprint_ValidateUnknownServiceIsolation(t *testing.T) {
	bp := newValidBP()
	bp.Services["bad"] = ServiceDef{
		Isolation: "dedicatd",
		Image:     "img",
	}
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "unknown isolation") {
		t.Errorf("expected unknown isolation error, got %v", errs)
	}
}

func TestBlueprint_ValidateStructuralDatabaseOnNonPostgres(t *testing.T) {
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Services: map[string]ServiceDef{
			"redis": {
				Isolation: IsolationDedicated,
				Image:     "redis:7",
				Databases: []DatabaseDef{{Name: "test", From: "base"}},
			},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "declares databases but is not a logical service") {
		t.Errorf("expected error for databases on non-logical service, got %v", errs)
	}
}

func TestBlueprint_ValidateStructuralDatabaseOnNonPostgresLogical(t *testing.T) {
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Services: map[string]ServiceDef{
			"db": {
				Isolation: IsolationLogical,
				Type:      "redis",
				Image:     "redis:7",
				Databases: []DatabaseDef{{Name: "test", From: "base"}},
			},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "declares databases but type is") {
		t.Errorf("expected error for databases on non-postgres logical service, got %v", errs)
	}
}

func TestBlueprint_ValidateStructuralDuplicateDatabaseKey(t *testing.T) {
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Services: map[string]ServiceDef{
			"db": {
				Isolation: IsolationLogical,
				Type:      "postgres",
				Image:     "postgres:16",
				Databases: []DatabaseDef{
					{Name: "test", From: "base"},
					{Name: "test", From: "base"},
				},
			},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "duplicate database key") {
		t.Errorf("expected duplicate database key error, got %v", errs)
	}
}

func TestBlueprint_ScrubKeyNotInTemplate_Warns(t *testing.T) {
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, ".env.example")
	if err := os.WriteFile(tmplPath, []byte("EXISTING_KEY=hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp := newValidBP()
	bp.Env.Template = tmplPath
	bp.Env.Scrub = []string{"EXISTING_KEY", "MISSING_KEY"}

	errs := ValidateBlueprint(bp)
	if !containsErr(t, errs, "scrubbed key") {
		t.Errorf("expected scrub key warning, got %v", errs)
	}
	// EXISTING_KEY is in template so no warning for it.
	// Only MISSING_KEY should trigger a warning.
	warnCount := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "scrubbed key") {
			warnCount++
		}
	}
	if warnCount != 1 {
		t.Errorf("expected exactly 1 scrub key warning (for MISSING_KEY), got %d", warnCount)
	}
}

func TestBlueprint_ValidateStructuralValidDatabaseDeclarations(t *testing.T) {
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Services: map[string]ServiceDef{
			"db": {
				Isolation: IsolationLogical,
				Type:      "postgres",
				Image:     "postgres:16",
				Databases: []DatabaseDef{
					{Name: "test", From: "base"},
					{Name: "cache", From: "base"},
				},
			},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
	errs := ValidateStructural(bp)
	for _, e := range errs {
		if strings.Contains(e.Error(), "databases") {
			t.Errorf("unexpected error about databases: %v", errs)
		}
	}
}

func TestBlueprint_ValidateUnknownProcessIsolation(t *testing.T) {
	bp := newValidBP()
	bp.Processes = append(bp.Processes, ProcessDef{
		Name:      "bad",
		Isolation: "container",
		Command:   "true",
		Workdir:   ".",
	})
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "unknown isolation") {
		t.Errorf("expected unknown isolation error, got %v", errs)
	}
}

func TestBlueprint_ValidateAllKnownServiceIsolations(t *testing.T) {
	for _, iso := range []ServiceIsolation{IsolationLogical, IsolationDedicated, IsolationShared, IsolationExternal} {
		bp := &Blueprint{
			Version:  1,
			Name:     "t",
			PortPool: PortPool{Start: 3000, End: 4000},
			Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
			Services: map[string]ServiceDef{
				"svc": {Isolation: iso, Type: "postgres", Image: "img"},
			},
			Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
		}
		errs := ValidateStructural(bp)
		for _, e := range errs {
			if strings.Contains(e.Error(), "unknown isolation") {
				t.Errorf("isolation %q should be valid, got: %v", iso, e)
			}
		}
	}
}

func TestBlueprint_ValidateStructuralDatabaseInvalidFrom(t *testing.T) {
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Services: map[string]ServiceDef{
			"db": {
				Isolation: IsolationLogical,
				Type:      "postgres",
				Image:     "postgres:16",
				Databases: []DatabaseDef{{Name: "test", From: "shadow"}},
			},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "unsupported from") {
		t.Errorf("expected unsupported from error, got %v", errs)
	}
}

func TestBlueprint_ValidateStructuralDatabaseEmptyName(t *testing.T) {
	bp := &Blueprint{
		Version:  1,
		Name:     "t",
		PortPool: PortPool{Start: 3000, End: 4000},
		Seed:     SeedConfig{Migrate: "m", Command: "c", Workdir: "."},
		Services: map[string]ServiceDef{
			"db": {
				Isolation: IsolationLogical,
				Type:      "postgres",
				Image:     "postgres:16",
				Databases: []DatabaseDef{{Name: "", From: "base"}},
			},
		},
		Env: EnvConfig{Template: ".env.example", Holes: map[string]string{}},
	}
	errs := ValidateStructural(bp)
	if !containsErr(t, errs, "must not be empty") {
		t.Errorf("expected empty name error, got %v", errs)
	}
}

func containsErr(t *testing.T, errs []error, substr string) bool {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}
