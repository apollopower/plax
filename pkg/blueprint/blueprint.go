package blueprint

type Blueprint struct {
	Version   int                   `json:"version"`
	Name      string                `json:"name"`
	PortPool  PortPool              `json:"port_pool"`
	Toolchain string                `json:"toolchain"`
	Seed      SeedConfig            `json:"seed"`
	Services  map[string]ServiceDef `json:"services"`
	Processes []ProcessDef          `json:"processes"`
	Env       EnvConfig             `json:"env"`
}

type PortPool struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type SeedConfig struct {
	Migrate       string `json:"migrate"`
	Command       string `json:"command"`
	Workdir       string `json:"workdir"`
	MigrationsDir string `json:"migrations_dir,omitempty"`
}

type ServiceDef struct {
	Isolation ServiceIsolation   `json:"isolation"`
	Type      string             `json:"type,omitempty"`
	Image     string             `json:"image"`
	Env       map[string]string  `json:"env,omitempty"`
	Ports     map[string]PortDef `json:"ports,omitempty"`
	Command   []string           `json:"command,omitempty"`
}

type PortDef struct {
	Var     string `json:"var"`
	Default string `json:"default,omitempty"`
}

type ServiceIsolation string

const (
	IsolationLogical   ServiceIsolation = "logical"
	IsolationDedicated ServiceIsolation = "dedicated"
	IsolationShared    ServiceIsolation = "shared"
	IsolationExternal  ServiceIsolation = "external"
	IsolationNative    ServiceIsolation = "native"
)

type ProcessDef struct {
	Name        string           `json:"name"`
	Isolation   ServiceIsolation `json:"isolation"`
	Command     string           `json:"command"`
	Workdir     string           `json:"workdir"`
	PortVar     string           `json:"port_var,omitempty"`
	DefaultPort int              `json:"default_port,omitempty"`
	DependsOn   []string         `json:"depends_on,omitempty"`
}

type EnvConfig struct {
	Template string            `json:"template"`
	Holes    map[string]string `json:"holes"`
}
