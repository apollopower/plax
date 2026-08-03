# Phase 1 — Blueprint, Registry, Init

## Objective

Define the data types, validation rules, and filesystem structures every other
phase depends on, plus `plax init` to scaffold a blueprint from a repo. No
instances run yet.

---

## Package layout

```
cmd/plax/main.go              # CLI entry: parses "plax init" via kong, calls pkg
pkg/
  blueprint/
    blueprint.go              # Blueprint, ServiceDef, ProcessDef, EnvConfig, isolation enums
    blueprint_test.go         # Unmarshal/marshal round-trip, validation table tests
    validate.go               # ValidateBlueprint(blueprint) []error
    validate_test.go          # One test per validation rule
    init.go                   # InitFromRepo(root string) (*Blueprint, error)
    init_test.go              # Runs against eai repo fixtures, golden-file output
  registry/
    registry.go               # Registry, InstanceRecord, PortAllocation, atomic read/write
    registry_test.go          # CRUD round-trip, concurrent read/write (sequential safe for now)
  portpool/
    pool.go                   # PortPool with Allocate/Release/Reserve
    pool_test.go              # Allocate fills range, Release returns port, OS probe
```

Every file is under `github.com/apollopower/plax/pkg/...`.

---

## Type specifications

### `pkg/blueprint/blueprint.go`

```go
package blueprint

// Blueprint is the top-level config, stored as plax.json in the repo root.
type Blueprint struct {
	Version   int                      `json:"version"`
	Name      string                   `json:"name"`
	PortPool  PortPool                 `json:"port_pool"`
	Toolchain string                   `json:"toolchain"`
	Seed      SeedConfig               `json:"seed"`
	Services  map[string]ServiceDef    `json:"services"`
	Processes []ProcessDef             `json:"processes"`
	Env       EnvConfig                `json:"env"`
}

type PortPool struct {
	Start int `json:"start"` // inclusive
	End   int `json:"end"`   // inclusive
}

type SeedConfig struct {
	Command string `json:"command"` // e.g. "bun run db fixtures"
	Workdir string `json:"workdir"` // relative to repo root, e.g. "."
}

type ServiceDef struct {
	Isolation ServiceIsolation   `json:"isolation"`
	Type      string             `json:"type,omitempty"`      // "postgres" for logical; empty otherwise
	Image     string             `json:"image"`
	Env       map[string]string  `json:"env,omitempty"`
	Ports     map[string]PortDef `json:"ports,omitempty"`     // key = container port as string, e.g. "6379"
	Command   []string           `json:"command,omitempty"`
}

type PortDef struct {
	Var string `json:"var"` // env var name that gets the allocated host port, e.g. "REDIS_PORT"
}

type ServiceIsolation string

const (
	IsolationLogical   ServiceIsolation = "logical"
	IsolationDedicated ServiceIsolation = "dedicated"
	IsolationShared    ServiceIsolation = "shared"
	IsolationExternal  ServiceIsolation = "external"
)

type ProcessDef struct {
	Name        string   `json:"name"`
	Isolation   string   `json:"isolation"` // always "native" for processes
	Command     string   `json:"command"`
	Workdir     string   `json:"workdir"`
	PortVar     string   `json:"port_var,omitempty"`     // env var set to allocated port
	DefaultPort int      `json:"default_port,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

type EnvConfig struct {
	Template string            `json:"template"` // relative path to template file, e.g. ".env.example"
	Holes    map[string]string `json:"holes"`    // KEY → template string with {{VAR}} placeholders
}
```

**Validation rules** (in `validate.go`, `ValidateBlueprint(bp *Blueprint) []error`):

| # | Rule | Error message |
|---|---|---|
| V1 | `version` must be 1 | `blueprint: unsupported version N` |
| V2 | `name` must be non-empty | `blueprint: name is required` |
| V3 | `port_pool.start` < `port_pool.end` and both > 1024 | `blueprint: port_pool range invalid` |
| V4 | Service names must be unique | `blueprint: duplicate service "X"` |
| V5 | `isolation: "logical"` services must have `type` field set (only `"postgres"` valid day one) | `blueprint: service "X" is logical but missing type` |
| V6 | `isolation: "logical"` services must NOT have `ports` | `blueprint: service "X" is logical but declares ports` |
| V7 | All `ports[].var` values must be unique across all services | `blueprint: port var "X" used by multiple services` |
| V8 | Process names must be unique | `blueprint: duplicate process "X"` |
| V9 | `port_var` on processes must be unique and not collide with any service `ports[].var` | `blueprint: port var "X" collides with service port var` |
| V10 | `depends_on` entries must reference existing process names | `blueprint: process "X" depends_on "Y" which does not exist` |
| V11 | `seed.command` must be non-empty | `blueprint: seed.command is required` |
| V12 | `seed.workdir` must be non-empty | `blueprint: seed.workdir is required` |
| V13 | `env.template` must be non-empty | `blueprint: env.template is required` |
| V14 | Every hole key must appear in `env.template` or be appended (no-op, warning only) | `blueprint: hole "X" not found in template file` |

### `pkg/registry/registry.go`

```go
package registry

