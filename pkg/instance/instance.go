// Package instance orchestrates the creation and destruction of Plax
// instances, wiring together the blueprint, registry, port allocator,
// Postgres driver, and Docker driver.
package instance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/registry"
)

// BaseManager is the subset of postgres.BaseManager that lifecycle
// orchestration needs. An interface so tests can fake it.
type BaseManager interface {
	BaseStatus(ctx context.Context) (postgres.BaseInfo, error)
	CloneBase(ctx context.Context, targetDB string) error
	DropInstanceDB(ctx context.Context, dbName string) error
	InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error)
	InstanceDBExists(ctx context.Context, dbName string) (bool, error)
	AppliedMigrations(ctx context.Context, dbName string) ([]string, error)
}

// DockerDriver is the subset of docker.Driver that lifecycle orchestration
// needs. An interface so tests can fake it.
type DockerDriver interface {
	CreateNetwork(ctx context.Context, name string) error
	RemoveNetwork(ctx context.Context, name string) error
	RunService(ctx context.Context, cfg docker.ServiceConfig) (string, error)
	StartService(ctx context.Context, containerID string) (bool, error)
	StopService(ctx context.Context, containerID string) error
	RemoveService(ctx context.Context, containerID string) error
	ServiceRunning(ctx context.Context, containerID string) (bool, error)
	ServiceExists(ctx context.Context, containerID string) (bool, error)
}

// Deps holds the dependencies for instance lifecycle operations.
// Assembled by the CLI layer and passed to Up/Down.
//
// Not every field is needed by every command:
//
//	Up:    all fields (nil causes a panic — do not call Up with partial Deps)
//	Down:  Blueprint and Pool unused; BM and Docker may be nil, in which case
//	       Down skips those resources with a warning and continues teardown
//	Resume: BM optional — nil skips DB checks
//	ls:    Registry, RepoRoot
//	attach/exec: Registry, RepoRoot
type Deps struct {
	Blueprint *blueprint.Blueprint
	Registry  *registry.Registry
	Pool      *portpool.PortPool
	BM        BaseManager
	Docker    DockerDriver
	RepoRoot  string // absolute path to the repo root

	// SourceRef is the original --ref value the user passed (empty if none).
	// Stored in the registry record for provenance.
	SourceRef string

	// ResolvedRef is the git ref to branch from (resolved from SourceRef by
	// worktree.ResolveRef). Empty means branch from repo-root HEAD.
	ResolvedRef string
}

// Instance names are embedded in Postgres database names as unquoted
// identifiers, so hyphens are not allowed even though git and Docker would
// accept them.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validateName checks that an instance name is safe for git branches,
// Docker containers, Postgres databases, and filesystem paths.
func validateName(name string) error {
	if len(name) == 0 || len(name) > 32 {
		return fmt.Errorf("invalid instance name %q: must be 1-32 characters", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name %q: must match ^[a-z][a-z0-9_]*$", name)
	}
	return nil
}

// NewUpDeps validates and assembles the dependencies that Up requires.
// Down does not use this constructor — it builds its own tolerant, partial
// deps that tolerate nil BM and Docker.
func NewUpDeps(bp *blueprint.Blueprint, reg *registry.Registry, pool *portpool.PortPool, bm BaseManager, docker DockerDriver, root string) (*Deps, error) {
	if bp == nil {
		return nil, fmt.Errorf("instance: blueprint is required")
	}
	if reg == nil {
		return nil, fmt.Errorf("instance: registry is required")
	}
	if pool == nil {
		return nil, fmt.Errorf("instance: port pool is required")
	}
	if bm == nil {
		return nil, fmt.Errorf("instance: base manager is required")
	}
	if docker == nil {
		return nil, fmt.Errorf("instance: docker driver is required")
	}
	if root == "" {
		return nil, fmt.Errorf("instance: repo root is required")
	}
	return &Deps{
		Blueprint: bp,
		Registry:  reg,
		Pool:      pool,
		BM:        bm,
		Docker:    docker,
		RepoRoot:  root,
	}, nil
}

// hashFile returns the SHA-256 hex digest of a file, or empty string if
// the file does not exist.
func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}
