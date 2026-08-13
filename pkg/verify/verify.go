// Package verify runs static and runtime checks against an instance,
// catching silent correctness bugs before the instance is trusted.
package verify

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
)

type CheckResult struct {
	Check    string `json:"check"`
	Layer    int    `json:"layer"`
	Passed   bool   `json:"passed"`
	Detail   string `json:"detail"`
	Artifact string `json:"artifact,omitempty"`
}

type VerificationError struct {
	Results []CheckResult
	Layer   int
}

func (e *VerificationError) Error() string {
	var parts []string
	for _, r := range e.Results {
		if !r.Passed {
			detail := r.Check
			if r.Artifact != "" {
				detail += " (" + r.Artifact + ")"
			}
			parts = append(parts, detail)
		}
	}
	return "verification failed: " + strings.Join(parts, ", ")
}

type BMInterface interface {
	InstanceDBExists(ctx context.Context, dbName string) (bool, error)
	InstanceProvenance(ctx context.Context, dbName string) (*postgres.ProvenanceRow, error)
}

type Deps struct {
	Blueprint *blueprint.Blueprint
	Registry  *registry.Registry
	BM        BMInterface
	RepoRoot  string
}

func RunVerify(ctx context.Context, deps *Deps, name string) ([]CheckResult, error) {
	rec, found := deps.Registry.GetInstance(name)
	if !found {
		return nil, fmt.Errorf("instance %q not found", name)
	}

	results := make([]CheckResult, 0)

	templatePath := filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)
	userEnvPath := filepath.Join(deps.RepoRoot, ".env")
	derivedPath := filepath.Join(rec.WorktreePath, ".env")
	scrub := BuildScrubSet(deps.Blueprint)
	results = append(results, CheckEnv(templatePath, userEnvPath, derivedPath, deps.Blueprint.Env.Holes, scrub)...)

	if rec.State == registry.StateRunning {
		results = append(results, CheckServices(ctx, deps.Blueprint.Services, rec.Ports)...)
		results = append(results, CheckProcesses(rec.PIDs, process.IsAlive)...)
	}

	if deps.BM != nil {
		results = append(results, CheckDatabases(ctx, registry.DBNamesFromRecord(rec), deps.BM)...)
	}

	allPassed := true
	for _, r := range results {
		if !r.Passed {
			allPassed = false
			break
		}
	}

	now := time.Now()
	rec.VerifiedAt = &now
	if allPassed {
		rec.Health = registry.HealthHealthy
	} else {
		rec.Health = registry.HealthUnhealthy
	}
	if err := deps.Registry.UpdateInstance(name, rec); err != nil {
		return results, fmt.Errorf("updating instance health: %w", err)
	}
	if err := deps.Registry.Save(); err != nil {
		return results, fmt.Errorf("saving verification results: %w", err)
	}

	if !allPassed {
		return results, &VerificationError{Results: results, Layer: 1}
	}
	return results, nil
}

func CheckEnv(templatePath, userEnvPath, derivedEnvPath string, holes map[string]string, scrub map[string]bool) []CheckResult {
	var results []CheckResult
	results = append(results, checkEnvCompleteness(templatePath, userEnvPath, derivedEnvPath, holes, scrub)...)
	results = append(results, checkEnvNoUnresolved(derivedEnvPath)...)
	results = append(results, checkEnvNoScrubbedLeaks(userEnvPath, derivedEnvPath, scrub, templatePath)...)
	return results
}

