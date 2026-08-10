// Package status collects and reports instance health information.
package status

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/toolchain"
	"github.com/apollopower/plax/pkg/worktree"
)

type Level string

const (
	OK      Level = "ok"
	Drift   Level = "drift"
	Unknown Level = "unknown"
)

type Dimension struct {
	Name   string `json:"-"`
	Level  Level  `json:"status"`
	Detail string `json:"detail"`
}

type Report struct {
	Instance string    `json:"instance"`
	State    string    `json:"state"`
	Code     Dimension `json:"code"`
	Schema   Dimension `json:"schema"`
	Data     Dimension `json:"data"`
	Host     Dimension `json:"host"`
	Config   Dimension `json:"config"`
}

type BaseManager interface {
	BaseStatus(ctx context.Context) (postgres.BaseInfo, error)
	InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error)
}

type Deps struct {
	Blueprint    *blueprint.Blueprint
	Registry     *registry.Registry
	BM           BaseManager
	RepoRoot     string
	CurrentStamp registry.BlueprintStamp
}

func Build(ctx context.Context, deps *Deps, name string) (*Report, error) {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return nil, fmt.Errorf("instance %q not found", name)
	}

	report := &Report{
		Instance: name,
		State:    string(rec.State),
	}

	base := resolveBase(&rec, deps.RepoRoot)

	report.Code = codeDrift(deps.RepoRoot, base, &rec)
	report.Host = hostDrift(deps.RepoRoot, deps.Blueprint, &rec)
	report.Config = configDrift(deps.Registry, deps.CurrentStamp)

	migrationsDir := deps.Blueprint.Seed.MigrationsDir
	if migrationsDir == "" {
		migrationsDir = filepath.Join("src", "db", "migrations")
	}

	var prov *postgres.ProvenanceRow
	var provErr error
	if deps.BM != nil {
		prov, provErr = deps.BM.InstanceProvenance(ctx, rec.DBName)
	}

	if provErr != nil {
		report.Schema = Dimension{Name: "schema", Level: Unknown, Detail: fmt.Sprintf("instance provenance: %v", provErr)}
		report.Data = Dimension{Name: "data", Level: Unknown, Detail: fmt.Sprintf("instance provenance: %v", provErr)}
	} else {
		report.Schema = schemaDrift(deps.RepoRoot, base, migrationsDir, deps.BM, &rec, prov)
		report.Data = dataDrift(ctx, deps.BM, &rec, prov)
	}

	return report, nil
}

func resolveBase(rec *registry.InstanceRecord, repoRoot string) string {
	if rec.BaseRef != "" && worktree.RefExists(repoRoot, rec.BaseRef) {
		return rec.BaseRef
	}
	if rec.BaseCommit != "" {
		return rec.BaseCommit
	}
	for _, cand := range []string{"main", "master", "origin/HEAD"} {
		if worktree.RefExists(repoRoot, cand) {
			return cand
		}
	}
	return ""
}

func codeDrift(repoRoot, base string, rec *registry.InstanceRecord) Dimension {
	d := Dimension{Name: "code"}
	if base == "" {
		d.Level = Unknown
		d.Detail = "no base ref recorded"
		return d
	}

	rhs := rec.Branch
	var detailSuffix string

	if rec.WorktreePath != "" {
		wtRef, wtCommit, err := worktree.WorktreeHead(rec.WorktreePath)
		if err != nil {
			detailSuffix = " (worktree path missing, using recorded branch)"
		} else if wtRef != "" && wtRef == rec.Branch {
			// Fast path: worktree is on the recorded branch.
		} else {
			branchCmd := exec.Command("git", "rev-parse", rec.Branch)
			branchCmd.Dir = repoRoot
			branchOut, branchErr := branchCmd.Output()
			if branchErr == nil && strings.TrimSpace(string(branchOut)) == wtCommit {
				// Same commit as rec.Branch — fast path, use branch name.
			} else if wtRef != "" {
				rhs = wtRef
				detailSuffix = fmt.Sprintf(" (on %s)", wtRef)
			} else {
				rhs = wtCommit
				short := wtCommit
				if len(short) > 7 {
					short = short[:7]
				}
				detailSuffix = fmt.Sprintf(" (detached at %s)", short)
			}
		}
	} else {
		detailSuffix = " (no worktree path)"
	}

	ahead, behind, err := worktree.AheadBehind(repoRoot, base, rhs)
	if err != nil {
		d.Level = Unknown
		d.Detail = fmt.Sprintf("git error: %v", err)
		return d
	}

	if ahead == 0 && behind == 0 {
		d.Level = OK
		d.Detail = fmt.Sprintf("up to date with %s%s", base, detailSuffix)
	} else {
		d.Level = Drift
		d.Detail = fmt.Sprintf("ahead %d, behind %d%s", ahead, behind, detailSuffix)
	}
	return d
}

