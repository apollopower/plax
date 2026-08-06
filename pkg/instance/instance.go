// Package instance orchestrates the creation and destruction of Plax
// instances, wiring together the blueprint, registry, port allocator,
// Postgres driver, and Docker driver.
package instance

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/registry"
)

// Deps holds the dependencies for instance lifecycle operations.
// Assembled by the CLI layer and passed to Up/Down.
//
// Not every field is needed by every command:
//
//	Up:    all fields
//	Down:  all fields
//	ls:    Registry, RepoRoot
//	attach/exec: Registry, RepoRoot
//
// The CLI layer populates only the fields each command requires.
// Nil fields must not be dereferenced — Up and Down use all fields.
type Deps struct {
	Blueprint *blueprint.Blueprint
	Registry  *registry.Registry
	Pool      *portpool.PortPool
	BM        *postgres.BaseManager
	Docker    *docker.Driver
	RepoRoot  string // absolute path to the repo root
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// validateName checks that an instance name is safe for git branches,
// Docker containers, Postgres databases, and filesystem paths.
func validateName(name string) error {
	if len(name) == 0 || len(name) > 32 {
		return fmt.Errorf("invalid instance name %q: must be 1-32 characters", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name %q: must match ^[a-z][a-z0-9_-]*$", name)
	}
	return nil
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

// computeBlueprintStamp hashes the files that the blueprint was derived from.
func computeBlueprintStamp(repoRoot string, bp *blueprint.Blueprint) registry.BlueprintStamp {
	return registry.BlueprintStamp{
		ComposeHash:    hashFile(filepath.Join(repoRoot, "docker-compose.yml")),
		EnvExampleHash: hashFile(filepath.Join(repoRoot, bp.Env.Template)),
		ToolchainHash:  hashFile(filepath.Join(repoRoot, bp.Toolchain)),
	}
}