func checkEnvCompleteness(templatePath, userEnvPath, derivedEnvPath string, holes map[string]string, scrub map[string]bool) []CheckResult {
	if _, err := os.Stat(derivedEnvPath); os.IsNotExist(err) {
		return []CheckResult{{
			Check: "env-completeness", Layer: 1, Passed: false,
			Detail: "derived .env not found at " + derivedEnvPath, Artifact: derivedEnvPath,
		}}
	}

	expected := map[string]bool{}

	if _, err := os.Stat(templatePath); err == nil {
		tmplKeys, err := env.ParseFile(templatePath)
		if err == nil {
			for k := range tmplKeys {
				expected[k] = true
			}
		}
	}

	if _, err := os.Stat(userEnvPath); err == nil {
		userKeys, err := env.ParseFile(userEnvPath)
		if err == nil {
			for k := range userKeys {
				expected[k] = true
			}
		}
	}

	for k := range holes {
		expected[k] = true
	}

	for k := range scrub {
		delete(expected, k)
	}

	derived, err := env.ParseFile(derivedEnvPath)
	if err != nil {
		return []CheckResult{{
			Check: "env-completeness", Layer: 1, Passed: false,
			Detail: "parsing derived .env: " + err.Error(), Artifact: derivedEnvPath,
		}}
	}

	var missing []string
	for k := range expected {
		if _, ok := derived[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		var results []CheckResult
		for _, k := range missing {
			results = append(results, CheckResult{
				Check: "env-completeness", Layer: 1, Passed: false,
				Detail: "key " + k + " is missing from derived .env", Artifact: k,
			})
		}
		return results
	}
	keyNoun := "keys"
	if len(expected) == 1 {
		keyNoun = "key"
	}
	return []CheckResult{{
		Check: "env-completeness", Layer: 1, Passed: true,
		Detail: fmt.Sprintf("all %d expected %s present", len(expected), keyNoun),
	}}
}

func checkEnvNoUnresolved(derivedEnvPath string) []CheckResult {
	data, err := os.ReadFile(derivedEnvPath)
	if err != nil {
		return nil
	}

	var results []CheckResult
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "{{") {
			results = append(results, CheckResult{
				Check: "env-unresolved-holes", Layer: 1, Passed: false,
				Detail:   "unresolved template hole " + extractHole(line) + " survives in derived .env",
				Artifact: "derived .env",
			})
		}
	}
	if len(results) > 0 {
		return results
	}
	return []CheckResult{{
		Check: "env-unresolved-holes", Layer: 1, Passed: true,
		Detail: "no unresolved holes in derived .env",
	}}
}

func extractHole(line string) string {
	start := strings.Index(line, "{{")
	if start < 0 {
		return ""
	}
	end := strings.Index(line[start:], "}}")
	if end < 0 {
		return line[start:]
	}
	return line[start : start+end+2]
}

func checkEnvNoScrubbedLeaks(userEnvPath, derivedEnvPath string, scrub map[string]bool, templatePath string) []CheckResult {
	tmplValues := map[string]string{}
	if _, err := os.Stat(templatePath); err == nil {
		tmplValues, _ = env.ParseFile(templatePath)
	}

	userValues, err := env.ParseFile(userEnvPath)
	if err != nil {
		return nil
	}

	derivedValues, err := env.ParseFile(derivedEnvPath)
	if err != nil {
		return nil
	}

	var results []CheckResult
	for k := range scrub {
		userVal, ok := userValues[k]
		if !ok || userVal == "" {
			continue
		}
		if userVal == tmplValues[k] {
			continue
		}
		for derivedKey, derivedVal := range derivedValues {
			if derivedVal == userVal {
				results = append(results, CheckResult{
					Check: "env-scrubbed-leaks", Layer: 1, Passed: false,
					Detail:   "scrubbed key " + k + "'s real value appears in derived .env under key " + derivedKey,
					Artifact: derivedKey,
				})
			}
		}
	}
	if len(results) > 0 {
		return results
	}
	return []CheckResult{{
		Check: "env-scrubbed-leaks", Layer: 1, Passed: true,
		Detail: "no scrubbed values leaked into derived .env",
	}}
}

