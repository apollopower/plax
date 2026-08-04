package blueprint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

type composeService struct {
	Image       string `yaml:"image"`
	Ports       []any  `yaml:"ports"`
	Volumes     []any  `yaml:"volumes"`
	Environment any    `yaml:"environment"` // map or list
	Command     any    `yaml:"command"`     // string or list
}

func InitFromRepo(root string) (*Blueprint, error) {
	bp := &Blueprint{
		Version:   1,
		PortPool:  PortPool{Start: 3000, End: 4000},
		Toolchain: ".tool-versions",
		Seed: SeedConfig{
			Command: "TODO: add seed command, e.g. 'bun run db fixtures'",
			Workdir: ".",
		},
		Env: EnvConfig{
			Template: ".env.example",
			Holes:    map[string]string{},
		},
		Services:  map[string]ServiceDef{},
		Processes: defaultProcesses(),
	}

	composePath := filepath.Join(root, "docker-compose.yml")
	if _, err := os.Stat(composePath); err != nil {
		return nil, fmt.Errorf("init: docker-compose.yml not found at %s", composePath)
	}

	svcs, err := parseComposeFile(composePath)
	if err != nil {
		return nil, fmt.Errorf("init: invalid compose YAML: %w", err)
	}

	for name, s := range svcs {
		if s.Image == "" {
			fmt.Fprintf(os.Stderr, "init: service %q has no image, skipping\n", name)
			continue
		}
		def := buildServiceDef(name, s)
		bp.Services[name] = def
	}

	portVarMap := buildPortVarMap(bp.Services, bp.Processes)

	envPath := filepath.Join(root, ".env.example")
	envVars, err := parseEnvExample(envPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: .env.example not found at %s — env.holes will be empty\n", envPath)
		envVars = map[string]string{}
	}

	bp.Env.Holes = detectHoles(envVars, bp.Services, portVarMap)

	bp.Name = filepath.Base(root)

	return bp, nil
}

func parseComposeFile(path string) (map[string]composeService, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cf struct {
		Services yaml.MapSlice `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, err
	}

	result := map[string]composeService{}
	for _, item := range cf.Services {
		name, ok := item.Key.(string)
		if !ok {
			continue
		}
		svcData, err := yaml.Marshal(item.Value)
		if err != nil {
			continue
		}
		var svc composeService
		if err := yaml.Unmarshal(svcData, &svc); err != nil {
			fmt.Fprintf(os.Stderr, "init: warning: service %q has unparseable definition: %v\n", name, err)
			continue
		}
		result[name] = svc
	}

	return result, nil
}

var composePortExpr = regexp.MustCompile(
	`^\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-(\d+))?\}(?::(\d+))?$`,
)

var barePortExpr = regexp.MustCompile(`^(\d+):(\d+)$`)

func buildServiceDef(name string, s composeService) ServiceDef {
	def := ServiceDef{
		Image: s.Image,
		Env:   parseEnvironment(s.Environment),
	}

	if img := strings.ToLower(s.Image); strings.Contains(img, "postgres") || strings.Contains(img, "pgvector") {
		def.Isolation = IsolationLogical
		def.Type = "postgres"
		def.Ports = nil
	} else if len(s.Volumes) > 0 {
		def.Isolation = IsolationDedicated
		def.Ports = buildPorts(s.Ports, name)
	} else {
		def.Isolation = IsolationShared
		def.Ports = buildPorts(s.Ports, name)
		fmt.Fprintf(os.Stderr, "init: service %q has no volumes, defaulting to shared — verify isolation\n", name)
	}

	if s.Command != nil {
		def.Command = normalizeCommand(s.Command)
	}

	return def
}

func buildPorts(ports []any, svcName string) map[string]PortDef {
	result := map[string]PortDef{}
	for _, p := range ports {
		portStr := fmt.Sprint(p)

		varName := ""
		containerPort := ""
		defaultHostPort := ""

		if m := composePortExpr.FindStringSubmatch(portStr); m != nil {
			varName = m[1]
			defaultHostPort = m[2]
			containerPort = m[3]
		} else if m := barePortExpr.FindStringSubmatch(portStr); m != nil {
			containerPort = m[2]
		} else if _, err := fmt.Sscanf(portStr, "%d", new(int)); err == nil {
			containerPort = portStr
		} else {
			fmt.Fprintf(os.Stderr, "init: warning: unparseable port %q, skipping\n", portStr)
			continue
		}

		if containerPort == "" {
			fmt.Fprintf(os.Stderr, "init: warning: unparseable port %q, skipping\n", portStr)
			continue
		}

		if varName == "" {
			varName = strings.ToUpper(svcName) + "_PORT"
		}

		result[containerPort] = PortDef{Var: varName, Default: defaultHostPort}
	}
	return result
}

func parseEnvironment(env any) map[string]string {
	if env == nil {
		return nil
	}
	switch e := env.(type) {
	case map[string]any:
		m := map[string]string{}
		for k, v := range e {
			m[k] = fmt.Sprint(v)
		}
		return m
	case map[any]any:
		m := map[string]string{}
		for k, v := range e {
			m[fmt.Sprint(k)] = fmt.Sprint(v)
		}
		return m
	case []any:
		m := map[string]string{}
		for _, item := range e {
			s := fmt.Sprint(item)
			if eq := strings.IndexByte(s, '='); eq > 0 {
				m[s[:eq]] = s[eq+1:]
			}
		}
		return m
	}
	return nil
}

func normalizeCommand(cmd any) []string {
	switch c := cmd.(type) {
	case string:
		return strings.Fields(c)
	case []any:
		out := make([]string, len(c))
		for i, v := range c {
			out[i] = fmt.Sprint(v)
		}
		return out
	}
	return nil
}

func parseEnvExample(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	vars := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		if commentIdx := findComment(val); commentIdx >= 0 {
			val = strings.TrimSpace(val[:commentIdx])
		}

		val = strings.Trim(val, `"'`)
		vars[key] = val
	}
	return vars, nil
}

