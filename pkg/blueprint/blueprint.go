// Package blueprint defines the schema for a repo's plax.json
// configuration, declaring services, processes, and isolation rules.
package blueprint

// Blueprint is the top-level config, stored as plax.json in the repo root.
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

// PortPool defines the inclusive range of host ports available for
// per-instance allocation. Ports below 1024 are system ports and rejected.
type PortPool struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// SeedConfig describes how to initialise the shared base database.
type SeedConfig struct {
	Migrate       string `json:"migrate"`
	Command       string `json:"command"`
	Workdir       string `json:"workdir"`
	MigrationsDir string `json:"migrations_dir,omitempty"`
}

// DatabaseDef declares a named database to clone from the base for a logical
// postgres service. From is set to "base" (the only supported source).
type DatabaseDef struct {
	Name string `json:"name"`
	From string `json:"from"`
}

// ServiceDef describes one service the repo needs (database, cache, etc).
type ServiceDef struct {
	Isolation ServiceIsolation   `json:"isolation"`
	Type      string             `json:"type,omitempty"`
	Image     string             `json:"image"`
	Env       map[string]string  `json:"env,omitempty"`
	Ports     map[string]PortDef `json:"ports,omitempty"`
	Command   []string           `json:"command,omitempty"`
	Databases []DatabaseDef      `json:"databases,omitempty"`
}

// PortDef maps a container port to an env var for hole resolution.
// Default is the host port from compose, used to pre-fill .env.example holes.
type PortDef struct {
	Var     string `json:"var"`
	Default string `json:"default,omitempty"`
}

// ServiceIsolation controls how a service's container and data are scoped
// per instance.
type ServiceIsolation string

const (
	// IsolationLogical creates one Postgres database per instance, sharing
	// one container across all instances. Supports template-clone for speed.
	IsolationLogical ServiceIsolation = "logical"
	// IsolationDedicated runs one container per instance with its own data.
	IsolationDedicated ServiceIsolation = "dedicated"
	// IsolationShared runs one container shared across all instances.
	IsolationShared ServiceIsolation = "shared"
	// IsolationExternal connects to an existing service outside plax control.
	IsolationExternal ServiceIsolation = "external"
	// IsolationNative spawns a native process in the worktree (not containerised).
	IsolationNative ServiceIsolation = "native"
)

// ProcessDef describes a native process to spawn in the worktree.
type ProcessDef struct {
	Name        string           `json:"name"`
	Isolation   ServiceIsolation `json:"isolation"`
	Command     string           `json:"command"`
	Workdir     string           `json:"workdir"`
	PortVar     string           `json:"port_var,omitempty"`
	DefaultPort int              `json:"default_port,omitempty"`
	DependsOn   []string         `json:"depends_on,omitempty"`
}

// EnvConfig controls per-instance .env derivation from a template,
// a set of template holes ({{VAR}}) that are filled with per-instance
// values, and a scrub list of keys whose real values must not reach
// instances.
type EnvConfig struct {
	Template string            `json:"template"`
	Holes    map[string]string `json:"holes"`
	Scrub    []string          `json:"scrub,omitempty"`
}
