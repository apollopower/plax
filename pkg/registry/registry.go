package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Registry struct {
	Version         int                       `json:"version"`
	BlueprintStamp  BlueprintStamp            `json:"blueprint_stamp"`
	Instances       map[string]InstanceRecord `json:"instances"`
	PortAllocations map[int]PortAllocation    `json:"port_allocations"`

	path string
}

type BlueprintStamp struct {
	ComposeHash    string `json:"compose_hash"`
	EnvExampleHash string `json:"env_example_hash"`
	ToolchainHash  string `json:"toolchain_hash"`
}

type InstanceRecord struct {
	ID           string            `json:"id"`
	Branch       string            `json:"branch"`
	WorktreePath string            `json:"worktree_path"`
	CreatedAt    time.Time         `json:"created_at"`
	State        string            `json:"state"`
	Ports        map[string]int    `json:"ports"`
	DBName       string            `json:"db_name"`
	ContainerIDs map[string]string `json:"container_ids,omitempty"`
	PIDs         map[string]int    `json:"pids,omitempty"`
	Provenance   Provenance        `json:"provenance"`
}

type PortAllocation struct {
	Instance string `json:"instance"`
	Service  string `json:"service"`
}

type Provenance struct {
	BaseVersion int    `json:"base_version"`
	Toolchain   string `json:"toolchain"`
}

// Opens an existing registry file or returns an empty, ready-to-use registry
// if the file does not exist. A separate Create function is unnecessary — the
// caller always wants a working registry, whether or not a file exists yet.
func Open(path string) (*Registry, error) {
	r := &Registry{
		Version:         1,
		BlueprintStamp:  BlueprintStamp{},
		Instances:       map[string]InstanceRecord{},
		PortAllocations: map[int]PortAllocation{},
		path:            path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("registry: reading %s: %w", path, err)
	}

	if err := json.Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("registry: parsing %s: %w", path, err)
	}

	if r.Instances == nil {
		r.Instances = map[string]InstanceRecord{}
	}
	if r.PortAllocations == nil {
		r.PortAllocations = map[int]PortAllocation{}
	}
	r.path = path

	return r, nil
}

func (r *Registry) Save() error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("registry: mkdir: %w", err)
	}

	// Write to a uniquely-named temp file then atomically rename into place,
	// so a crash never leaves a partial or corrupted registry on disk.
	f, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("registry: create tmp: %w", err)
	}
	tmpPath := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("registry: write tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("registry: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("registry: rename: %w", err)
	}

	return nil
}

func (r *Registry) AddInstance(id string, rec InstanceRecord) error {
	if rec.ID != "" && rec.ID != id {
		return fmt.Errorf("registry: record ID %q does not match key %q", rec.ID, id)
	}
	if _, exists := r.Instances[id]; exists {
		return fmt.Errorf("registry: instance %q already exists", id)
	}
	if rec.ID == "" {
		rec.ID = id
	}
	r.Instances[id] = rec
	return nil
}

func (r *Registry) RemoveInstance(id string) error {
	if _, exists := r.Instances[id]; !exists {
		return fmt.Errorf("registry: instance %q not found", id)
	}
	delete(r.Instances, id)
	for port, alloc := range r.PortAllocations {
		if alloc.Instance == id {
			delete(r.PortAllocations, port)
		}
	}
	return nil
}

func (r *Registry) GetInstance(id string) (InstanceRecord, bool) {
	rec, ok := r.Instances[id]
	return rec, ok
}

func (r *Registry) AllocPort(port int, inst, svc string) error {
	if _, exists := r.PortAllocations[port]; exists {
		return fmt.Errorf("registry: port %d already allocated", port)
	}
	r.PortAllocations[port] = PortAllocation{Instance: inst, Service: svc}
	return nil
}

func (r *Registry) ReleasePort(port int) {
	delete(r.PortAllocations, port)
}