import "time"

type Registry struct {
	Version         int                        `json:"version"`
	BlueprintStamp  BlueprintStamp             `json:"blueprint_stamp"`
	Instances       map[string]InstanceRecord  `json:"instances"`
	PortAllocations map[int]PortAllocation     `json:"port_allocations"`
}

type BlueprintStamp struct {
	ComposeHash    string `json:"compose_hash"`     // sha256 hex
	EnvExampleHash string `json:"env_example_hash"` // sha256 hex
	ToolchainHash  string `json:"toolchain_hash"`   // sha256 hex
}

type InstanceRecord struct {
	ID           string            `json:"id"`
	Branch       string            `json:"branch"`
	WorktreePath string            `json:"worktree_path"`
	CreatedAt    time.Time         `json:"created_at"`
	State        string            `json:"state"` // "running" | "suspended"
	Ports        map[string]int    `json:"ports"` // var name → port number
	DBName       string            `json:"db_name"`
	ContainerIDs map[string]string `json:"container_ids,omitempty"` // service → container id
	PIDs         map[string]int    `json:"pids,omitempty"`          // process name → pid
	Provenance   Provenance        `json:"provenance"`
}

type PortAllocation struct {
	Instance string `json:"instance"`
	Service  string `json:"service"` // service name or process name
}

type Provenance struct {
	BaseVersion int    `json:"base_version"`
	Toolchain   string `json:"toolchain"`
}
```

**Public API:**

| Function | Signature | Behavior |
|---|---|---|
| `Open` | `Open(path string) (*Registry, error)` | Reads JSON from `path`. If file does not exist, returns empty registry (`Instances: map[string]InstanceRecord{}`, `PortAllocations: map[int]PortAllocation{}`), nil error. If file exists but is invalid JSON, returns error. |
| `Save` | `(r *Registry) Save() error` | Writes JSON to `path` atomically: marshal to `[]byte`, write to `path.tmp`, `os.Rename(path.tmp, path)`. The `path` is stored in the Registry struct on `Open` (unexported field). |
| `AddInstance` | `(r *Registry) AddInstance(id string, rec InstanceRecord) error` | Sets `r.Instances[id] = rec`. Returns error if `id` already exists. |
| `RemoveInstance` | `(r *Registry) RemoveInstance(id string) error` | Deletes `r.Instances[id]`. Removes all port allocations for this instance from `r.PortAllocations`. Returns error if `id` does not exist. |
| `GetInstance` | `(r *Registry) GetInstance(id string) (*InstanceRecord, bool)` | Returns `rec, true` if found, `nil, false` otherwise. |
| `AllocPort` | `(r *Registry) AllocPort(port int, inst string, svc string) error` | Sets `r.PortAllocations[port] = PortAllocation{inst, svc}`. Returns error if port already allocated. |
| `ReleasePort` | `(r *Registry) ReleasePort(port int)` | Deletes `r.PortAllocations[port]`. No-op if not allocated. |

### `pkg/portpool/pool.go`

```go
package portpool

type PortPool struct {
	start    int
	end      int
	registry *registry.Registry // reference to the registry
}

func New(start, end int, reg *registry.Registry) *PortPool {
	return &PortPool{start: start, end: end, registry: reg}
}

// Allocate returns the first port in [start, end] that is both:
//   (a) not present in reg.PortAllocations (registry check)
//   (b) not currently bound on the OS (net.Listen probe on 127.0.0.1)
// Registers the allocation in reg.PortAllocations.
// Returns error if no port is free.
func (p *PortPool) Allocate(instance, service string) (int, error)

// Release removes the port from reg.PortAllocations.
func (p *PortPool) Release(port int)

