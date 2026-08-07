package blueprint

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// nameCharset constrains service and process names: they become Docker
// container names, log filenames, and map keys, so slashes and dots are
// unsafe.
var nameCharset = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidateStructural reports errors that make a blueprint unsafe to execute:
// bad names, collisions, missing required config. It does not check whether
// hole keys appear in the env template — derivation appends missing holes, so
// that condition is a warning, not a fatal error. Lifecycle commands use this
// before producing side effects.
func ValidateStructural(bp *Blueprint) []error {
	var errs []error

	if bp.Version != 1 {
		errs = append(errs, fmt.Errorf("blueprint: unsupported version %d", bp.Version))
	}

	if bp.Name == "" {
		errs = append(errs, fmt.Errorf("blueprint: name is required"))
	}

	if bp.PortPool.Start < 1024 || bp.PortPool.End < 1024 {
		errs = append(errs, fmt.Errorf("blueprint: port_pool.start (%d) or end (%d) is a system port", bp.PortPool.Start, bp.PortPool.End))
	}
	if bp.PortPool.Start >= bp.PortPool.End {
		errs = append(errs, fmt.Errorf("blueprint: port_pool.start (%d) must be less than end (%d)", bp.PortPool.Start, bp.PortPool.End))
	}

	usedPortVars := map[string]string{}
	dockerNames := map[string]string{}
	for svcName, svc := range bp.Services {
		if !nameCharset.MatchString(svcName) {
			errs = append(errs, fmt.Errorf("blueprint: service name %q must match ^[a-z0-9][a-z0-9_-]*$", svcName))
		} else {
			dn := dockerName(svcName)
			if prev, ok := dockerNames[dn]; ok {
				errs = append(errs, fmt.Errorf("blueprint: services %q and %q both map to docker name %q", prev, svcName, dn))
			} else {
				dockerNames[dn] = svcName
			}
		}
		if svc.Isolation == IsolationLogical {
			if svc.Type == "" {
				errs = append(errs, fmt.Errorf("blueprint: service %q is logical but missing type", svcName))
			}
			if len(svc.Ports) > 0 {
				errs = append(errs, fmt.Errorf("blueprint: service %q is logical but declares ports", svcName))
			}
		}
		for portKey, pd := range svc.Ports {
			if pd.Var == "" {
				errs = append(errs, fmt.Errorf("blueprint: service %q port %s has empty var name", svcName, portKey))
				continue
			}
			if prev, ok := usedPortVars[pd.Var]; ok {
				errs = append(errs, fmt.Errorf("blueprint: port var %q used by services %q and %q", pd.Var, prev, svcName))
			} else {
				usedPortVars[pd.Var] = svcName
			}
		}
	}

	procNames := map[string]bool{}
	for _, proc := range bp.Processes {
		if !nameCharset.MatchString(proc.Name) {
			errs = append(errs, fmt.Errorf("blueprint: process name %q must match ^[a-z0-9][a-z0-9_-]*$", proc.Name))
		}
		if procNames[proc.Name] {
			errs = append(errs, fmt.Errorf("blueprint: duplicate process %q", proc.Name))
		}
		procNames[proc.Name] = true
	}
	for _, proc := range bp.Processes {
		if proc.PortVar != "" {
			if prev, ok := usedPortVars[proc.PortVar]; ok {
				errs = append(errs, fmt.Errorf("blueprint: port var %q collides with service %q port var", proc.PortVar, prev))
			}
			usedPortVars[proc.PortVar] = proc.Name
		}
		for _, dep := range proc.DependsOn {
			if !procNames[dep] {
				errs = append(errs, fmt.Errorf("blueprint: process %q depends_on %q which does not exist", proc.Name, dep))
			}
		}
	}

	if bp.Seed.Migrate == "" {
		errs = append(errs, fmt.Errorf("blueprint: seed.migrate is required"))
	}
	if bp.Seed.Command == "" {
		errs = append(errs, fmt.Errorf("blueprint: seed.command is required"))
	}
	if bp.Seed.Workdir == "" {
		errs = append(errs, fmt.Errorf("blueprint: seed.workdir is required"))
	}
	if len(bp.Env.Holes) > 0 && bp.Env.Template == "" {
		errs = append(errs, fmt.Errorf("blueprint: env.template is required"))
	}

	return errs
}

func ValidateBlueprint(bp *Blueprint) []error {
	errs := ValidateStructural(bp)

	if bp.Env.Template != "" && len(bp.Env.Holes) > 0 {
		errs = append(errs, checkHolesInTemplate(bp.Env.Template, bp.Env.Holes)...)
	}

	return errs
}

// dockerName mirrors the sanitization the docker driver applies to container
// names, so collisions can be detected before any container is created.
func dockerName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

func checkHolesInTemplate(templatePath string, holes map[string]string) []error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return []error{fmt.Errorf("blueprint: cannot read template file %q: %w", templatePath, err)}
	}
	content := string(data)
	var errs []error
	for key := range holes {
		if !lineHasKey(content, key) {
			errs = append(errs, fmt.Errorf("blueprint: hole %q not found in template file %q (warning)", key, templatePath))
		}
	}
	return errs
}

func lineHasKey(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, key+"=") || strings.HasPrefix(line, "export "+key+"=") {
			return true
		}
	}
	return false
}