func CheckServices(ctx context.Context, services map[string]blueprint.ServiceDef, allocated map[string]int) []CheckResult {
	var results []CheckResult
	checked := 0
	for svcName, svc := range services {
		if svc.Isolation != blueprint.IsolationDedicated {
			continue
		}
		for _, portDef := range svc.Ports {
			port, ok := allocated[portDef.Var]
			if !ok {
				continue
			}
			checked++
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err == nil {
				_ = conn.Close()
				continue
			}
			if isRefused(err) {
				time.Sleep(1 * time.Second)
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err == nil {
					_ = conn.Close()
					continue
				}
			}
			detail := fmt.Sprintf("service %s on port %d is not reachable", svcName, port)
			if isTimeout(err) {
				detail = fmt.Sprintf("service %s on port %d timed out after 2s", svcName, port)
			}
			results = append(results, CheckResult{
				Check: "tcp-reachability", Layer: 1, Passed: false,
				Detail: detail, Artifact: addr,
			})
		}
	}
	if len(results) > 0 {
		return results
	}
	if checked == 0 {
		return nil
	}
	noun := "service endpoints"
	if checked == 1 {
		noun = "service endpoint"
	}
	return []CheckResult{{
		Check: "tcp-reachability", Layer: 1, Passed: true,
		Detail: fmt.Sprintf("all %d %s reachable", checked, noun),
	}}
}

func isRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

func isTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "i/o timeout")
}

func CheckProcesses(pids map[string]int, isAlive func(int) bool) []CheckResult {
	var results []CheckResult
	for procName, pgid := range pids {
		if !isAlive(pgid) {
			results = append(results, CheckResult{
				Check: "process-liveness", Layer: 1, Passed: false,
				Detail:   fmt.Sprintf("process %s (PGID=%d) is not alive", procName, pgid),
				Artifact: procName,
			})
		}
	}
	if len(results) > 0 {
		return results
	}
	if len(pids) == 0 {
		return nil
	}
	noun := "processes"
	if len(pids) == 1 {
		noun = "process"
	}
	return []CheckResult{{
		Check: "process-liveness", Layer: 1, Passed: true,
		Detail: fmt.Sprintf("all %d %s alive", len(pids), noun),
	}}
}

func CheckDatabases(ctx context.Context, dbNames []string, bm BMInterface) []CheckResult {
	var results []CheckResult
	allExist := true
	allProv := true
	for _, dbName := range dbNames {
		exists, err := bm.InstanceDBExists(ctx, dbName)
		if err != nil {
			results = append(results, CheckResult{
				Check: "db-existence", Layer: 1, Passed: false,
				Detail:   fmt.Sprintf("checking database %s: %v", dbName, err),
				Artifact: dbName,
			})
			allExist = false
			continue
		}
		if !exists {
			results = append(results, CheckResult{
				Check: "db-existence", Layer: 1, Passed: false,
				Detail:   fmt.Sprintf("database %s does not exist", dbName),
				Artifact: dbName,
			})
			allExist = false
			continue
		}

		prov, err := bm.InstanceProvenance(ctx, dbName)
		if err != nil {
			results = append(results, CheckResult{
				Check: "db-provenance", Layer: 1, Passed: false,
				Detail:   fmt.Sprintf("checking provenance for %s: %v", dbName, err),
				Artifact: dbName,
			})
			allProv = false
			continue
		}
		if prov == nil {
			results = append(results, CheckResult{
				Check: "db-provenance", Layer: 1, Passed: false,
				Detail:   fmt.Sprintf("database %s has no provenance table — it may have been dropped and recreated externally", dbName),
				Artifact: dbName,
			})
			allProv = false
		}
	}
	if len(results) > 0 {
		return results
	}
	if len(dbNames) == 0 {
		return nil
	}
	var pass []CheckResult
	dbNoun := "databases"
	if len(dbNames) == 1 {
		dbNoun = "database"
	}
	if allExist {
		pass = append(pass, CheckResult{
			Check: "db-existence", Layer: 1, Passed: true,
			Detail: fmt.Sprintf("all %d %s exist", len(dbNames), dbNoun),
		})
	}
	if allProv {
		pass = append(pass, CheckResult{
			Check: "db-provenance", Layer: 1, Passed: true,
			Detail: fmt.Sprintf("all %d %s have valid provenance", len(dbNames), dbNoun),
		})
	}
	return pass
}

func BuildScrubSet(bp *blueprint.Blueprint) map[string]bool {
	s := make(map[string]bool, len(bp.Env.Scrub))
	for _, k := range bp.Env.Scrub {
		s[k] = true
	}
	return s
}