// Reserve attempts to claim a specific port for an instance.
// Fails if the port is already allocated OR bound on the OS.
// Used by plax resume (Phase 4).
func (p *PortPool) Reserve(port int, instance, service string) error
```

---

## Algorithms

### `plax init` — `InitFromRepo(root string) (*Blueprint, error)`

This is the only algorithm in Phase 1. It runs offline with no network or
container dependencies.

**Inputs** (all relative to `root`):
- `docker-compose.yml` — Docker Compose v3 file
- `.env.example` — key=value env template with `#` comments

**Steps:**

1. **Parse compose YAML.**
   Read `docker-compose.yml`. Unmarshal into a generic structure:
   ```
   map[string]interface{} with key "services" → map[string]interface{}
   ```
   ⚠ If `docker-compose.yml` does not exist or is invalid YAML, return error.

   For each service, extract:
   - `image` (string, required) — ⚠ skip services without an `image` field (emit warning to stderr, do not include in output)
   - `ports` ([]interface{}) — each element may be:
     - string like `"${POSTGRES_PORT:-5432}:5432"` → extract container port (after colon), extract env var name (`POSTGRES_PORT`)
     - string like `"5432:5432"` → extract container port (after colon), no env var
     - number like `5432` → treat as container port
   - `volumes` ([]string) — presence indicates stateful service
   - `environment` (map[string]string or []string) — ⚠ handle both forms. If []string, split on `=`
   - `command` (string or []string) — normalize to []string

2. **Classify services.**
   For each service, apply heuristics to determine `isolation`:

   | Condition | Isolation | Notes |
   |---|---|---|
   | Image name contains "postgres" or "pgvector" | `"logical"` | Also set `type: "postgres"` |
   | Has at least one `volumes:` entry AND is not postgres | `"dedicated"` | |
   | No `volumes:` entry | `"shared"` | Emit warning to stderr: "service X: gotenberg has no volumes, defaulting to shared — verify isolation" |

   ⚠ The heuristics are best-effort. Every service emits a comment in stderr
   stating its assigned isolation. The user must review.

3. **Build `ports` map per service.**
   For each service that is NOT `"logical"`:
   - For each compose port entry that has an env var (e.g. `${REDIS_PORT}`), emit:
     `"6379": { "var": "REDIS_PORT" }` where the key is the container port as a string
   - For each compose port entry with a bare number, emit a suggested env var:
     `"3000": { "var": "SVCNAME_PORT" }` — derived from service name + `_PORT`

   ⚠ Logical services get no `ports` field in the output.

4. **Build process list.**
   Emit two entries (hardcoded for the eai target; other repos will need
   this filled in manually):

   ```json
   {
     "name": "app",
     "isolation": "native",
     "command": "bun run dev:app",
     "workdir": ".",
     "port_var": "PORT",
     "default_port": 3000
   },
   {
     "name": "workers",
     "isolation": "native",
     "command": "bun run dev:workers",
     "workdir": ".",
     "depends_on": ["app"]
   }
   ```

   ⚠ `init` emits these as placeholders. The `command` field comes from
   `package.json` scripts if available, or is left as a TODO marker.

5. **Parse `.env.example`.**
   Read `.env.example` line by line. For each line:
   - Skip blank lines
   - Skip lines starting with `#` (comments)
   - Match `KEY=value` or `KEY="value"` — ⚠ strip leading/trailing whitespace, strip surrounding quotes from values
   - ⚠ `KEY=value # comment` — the comment after `#` is NOT part of the value. Split on first `#` not inside quotes

   Produce a `map[string]string` of `KEY → value` (the template value).

6. **Detect holes.**
   For each variable in the env template:
   - If its value matches `localhost:\d+` (contains `localhost:` followed by a port number) → candidate hole
   - ⚠ EXCEPT: if the port number is 5432 AND a logical postgres service was detected in step 2:
     do NOT flag it as a hole — postgres port is shared across instances, so `DATABASE_URL=postgres://postgres:postgres@localhost:5432/eai_dev` stays static (the DB name substitution happens via `{{DB_NAME}}` in the blueprint hole template, which the user writes manually)
   - For all other `localhost:PORT` candidates, emit a hole template using `{{PORT_VAR}}` syntax derived from the variable or service name

   Emit the `holes` map: each key is the env var name, each value is a
   template string with `{{VAR}}` placeholders.

   ⚠ `init` cannot determine the correct `{{VAR}}` name for every hole
   automatically. It guesses based on the service port vars. Variables that
   don't match any service's port var are emitted with a TODO marker:
   `"KEY": "TODO {{FIXME}}"` — the user must replace the template.

