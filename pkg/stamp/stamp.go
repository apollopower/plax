// Package stamp computes and compares blueprint input stamps for drift
// detection. A stamp records the SHA-256 hashes of the three files that
// define the environment: docker-compose.yml, the env template, and the
// toolchain file. Comparing stamps tells you whether the blueprint's inputs
// have changed since the last operation.
package stamp

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/registry"
)

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}

// Compute returns the current BlueprintStamp for the given blueprint and
// repo root, hashing the three input files.
func Compute(root string, bp *blueprint.Blueprint) registry.BlueprintStamp {
	return registry.BlueprintStamp{
		ComposeHash:    hashFile(filepath.Join(root, "docker-compose.yml")),
		EnvExampleHash: hashFile(filepath.Join(root, bp.Env.Template)),
		ToolchainHash:  hashFile(filepath.Join(root, bp.Toolchain)),
	}
}

// Check compares the current stamp against the stored stamp in the registry.
// Returns a notice string and whether anything changed. Returns empty string
// and false when there is no stored stamp.
func Check(current registry.BlueprintStamp, stored registry.BlueprintStamp) (string, bool) {
	if stored.ComposeHash == "" && stored.EnvExampleHash == "" && stored.ToolchainHash == "" {
		return "", false
	}
	if stored.ComposeHash != current.ComposeHash ||
		stored.EnvExampleHash != current.EnvExampleHash ||
		stored.ToolchainHash != current.ToolchainHash {
		return "note: blueprint inputs changed since last 'plax up' — run 'plax doctor' for details", true
	}
	return "", false
}

// HasChanged returns true if the current stamp differs from the stored one.
func HasChanged(current, stored registry.BlueprintStamp) bool {
	if stored.ComposeHash == "" && stored.EnvExampleHash == "" && stored.ToolchainHash == "" {
		return false
	}
	return stored.ComposeHash != current.ComposeHash ||
		stored.EnvExampleHash != current.EnvExampleHash ||
		stored.ToolchainHash != current.ToolchainHash
}

// ChangedInputs returns which inputs have changed, or nil if none.
func ChangedInputs(current, stored registry.BlueprintStamp) []string {
	if !HasChanged(current, stored) {
		return nil
	}
	var changes []string
	if stored.ComposeHash != current.ComposeHash {
		changes = append(changes, "docker-compose.yml changed since the last 'plax up' — recheck the blueprint")
	}
	if stored.EnvExampleHash != current.EnvExampleHash {
		changes = append(changes, ".env example changed since the last 'plax up' — recheck the blueprint")
	}
	if stored.ToolchainHash != current.ToolchainHash {
		changes = append(changes, "toolchain file changed since the last 'plax up' — recheck the blueprint")
	}
	return changes
}
