package doctor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/toolchain"
	"github.com/apollopower/plax/pkg/worktree"
	"github.com/goccy/go-yaml"
)

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}

type Level string

const (
	Pass Level = "ok"
	Warn Level = "warn"
	Fail Level = "fail"
)

type Check struct {
	Area    string `json:"area"`
	Level   Level  `json:"level"`
	Message string `json:"message"`
}

type Report struct {
	Checks []Check `json:"checks"`
}

func (r *Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Level == Fail {
			return true
		}
	}
	return false
}

type BaseManager interface {
	BaseStatus(ctx context.Context) (postgres.BaseInfo, error)
	InstanceDBExists(ctx context.Context, dbName string) (bool, error)
}

type DockerDriver interface {
	ServiceExists(ctx context.Context, containerID string) (bool, error)
	ServiceRunning(ctx context.Context, containerID string) (bool, error)
}

type Deps struct {
	Blueprint *blueprint.Blueprint
	Registry  *registry.Registry
	BM        BaseManager
	Docker    DockerDriver
	RepoRoot  string
}

func Run(ctx context.Context, deps *Deps) *Report {
	r := &Report{}
	runBlueprintVsRepo(r, deps)
	runBlueprintVsRegistry(ctx, r, deps)
	runRepoVsMachine(r, deps)
	runBase(ctx, r, deps)
	return r
}

func runBlueprintVsRepo(r *Report, deps *Deps) {
	area := "blueprint-vs-repo"
	hasFail := false

	errs := blueprint.ValidateStructural(deps.Blueprint)
	for _, e := range errs {
		r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: e.Error()})
		hasFail = true
	}

	warns := blueprint.ValidateBlueprint(deps.Blueprint)
	for _, w := range warns {
		r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: w.Error()})
	}

	composePath := filepath.Join(deps.RepoRoot, "docker-compose.yml")
	data, composeErr := os.ReadFile(composePath)
	if composeErr != nil {
		r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: "docker-compose.yml missing or unreadable"})
	} else {
		var parsed map[string]any
		if yamlErr := yaml.Unmarshal(data, &parsed); yamlErr == nil {
			services, _ := parsed["services"].(map[string]any)
			bpSvcs := map[string]string{}
			for name, svc := range deps.Blueprint.Services {
				bpSvcs[name] = svc.Image
			}
			for cName, cDef := range services {
				img := ""
				if m, ok := cDef.(map[string]any); ok {
					if i, ok := m["image"]; ok {
						if s, ok := i.(string); ok {
							img = s
						}
					}
				}
				if _, ok := bpSvcs[cName]; !ok {
					r.Checks = append(r.Checks, Check{
						Area: area, Level: Warn,
						Message: fmt.Sprintf("compose service %s (%s) is not in the blueprint", cName, img),
					})
				}
			}
			for bpName := range bpSvcs {
				if _, ok := services[bpName]; !ok {
					r.Checks = append(r.Checks, Check{
						Area: area, Level: Warn,
						Message: fmt.Sprintf("service %s not found in docker-compose.yml — blueprint may need a recheck", bpName),
					})
				}
			}
		} else {
			r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: fmt.Sprintf("docker-compose.yml unparseable: %v", yamlErr)})
		}
	}

	stored := deps.Registry.BlueprintStamp
	if stored.ComposeHash != "" || stored.EnvExampleHash != "" || stored.ToolchainHash != "" {
		current := registry.BlueprintStamp{
			ComposeHash:    hashFile(filepath.Join(deps.RepoRoot, "docker-compose.yml")),
			EnvExampleHash: hashFile(filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)),
			ToolchainHash:  hashFile(filepath.Join(deps.RepoRoot, deps.Blueprint.Toolchain)),
		}
		if stored.ComposeHash != current.ComposeHash {
			r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: "docker-compose.yml changed since the last 'plax up' — recheck the blueprint"})
		}
		if stored.EnvExampleHash != current.EnvExampleHash {
			r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: ".env example changed since the last 'plax up' — recheck the blueprint"})
		}
		if stored.ToolchainHash != current.ToolchainHash {
			r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: "toolchain file changed since the last 'plax up' — recheck the blueprint"})
		}
	}

	if !hasFail {
		r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: "blueprint parses and validates"})
	}
}