7. **Emit `seed` block.**
   ```
   "seed": {
     "command": "TODO: add seed command, e.g. 'bun run db fixtures'",
     "workdir": "."
   }
   ```
   ⚠ Always a placeholder. The user fills in the actual command.

8. **Assemble and return the Blueprint.**
   Merge all sections. Return `*Blueprint`. Caller (the CLI) marshals to JSON
   and writes to stdout.

### `ValidateBlueprint(bp *Blueprint) []error`

Runs all V1–V14 rules from the validation table above. Returns a slice of
errors (not a single multi-error — the caller formats them). If `env.template`
is present on disk, also runs V14 (checks each hole key appears in the
template file).

---

## CLI specification

### `plax init`

```
plax init [--root <path>]
```

| Aspect | Value |
|---|---|
| Flags | `--root <path>` — repo root, defaults to `.` (cwd) |
| Exit 0 | Blueprint JSON written to stdout |
| Exit 1 | Parse error (missing compose, invalid YAML, etc.) |
| Stderr | Warnings, progress messages |
| Stdout | Valid JSON blueprint |
| Idempotent | Yes — reads files, writes nothing to disk |

Example usage:
```
cd ~/Work/repos/eai
plax init > plax.json
plax init --root ~/Work/repos/eai > plax.json
```

`plax init` does NOT create `plax.json`. It prints to stdout. The user
redirects or copies.

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| `docker-compose.yml` missing | `os.Stat` returns `os.ErrNotExist` | Print to stderr: `init: docker-compose.yml not found at <path>`. Exit 1. |
| `docker-compose.yml` is invalid YAML | `yaml.Unmarshal` returns error | Print to stderr: `init: invalid compose YAML: <err>`. Exit 1. |
| `.env.example` missing | `os.Stat` returns `os.ErrNotExist` | Print to stderr: `init: .env.example not found at <path> — env.holes will be empty`. Continue (empty map is valid). |
| Service without `image` field | `image` key absent from service map | Print warning to stderr: `init: service "X" has no image, skipping`. Omit from output. |
| Port entry is unparseable | String doesn't match `HOST:CONTAINER` pattern | Print warning to stderr: `init: service "X" port "Y" unparseable, skipping`. Omit that port. |
| Invalid blueprint (validation fails) | `ValidateBlueprint` returns errors | Print each error to stderr. Return nil blueprint + error. This is a **programming error** — init should always produce valid output. |

---

## Tests

### Test fixtures

Create `pkg/blueprint/testdata/eai/` directory containing copies of:
- `docker-compose.yml` — the eai compose file
- `.env.example` — the eai env example file
- `plax.json.golden` — the expected golden output of `plax init` on these inputs

The golden file is committed. Tests compare `InitFromRepo(testdata/eai/)`
output against the golden file using `go-cmp` or `bytes.Equal` after
normalizing JSON (canonical ordering via `json.Marshal` + `json.Indent`).
⚠ Use a `cmp.Transformer` to ignore field ordering in maps and slices.

### Unit tests

**`pkg/blueprint/blueprint_test.go`:**
- `TestBlueprintMarshalRoundTrip` — marshal → unmarshal → marshal, assert JSON identical (canonical ordering)
- `TestBlueprintUnmarshal_UnknownFields` — JSON with extra keys does not error (`json.Decoder.DisallowUnknownFields()` is NOT used — forward-compat)

**`pkg/blueprint/validate_test.go`:**
- `TestValidate_ValidBlueprint` — eai golden blueprint passes all rules
- `TestValidate_VersionNotOne` — version 2 returns "unsupported version 2"
- `TestValidate_MissingName` — empty name returns "name is required"
- `TestValidate_PortPoolInvalid` — start=5000 end=4000 → "range invalid"; start=0 → "range invalid" (> 1024 check)
- `TestValidate_DuplicateService` — two services named "db" → "duplicate service"
- `TestValidate_LogicalMissingType` — isolation="logical" with no type → "missing type"
- `TestValidate_LogicalHasPorts` — isolation="logical" with ports block → "declares ports"
- `TestValidate_PortVarConflict` — two services with same `ports[].var` → "port var collision"
- `TestValidate_DuplicateProcess` — two processes named "app" → "duplicate process"
- `TestValidate_PortVarProcessCollision` — service port var matches process port_var → "port var collision"
- `TestValidate_DependsOnMissing` — depends_on references nonexistent process → "does not exist"
- `TestValidate_SeedCommandMissing` — seed.command empty → "seed.command is required"
- `TestValidate_HoleNotInTemplate` — hole key absent from template file → warning (not error)

