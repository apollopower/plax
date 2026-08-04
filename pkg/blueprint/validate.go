package blueprint

import (
	"fmt"
	"os"
	"strings"
)

func ValidateBlueprint(bp *Blueprint) []error {
	var errs []error

	if bp.Version != 1 {
		errs = append(errs, fmt.Errorf("blueprint: unsupported version %d", bp.Version))
	}

	if bp.Name == "" {
		errs = append(errs, fmt.Errorf("blueprint: name is required"))
	}

	if bp.PortPool.Start < 1024 || bp.PortPool.End < 1024 {
		errs = append(errs, fmt.Errorf("blueprint: port_pool range invalid"))
	}
	if bp.PortPool.Start >= bp.PortPool.End {
		errs = append(errs, fmt.Errorf("blueprint: port_pool range invalid"))
	}

	usedPortVars := map[string]string{}
	for svcName, svc := range bp.Services {
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

	if bp.Seed.Command == "" {
		errs = append(errs, fmt.Errorf("blueprint: seed.command is required"))
	}
	if bp.Seed.Workdir == "" {
		errs = append(errs, fmt.Errorf("blueprint: seed.workdir is required"))
	}
	if len(bp.Env.Holes) > 0 && bp.Env.Template == "" {
		errs = append(errs, fmt.Errorf("blueprint: env.template is required"))
	}

	if bp.Env.Template != "" && len(bp.Env.Holes) > 0 {
		errs = append(errs, checkHolesInTemplate(bp.Env.Template, bp.Env.Holes)...)
	}

	return errs
}

func checkHolesInTemplate(templatePath string, holes map[string]string) []error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return []error{fmt.Errorf("blueprint: cannot read template file %q: %w", templatePath, err)}
	}
	content := string(data)
	var warns []error
	for key := range holes {
		if !lineHasKey(content, key) {
			warns = append(warns, fmt.Errorf("blueprint: hole %q not found in template file %q (warning)", key, templatePath))
		}
	}
	return warns
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