func runBlueprintVsRegistry(ctx context.Context, r *Report, deps *Deps) {
	area := "blueprint-vs-registry"

	for port, alloc := range deps.Registry.PortAllocations {
		if _, found := deps.Registry.GetInstance(alloc.Instance); !found {
			r.Checks = append(r.Checks, Check{
				Area: area, Level: Fail,
				Message: fmt.Sprintf("port %d allocated to unknown instance %q — remove it from .plax/registry.json", port, alloc.Instance),
			})
		}
	}

	bpSvcs := map[string]bool{}
	for name := range deps.Blueprint.Services {
		bpSvcs[name] = true
	}
	bpProcs := map[string]bool{}
	for _, p := range deps.Blueprint.Processes {
		bpProcs[p.Name] = true
	}

	for port, alloc := range deps.Registry.PortAllocations {
		if !bpSvcs[alloc.Service] && !bpProcs[alloc.Service] {
			r.Checks = append(r.Checks, Check{
				Area: area, Level: Warn,
				Message: fmt.Sprintf("port %d allocated to %s/%s but the blueprint declares no such service", port, alloc.Instance, alloc.Service),
			})
		}
	}

	hasFail := false
	count := 0
	for name, rec := range deps.Registry.Instances {
		count++
		if _, err := os.Stat(rec.WorktreePath); os.IsNotExist(err) {
			r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("%s: worktree missing — run 'plax down %s' to clean up the record", name, name)})
			hasFail = true
		}
		if !worktree.BranchExists(deps.RepoRoot, name) {
			r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("%s: branch missing — run 'plax down %s' to clean up the record", name, name)})
			hasFail = true
		}
		if deps.BM != nil {
			exists, err := deps.BM.InstanceDBExists(ctx, rec.DBName)
			switch {
			case err != nil:
				r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: fmt.Sprintf("%s: cannot check database: %v", name, err)})
			case !exists:
				r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("%s: database %s missing — 'plax down' + 'plax up' to rebuild", name, rec.DBName)})
				hasFail = true
			}
		}
		if deps.Docker != nil {
			for svcName, cid := range rec.ContainerIDs {
				exists, err := deps.Docker.ServiceExists(ctx, cid)
				switch {
				case err != nil:
					r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: fmt.Sprintf("%s: cannot check container %s: %v", name, svcName, err)})
				case !exists:
					r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("%s: container %s missing — 'plax down' + 'plax up' to rebuild", name, svcName)})
					hasFail = true
				}
			}
		}
		if rec.State == "running" {
			for svcName, cid := range rec.ContainerIDs {
				if deps.Docker != nil {
					running, err := deps.Docker.ServiceRunning(ctx, cid)
					if err == nil && !running {
						r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: fmt.Sprintf("%s: %s container is not running but state is \"running\" — crashed? 'plax suspend' then 'plax resume' to restart", name, svcName)})
					}
				}
			}
			for procName, pgid := range rec.PIDs {
				dead := !process.IsAlive(pgid)
				if !dead && rec.PIDStarts[procName] != 0 {
					dead = process.StartTime(pgid) != rec.PIDStarts[procName]
				}
				if dead {
					r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: fmt.Sprintf("%s: %s process is not running but state is \"running\" — crashed? 'plax suspend' then 'plax resume' to restart", name, procName)})
				}
			}
		}
	}

	if count > 0 && !hasFail {
		r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: fmt.Sprintf("%d instances, all resources present", count)})
	}
}

func runRepoVsMachine(r *Report, deps *Deps) {
	area := "repo-vs-machine"

	if deps.Blueprint.Toolchain != "" {
		tcPath := filepath.Join(deps.RepoRoot, deps.Blueprint.Toolchain)
		pins, err := toolchain.ParsePins(tcPath)
		if err != nil {
			r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: fmt.Sprintf("toolchain file %s: %v", deps.Blueprint.Toolchain, err)})
		} else if pins == nil {
			r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: "no toolchain file — skipped"})
		} else {
			resolved := toolchain.ResolveVersions(pins)
			for tool, pin := range pins {
				ver, ok := resolved[tool]
				if !ok {
					r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("%s: pin %s but not installed", tool, pin)})
				} else if !toolchain.MatchesPin(pin, ver) {
					r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("%s: pinned %s, installed %s", tool, pin, ver)})
				} else {
					r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: fmt.Sprintf("%s %s (pinned %s)", tool, ver, pin)})
				}
			}
		}
	}

	if deps.Docker == nil {
		r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: "docker: cannot connect to daemon"})
	} else {
		r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: "docker: reachable"})
	}

	if deps.BM == nil {
		r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: "postgres: cannot connect"})
	} else {
		r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: "postgres: reachable"})
	}
}

func runBase(ctx context.Context, r *Report, deps *Deps) {
	area := "base"
	if deps.BM == nil {
		return
	}

	info, err := deps.BM.BaseStatus(ctx)
	if err != nil {
		r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: fmt.Sprintf("base status: %v", err)})
		return
	}

	if !info.Exists {
		r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: "plax_base does not exist — run 'plax base reset'"})
		return
	}

	if !info.Locked {
		r.Checks = append(r.Checks, Check{Area: area, Level: Fail, Message: "plax_base is not locked — run 'plax base reset' to repair"})
	} else {
		r.Checks = append(r.Checks, Check{Area: area, Level: Pass, Message: fmt.Sprintf("plax_base v%d, locked", info.ProvenanceVer)})
	}

	if info.ProvenanceVer == 0 {
		r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: "plax_base has no provenance row"})
	}

	if info.HasBaseNext {
		r.Checks = append(r.Checks, Check{Area: area, Level: Warn, Message: "staged plax_base_next exists — a refresh swap was deferred; run 'plax base refresh' to finish it or 'plax base reset' to discard"})
	}
}
