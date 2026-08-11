package instance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/apollopower/plax/pkg/derive/env"
)

func Rederive(ctx context.Context, deps *Deps) error {
	if deps.Blueprint.Env.Template == "" {
		fmt.Fprintln(os.Stderr, "nothing to rederive: blueprint has no env template")
		return nil
	}

	templatePath := filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)
	if _, err := os.Stat(templatePath); err != nil {
		return fmt.Errorf("env: template not found at %s", templatePath)
	}

	userEnv := map[string]string{}
	userEnvPath := filepath.Join(deps.RepoRoot, ".env")
	if ue, err := env.ParseFileRaw(userEnvPath); err == nil {
		userEnv = ue
	}

	holeKeys := map[string]bool{}
	for k := range deps.Blueprint.Env.Holes {
		holeKeys[k] = true
	}

	names := make([]string, 0, len(deps.Registry.Instances))
	for name := range deps.Registry.Instances {
		names = append(names, name)
	}
	sort.Strings(names)

	changed := 0
	failed := 0

	for _, name := range names {
		rec := deps.Registry.Instances[name]

		values := map[string]string{}
		for varName, port := range rec.Ports {
			values[varName] = strconv.Itoa(port)
		}
		for key, physicalName := range rec.DBNames {
			if key == "" {
				values["DB_NAME"] = physicalName
			} else {
				values["DB_NAME_"+key] = physicalName
			}
		}

		instEnv := map[string]string{}
		instEnvPath := filepath.Join(rec.WorktreePath, ".env")
		if ie, err := env.ParseFileRaw(instEnvPath); err == nil {
			instEnv = ie
		}

		merged := map[string]string{}
		for k, v := range instEnv {
			if !holeKeys[k] {
				merged[k] = v
			}
		}
		for k, v := range userEnv {
			if !holeKeys[k] {
				merged[k] = v
			}
		}

		oldBytes, _ := os.ReadFile(instEnvPath)
		oldEnv, _ := env.ParseFileRaw(instEnvPath)

		tmpPath := filepath.Join(rec.WorktreePath, ".env.plax-tmp")
		if err := env.DeriveMerged(templatePath, merged, deps.Blueprint.Env.Holes, values, tmpPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", name, err)
			failed++
			_ = os.Remove(tmpPath)
			continue
		}

		newBytes, err := os.ReadFile(tmpPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: read tmp: %v\n", name, err)
			failed++
			_ = os.Remove(tmpPath)
			continue
		}
		newEnv, _ := env.ParseFileRaw(tmpPath)

		if string(newBytes) == string(oldBytes) {
			_ = os.Remove(tmpPath)
			continue
		}

		allKeys := make([]string, 0, len(newEnv)+len(oldEnv))
		for k := range newEnv {
			allKeys = append(allKeys, k)
		}
		for k := range oldEnv {
			if _, ok := newEnv[k]; !ok {
				allKeys = append(allKeys, k)
			}
		}
		sort.Strings(allKeys)
		var diffs strings.Builder
		for _, k := range allKeys {
			newVal, inNew := newEnv[k]
			oldVal, inOld := oldEnv[k]
			switch {
			case !inOld:
				fmt.Fprintf(&diffs, "  + %s=%s\n", k, newVal)
			case !inNew:
				fmt.Fprintf(&diffs, "  - %s=%s\n", k, oldVal)
			case oldVal != newVal:
				fmt.Fprintf(&diffs, "  - %s=%s\n", k, oldVal)
				fmt.Fprintf(&diffs, "  + %s=%s\n", k, newVal)
			}
		}
		if diffs.Len() == 0 {
			fmt.Fprintf(os.Stderr, "%s: formatting only — no key-level changes\n", name)
		} else {
			fmt.Printf("%s:\n%s", name, diffs.String())
		}

		if err := os.Rename(tmpPath, instEnvPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: rename: %v\n", name, err)
			failed++
			_ = os.Remove(tmpPath)
			continue
		}
		changed++
	}

	fmt.Fprintf(os.Stderr, "rederived %d of %d instance(s)\n", changed, len(names))
	if changed > 0 {
		fmt.Fprintf(os.Stderr, "note: restart instances to apply ('plax suspend <name>' && 'plax resume <name>')\n")
	}
	if failed > 0 {
		return fmt.Errorf("failed to rederive %d instance(s)", failed)
	}
	return nil
}