func findComment(s string) int {
	inQuote := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\'' {
			inQuote = !inQuote
		}
		if !inQuote && s[i] == '#' {
			return i
		}
	}
	return -1
}

func buildPortVarMap(services map[string]ServiceDef, processes []ProcessDef) map[string]string {
	m := map[string]string{}
	for _, svc := range services {
		for _, pd := range svc.Ports {
			if pd.Default != "" {
				m[pd.Default] = pd.Var
			}
		}
	}
	for _, proc := range processes {
		if proc.PortVar != "" && proc.DefaultPort > 0 {
			m[fmt.Sprint(proc.DefaultPort)] = proc.PortVar
		}
	}
	return m
}

func detectHoles(envVars map[string]string, services map[string]ServiceDef, portVarMap map[string]string) map[string]string {
	hasLogicalPostgres := false
	for _, svc := range services {
		if svc.Isolation == IsolationLogical && svc.Type == "postgres" {
			hasLogicalPostgres = true
			break
		}
	}

	portRef := regexp.MustCompile(`localhost:(\d+)`)

	holes := map[string]string{}
	for key, val := range envVars {
		matches := portRef.FindAllStringSubmatch(val, -1)
		if len(matches) == 0 {
			continue
		}

		template := val
		replaced := false
		for _, m := range matches {
			portNum := m[1]
			if portNum == "5432" && hasLogicalPostgres {
				continue
			}

			varName, known := portVarMap[portNum]
			if !known {
				varName = "FIXME_PORT_" + portNum
			}

			pat := regexp.MustCompile(`localhost:` + regexp.QuoteMeta(portNum) + `\b`)
			template = pat.ReplaceAllString(template, "localhost:{{"+varName+"}}")
			replaced = true
		}

		if replaced {
			holes[key] = template
		}
	}
	return holes
}

func defaultProcesses() []ProcessDef {
	return []ProcessDef{
		{
			Name:        "app",
			Isolation:   IsolationNative,
			Command:     "bun run dev:app",
			Workdir:     ".",
			PortVar:     "PORT",
			DefaultPort: 3000,
		},
		{
			Name:      "workers",
			Isolation: IsolationNative,
			Command:   "bun run dev:workers",
			Workdir:   ".",
			DependsOn: []string{"app"},
		},
	}
}