func hostDrift(repoRoot string, bp *blueprint.Blueprint, rec *registry.InstanceRecord) Dimension {
	d := Dimension{Name: "host"}
	if rec.Provenance.ToolVersions == nil {
		d.Level = Unknown
		d.Detail = "recorded before Phase 4"
		return d
	}

	toolchainPath := filepath.Join(repoRoot, bp.Toolchain)
	pins, err := toolchain.ParsePins(toolchainPath)
	if err != nil || pins == nil {
		d.Level = Unknown
		d.Detail = "no toolchain file"
		return d
	}

	current := toolchain.ResolveVersions(pins)
	diffs := toolchain.CompareVersions(rec.Provenance.ToolVersions, current)
	if len(diffs) == 0 {
		var tools []string
		for t := range pins {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		var parts []string
		for _, t := range tools {
			if ver, ok := current[t]; ok {
				parts = append(parts, fmt.Sprintf("%s@%s", t, ver))
			}
		}
		d.Level = OK
		d.Detail = fmt.Sprintf("toolchain unchanged (%s)", strings.Join(parts, ", "))
	} else {
		var parts []string
		for _, diff := range diffs {
			switch {
			case diff.Recorded == "":
				parts = append(parts, fmt.Sprintf("%s %s present", diff.Tool, diff.Current))
			case diff.Current == "":
				parts = append(parts, fmt.Sprintf("%s %s -> missing", diff.Tool, diff.Recorded))
			default:
				parts = append(parts, fmt.Sprintf("%s %s -> %s", diff.Tool, diff.Recorded, diff.Current))
			}
		}
		d.Level = Drift
		d.Detail = strings.Join(parts, ", ")
	}
	return d
}

func configDrift(reg *registry.Registry, current registry.BlueprintStamp) Dimension {
	d := Dimension{Name: "config"}
	stored := reg.BlueprintStamp
	if stored.ComposeHash == "" && stored.EnvExampleHash == "" && stored.ToolchainHash == "" {
		d.Level = Unknown
		d.Detail = "no stamp recorded"
		return d
	}

	var diffs []string
	if stored.ComposeHash != current.ComposeHash {
		diffs = append(diffs, "docker-compose.yml changed")
	}
	if stored.EnvExampleHash != current.EnvExampleHash {
		diffs = append(diffs, ".env example changed")
	}
	if stored.ToolchainHash != current.ToolchainHash {
		diffs = append(diffs, "toolchain file changed")
	}

	if len(diffs) == 0 {
		d.Level = OK
		d.Detail = "compose, env template, toolchain unchanged"
	} else {
		d.Level = Drift
		d.Detail = strings.Join(diffs, "; ")
	}
	return d
}

func schemaDrift(repoRoot, base, migrationsDir string, bm BaseManager, rec *registry.InstanceRecord, prov *postgres.ProvenanceRow) Dimension {
	d := Dimension{Name: "schema"}
	if bm == nil || prov == nil || prov.SchemaHash == "" {
		d.Level = Unknown
		switch {
		case bm == nil:
			d.Detail = "postgres unreachable"
		case prov == nil:
			d.Detail = "no provenance row"
		default:
			d.Detail = "no schema hash recorded"
		}
		return d
	}

	if base == "" {
		d.Level = Unknown
		d.Detail = "no base ref to compare against"
		return d
	}

	ref := base
	usingWorktreeHead := false

	if rec.WorktreePath != "" {
		_, wtCommit, err := worktree.WorktreeHead(rec.WorktreePath)
		if err == nil && wtCommit != "" {
			ref = wtCommit
			usingWorktreeHead = true
		}
	}

	names, err := worktree.SchemaFilesAtRef(repoRoot, ref, migrationsDir)
	if err != nil || len(names) == 0 {
		d.Level = Unknown
		if err != nil {
			d.Detail = fmt.Sprintf("git ls-tree: %v", err)
		} else {
			d.Detail = fmt.Sprintf("no migration files at %s on %s", migrationsDir, ref)
		}
		return d
	}
	repoHash := postgres.HashMigrationNames(names)

	if prov.SchemaHash == repoHash {
		d.Level = OK
		if usingWorktreeHead {
			d.Detail = "migrations match worktree HEAD"
		} else {
			d.Detail = fmt.Sprintf("migrations match %s", base)
		}
	} else {
		d.Level = Drift
		if usingWorktreeHead {
			d.Detail = "database was built from a different migration set than worktree HEAD declares — re-migrate the instance, or 'plax down' + 'plax up' to rebuild from a refreshed base"
		} else {
			d.Detail = fmt.Sprintf("database was built from a different migration set than %s declares — re-migrate the instance, or 'plax down' + 'plax up' to rebuild from a refreshed base", base)
		}
	}
	return d
}

func dataDrift(ctx context.Context, bm BaseManager, rec *registry.InstanceRecord, prov *postgres.ProvenanceRow) Dimension {
	d := Dimension{Name: "data"}
	if bm == nil {
		d.Level = Unknown
		d.Detail = "postgres unreachable"
		return d
	}
	if prov == nil || prov.Version == 0 {
		d.Level = Unknown
		switch prov {
		case nil:
			d.Detail = "no provenance row in base — run 'plax base reset' to repair"
		default:
			d.Detail = "no provenance version recorded"
		}
		return d
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		d.Level = Unknown
		d.Detail = fmt.Sprintf("base status: %v", err)
		return d
	}
	if !info.Exists {
		d.Level = Unknown
		d.Detail = "base database missing"
		return d
	}

	if prov.Version == info.ProvenanceVer {
		d.Level = OK
		d.Detail = fmt.Sprintf("built from base v%d (current)", prov.Version)
	} else {
		d.Level = Drift
		d.Detail = fmt.Sprintf("built from base v%d — base is now v%d (stale; 'plax down' + 'plax up' to rebuild from the new base)", prov.Version, info.ProvenanceVer)
	}
	return d
}