**`pkg/blueprint/init_test.go`:**
- `TestInit_EaiRepo_GoldenMatch` — `InitFromRepo("testdata/eai/")` output matches `testdata/eai/plax.json.golden`
- `TestInit_MissingCompose` — returns error, error message contains "not found"
- `TestInit_MissingEnvExample` — succeeds with empty holes map
- `TestInit_ComposeWithNoImage` — service without image is skipped with warning
- `TestInit_ComposePortsBareNumber` — bare port generates a suggested var name
- `TestInit_ComposePortsEnvVarDefault` — `${POSTGRES_PORT:-5432}:5432` extracts correct var name
- `TestInit_PostgresService_NotInPortMap` — postgres service has no `ports` field in output
- `TestInit_PostgresPort_NotHole` — .env.example value `localhost:5432` is NOT flagged as a hole when logical postgres exists

**`pkg/registry/registry_test.go`:**
- `TestOpen_NewFile` — `Open` on nonexistent path returns empty registry, nil error
- `TestOpen_ExistingFile` — `Open` on existing valid JSON returns populated registry
- `TestOpen_InvalidJSON` — `Open` on malformed JSON returns error
- `TestSave_ReadBack` — `AddInstance` → `Save` → `Open` returns same data
- `TestSave_AtomicWrite` — verify temp file used (check no `.tmp` left behind), verify content never partial
- `TestAddInstance_Duplicate` — adding same id twice returns error
- `TestRemoveInstance_NotFound` — removing nonexistent id returns error
- `TestRemoveInstance_CleansPorts` — remove instance also removes all its port allocations
- `TestAllocPort_Duplicate` — allocating same port twice returns error
- `TestReleasePort_NotAllocated` — releasing unallocated port is no-op (no error)

**`pkg/portpool/pool_test.go`:**
- `TestAllocate_FirstFree` — first call returns `start` port
- `TestAllocate_SkipsAllocated` — after allocating port X, next call returns X+1
- `TestAllocate_Exhausted` — all ports in pool used → returns error
- `TestAllocate_OSProbe` — if port is bound (start a listener in test), Allocate skips it
- `TestRelease_ReturnsPort` — after release, same port becomes allocatable again
- `TestReserve_Success` — Reserve specific port returns no error
- `TestReserve_Taken` — Reserve port already allocated → returns error
- `TestReserve_OSBound` — Reserve port bound by OS → returns error

### Integration test

`TestEaiInitWithoutErrors` — runs `plax init --root <actual eai repo path>`.
This test is **skipped in CI** (requires the eai repo to be present) but
runs locally. Verifies exit code 0 and valid JSON on stdout.

---

## Acceptance criteria

- [ ] `plax init --root ~/Work/repos/eai` prints valid JSON to stdout, exits 0
- [ ] The output JSON passes `ValidateBlueprint()` with zero errors
- [ ] The output JSON matches the golden file byte-for-byte (canonical JSON)
- [ ] `db` service has no `ports` field
- [ ] `seed.command` is `"bun run db fixtures"` (from golden file, not from init — init emits a TODO)
- [ ] `redis` service has `ports: { "6379": { "var": "REDIS_PORT" } }`
- [ ] `gotenberg` service has `ports: { "3000": { "var": "GOTENBERG_PORT" } }`
- [ ] `app` process has `port_var: "PORT"` and `default_port: 3000`
- [ ] Registry Open/Save round-trips all fields correctly
- [ ] Registry atomic write leaves no `.tmp` file on success
- [ ] Port allocator skips ports that `net.Listen` cannot bind
- [ ] Port allocator returns error when pool is exhausted
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes with all tests above
- [ ] `golangci-lint run` passes (or any lint issues are pre-approved `//nolint`)

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| kong | `github.com/alecthomas/kong/v2` | latest | CLI framework |
| go-yaml | `github.com/goccy/go-yaml` | latest | Parse docker-compose.yml |
| go-cmp | `github.com/google/go-cmp/cmp` | latest | Deep equality in tests (golden file comparison) |

All standard library:
- `encoding/json` — blueprint marshal/unmarshal, registry read/write
- `crypto/sha256` — config stamp hashing (registry blueprint_stamp)
- `net` — `net.Listen` probe in port allocator
- `os` — file I/O, `os.Rename` for atomic writes
- `testing` — test framework
