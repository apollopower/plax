package blueprint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var validBP = &Blueprint{
	Version:   1,
	Name:      "test",
	PortPool:  PortPool{Start: 3000, End: 4000},
	Toolchain: ".tool-versions",
	Seed:      SeedConfig{Command: "bun run db fixtures", Workdir: "."},
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

func TestValidate_ValidBlueprint(t *testing.T) {
	errs := ValidateBlueprint(validBP)
	if len(errs) > 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidate_VersionNotOne(t *testing.T) {
	bp := *validBP
	bp.Version = 2
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "unsupported version 2") {
		t.Errorf("expected unsupported version error, got %v", errs)
	}
}

func TestValidate_MissingName(t *testing.T) {
	bp := *validBP
	bp.Name = ""
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "name is required") {
		t.Errorf("expected name required error, got %v", errs)
	}
}

func TestValidate_PortPoolInvalid(t *testing.T) {
	tests := []struct {
		name string
		pp   PortPool
	}{
		{"start less than 1024", PortPool{Start: 100, End: 4000}},
		{"start >= end", PortPool{Start: 5000, End: 4000}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bp := *validBP
			bp.PortPool = tc.pp
			errs := ValidateBlueprint(&bp)
			if !containsErr(errs, "port_pool range invalid") {
				t.Errorf("expected range invalid, got %v", errs)
			}
		})
	}
}

func TestValidate_DuplicateService(t *testing.T) {
	bp := *validBP
	bp.Services["db"] = ServiceDef{Isolation: IsolationLogical, Type: "postgres"} // overwrites, no duplicate in map
	errs := ValidateBlueprint(&bp)
	if len(errs) != 0 {
		t.Errorf("map overwrites so no duplicate, expected 0 errors, got %v", errs)
	}
}

func TestValidate_LogicalMissingType(t *testing.T) {
	bp := *validBP
	bp.Services["other"] = ServiceDef{
		Isolation: IsolationLogical,
		Image:     "postgres:16",
	}
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "is logical but missing type") {
		t.Errorf("expected missing type error, got %v", errs)
	}
}

func TestValidate_LogicalHasPorts(t *testing.T) {
	bp := *validBP
	bp.Services["bad"] = ServiceDef{
		Isolation: IsolationLogical,
		Type:      "postgres",
		Ports:     map[string]PortDef{"5432": {Var: "PGPORT"}},
	}
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "is logical but declares ports") {
		t.Errorf("expected declares ports error, got %v", errs)
	}
}

func TestValidate_PortVarConflict(t *testing.T) {
	bp := *validBP
	bp.Services["svc2"] = ServiceDef{
		Isolation: IsolationDedicated,
		Image:     "some-image",
		Ports:     map[string]PortDef{"9000": {Var: "REDIS_PORT"}},
	}
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "port var") {
		t.Errorf("expected port var collision error, got %v", errs)
	}
}

func TestValidate_DuplicateProcess(t *testing.T) {
	bp := *validBP
	bp.Processes = append(bp.Processes, ProcessDef{Name: "app"})
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "duplicate process") {
		t.Errorf("expected duplicate process error, got %v", errs)
	}
}

func TestValidate_PortVarProcessCollision(t *testing.T) {
	bp := *validBP
	bp.Services["compress"] = ServiceDef{
		Isolation: IsolationDedicated,
		Image:     "img",
		Ports:     map[string]PortDef{"5000": {Var: "PORT"}},
	}
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "port var") {
		t.Errorf("expected port var collision, got %v", errs)
	}
}

func TestValidate_DependsOnMissing(t *testing.T) {
	bp := *validBP
	bp.Processes = append(bp.Processes, ProcessDef{Name: "cron", Isolation: "native", Command: "echo hi", DependsOn: []string{"nonexistent"}})
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "depends_on") {
		t.Errorf("expected depends_on error, got %v", errs)
	}
}

func TestValidate_SeedCommandMissing(t *testing.T) {
	bp := *validBP
	bp.Seed.Command = ""
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "seed.command is required") {
		t.Errorf("expected seed.command error, got %v", errs)
	}
}

func TestValidate_SeedWorkdirMissing(t *testing.T) {
	bp := *validBP
	bp.Seed.Workdir = ""
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "seed.workdir is required") {
		t.Errorf("expected seed.workdir error, got %v", errs)
	}
}

func TestValidate_EnvTemplateMissing(t *testing.T) {
	bp := *validBP
	bp.Env.Template = ""
	bp.Env.Holes = map[string]string{"K": "v"}
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "env.template is required") {
		t.Errorf("expected env.template error, got %v", errs)
	}
}

func TestValidate_HoleNotInTemplate(t *testing.T) {
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, ".env.example")
	if err := os.WriteFile(tmplPath, []byte("EXISTING_KEY=hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bp := *validBP
	bp.Env.Template = tmplPath
	bp.Env.Holes = map[string]string{
		"EXISTING_KEY": "postgres://localhost:{{PORT}}/db",
		"MISSING_KEY":  "redis://localhost:{{REDIS_PORT}}/0",
	}
	errs := ValidateBlueprint(&bp)
	if !containsErr(errs, "hole") {
		t.Errorf("expected hole not found warning, got %v", errs)
	}
}

func containsErr(errs []error, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}
