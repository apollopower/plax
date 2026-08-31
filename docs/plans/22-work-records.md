# Plan 22 — Work records and instance lineage

## Objective

Add a durable, append-only work record for each instance, with optional
instance-to-instance lineage that supports stacked worktrees and pull
requests (issue #76; triaged in `docs/plans/triage/2026-08-26.md`).

## Package layout

```
pkg/record/record.go          # record format, paths, creation, append, read
pkg/record/record_test.go     # format, atomic creation, append, and parsing tests
pkg/worktree/worktree.go      # create a child branch from a parent instance HEAD
pkg/worktree/worktree_test.go # parent-base and missing-parent tests
pkg/registry/registry.go      # instance lookup needed by parent resolution
cmd/plax/main.go              # up flags, log and record commands
cmd/plax/main_test.go         # CLI validation and stdout/stderr behavior
cmd/plax/e2e_test.go          # real-Git stacked ancestry scenario
cmd/plax/guide.md             # agent-facing command reference
docs/manual.md                # human-facing work-record and lineage documentation
docs/plans/index.md           # phase table entry for plan 22
docs/plans/triage/2026-08-26.md # link plan 22 to issue #76 triage
```

The record package owns file format and I/O. It does not own instance
lifecycle, Git, agent processes, or verification policy. The CLI remains a
thin adapter, consistent with the existing mailbox commands.

## Type specifications

### `pkg/record`

The on-disk record is text rather than JSON so it remains greppable,
diffable, and useful with ordinary UNIX tools. Headers precede `---`; the
body is free-form prose.

```go
// Record is the parsed representation of an instance work record.
type Record struct {
    Instance   string   `json:"instance"`
    Parent     string   `json:"parent,omitempty"`
    BaseCommit string   `json:"base_commit,omitempty"`
    Intent     string   `json:"intent"`
    Contract   []string `json:"contract,omitempty"`
    Body       string     `json:"body,omitempty"`
    Log        []LogEntry `json:"log,omitempty"`
    Verdict    *Verdict   `json:"verdict,omitempty"`
}

// LogEntry is one append-only historical note.
type LogEntry struct {
    At   time.Time `json:"at"`
    Text string    `json:"text"`
}

// Verdict is the executor's terminal declaration about the work record.
type Verdict struct {
    Status   string    `json:"status"`
    Contract string    `json:"contract,omitempty"`
    At       time.Time `json:"at"`
    Summary  string    `json:"summary,omitempty"`
}

// CreateInput supplies the operator-authored portion of a record.
type CreateInput struct {
    Instance   string   `json:"instance"`
    Parent     string   `json:"parent,omitempty"`
    BaseCommit string   `json:"base_commit,omitempty"`
    Intent     string   `json:"intent"`
    Contract   []string `json:"contract,omitempty"`
    Body       string   `json:"body,omitempty"`
}

// Path returns the repository-scoped record path.
func Path(repoRoot, name string) string

// Create writes a new record atomically and fails if the record already exists.
func Create(repoRoot string, input CreateInput) error

// Append appends a timestamped prose entry to an existing record.
func Append(repoRoot, name, text string, now time.Time) error

// Read parses an existing record.
func Read(repoRoot, name string) (Record, error)
```

The exact wire representation is:

```
instance: i1
parent: i0
base_commit: 0123456789abcdef0123456789abcdef01234567
intent: add retry coverage
contract: tests
contract: typecheck
---
## intent
add retry coverage
The child task was assigned from i0's billing work.

## log
at: 2026-08-29T12:00:00Z
Found the retry path; adding a regression test.

## verdict
status: pass
contract: pass
at: 2026-08-29T12:01:00Z
Tests and typecheck pass.
```

Rules:

- `instance` is required and must equal the requested instance name.
- `parent` and `base_commit` are optional for root instances and required
  together for a stacked child.
- `intent` is operator-authored text. It is not invalid merely because an AI
  operator supplied it; self-authored intent is still useful as a statement
  of the requested task.
- `contract` is a repeated header, one entry per ordered acceptance statement.
  A contract entry may contain commas without escaping. It is recorded and
  projected, but is not silently treated as executable verification checks;
  see the scope boundary below.
- Headers are single-line and use `key: value`. Prose belongs below the
  separator. The `intent` header is the first non-empty line of the supplied
  intent; the complete multiline intent is copied below `## intent` in the
  body. Newlines are never escaped into a header.
- `base_commit` is always the complete 40-hex-character Git commit ID, not a
  display abbreviation.
- `Append` writes only after the record exists and never rewrites prior bytes.
- Record paths are `.plax/records/<name>.md`; instance names are validated by
  the existing instance-name rules before path construction.

Body sections have a fixed grammar for parsing and JSON projection:

- Exactly one `## intent` section contains the complete intent prose.
- Zero or more `## log` sections contain `at: <RFC3339>` followed by prose.
- Zero or one `## verdict` section contains `status: pass|fail`, an optional
  `contract: pass|fail`, an `at: <RFC3339>` line, and prose summary.
- Unknown `##` sections and duplicate verdict sections are parse errors.
- A verdict is author-once, not file-terminal. `plax log` after a verdict
  remains allowed for historical notes, but `plax verdict` cannot append a
  second verdict. Log sections may appear before or after the verdict.

### `pkg/registry`

No new persisted registry field is required for the first implementation.
The existing instance record lookup resolves `parent`; the work record stores
the durable human-readable projection.

### `pkg/worktree`

Extend branch creation without changing the existing default:

```go
// CreateFromCommit creates a plax branch and worktree from an exact commit.
func CreateFromCommit(repoRoot, name, commit string) (string, error)
```

`Create` continues to branch from the repository `HEAD` or an explicit
`--ref`. A parent-based `up` call uses `CreateFromCommit` with the parent's
current worktree `HEAD` and records that commit as `base_commit`.

## Algorithms

### Root instance creation

```
plax up --intent <file> i0

1. Validate the instance name and blueprint before side effects.
2. Read <file>; reject a missing, unreadable, or empty intent file.
3. Create i0 using the existing current-HEAD / --ref behavior.
4. Create the normal instance resources.
5. Atomically create .plax/records/i0.md before reporting success.
6. If record creation fails, roll back the instance like any other required
   up phase.
```

`--intent` supplies the root record's intent. It does not mean intent is
human-only; any external operator, including Codex, can supply it.

### Child instance creation and stacked ancestry

```
plax up --parent i0 --intent <file> i1

1. Validate i1 and resolve i0 in the same repository registry.
   ⚠ i0 must be an existing registered instance with an accessible worktree.
2. Read i0's worktree HEAD; reject a dirty/unresolvable parent state rather
   than silently falling back to repository HEAD.
3. Create branch plax/i1 from that exact parent commit.
4. Create the normal i1 resources.
5. Atomically create i1's record with parent=i0 and base_commit=<commit>.
6. Roll back the child worktree/resources if any required step fails.
```

This makes a two-layer stack explicit:

```
main -> i0/PR1 -> i1/PR2 -> i2/PR3
```

The relationship is a snapshot, not live tracking. Later commits on `i0` do
not move `i1`; the operator must deliberately rebase or recreate the child.
Uncommitted changes in the parent are not a usable Git base and must be
rejected.

`--parent` therefore means both:

- record lineage: `i1` belongs beneath `i0`; and
- Git ancestry: `i1` starts from `i0`'s captured `HEAD`.

There is no implicit parent for an operator running from the main repository.
An external AI creates a root with `--intent`; `--parent` is valid only when
it names an existing plax instance. A separate task-parent relationship is
out of scope for this phase because it would overload the record with two
independent graphs.

### Record append

```
plax log i1 -- "Found the failing retry path"

1. Resolve i1 by validating its instance name and locating
   `.plax/records/i1.md`; registry membership is not required, so logging
   remains possible after `down`.
2. Acquire an OS-level `flock` on a sibling lock file for the append. The lock
   must coordinate separate `plax log` processes, not only goroutines in one
   process.
3. Open its record without truncation.
4. Append an RFC3339 timestamp and the supplied text.
5. Flush and close; return the record path on stdout only if requested by
   the command's output mode.
   ⚠ A partial append must return an error; never claim a successful log
   entry when the write did not complete.
```

### Record read and JSON projection

```
plax record i1

1. Acquire a shared OS-level `flock` on the sibling lock file before reading;
   readers do not observe a concurrent partial append.
2. Parse the record and preserve the original text for default output.
3. Print the complete record to stdout; diagnostics go to stderr.
4. With --json, emit the parsed headers, contract list, body, log entries,
   and verdict as one JSON object.
```

The default output is the file content, not a synthesized summary, so tools
can compose around the same representation.

### Verdict authoring

```
plax verdict i1 --status pass --contract pass -- "Tests and typecheck pass"

1. Locate `.plax/records/i1.md` directly; registry membership is not required,
   so this remains usable after `down`.
2. Acquire the same OS-level `flock` used by `log` before reading or checking
   for an existing verdict.
3. Parse the record and reject the operation if a verdict already exists.
4. Validate status and contract values (`pass` or `fail`).
5. Append one author-once `## verdict` section with the current RFC3339 time and
   optional summary prose.
6. Flush and close under the held lock.
   ⚠ This records the operator's declaration; it does not claim that Plax
   independently validated the task contract.
```

### Record lifecycle

Records survive `down`, `suspend`, and `resume`. `down` removes the worktree
and currently deletes the plax branch, but does not remove `.plax/records`.
The record's `base_commit` remains historical evidence of a child stack base;
it is not sufficient to recreate a parent branch after Git garbage collection.
`plax log`, `plax verdict`, and `plax record` resolve directly from the record
path rather than the registry, so they continue to work after `down`.

The sibling lock file is persistent metadata at `.plax/records/<name>.lock`;
it is created with the record directory and is never removed after an
operation. Writers take an exclusive flock and readers take a shared flock,
so `record` always parses a complete append. Lock-file cleanup is part of
explicit record deletion, which is not provided by this phase.

### Contract boundary

The current `verify` package runs fixed environment, service, process, and
database checks; it does not execute arbitrary per-record contract entries.
This phase stores and displays `contract` so an operator or merge tool can use
it, but does not invent a second verification language or claim that
`plax verify` has checked it. A later plan may make contracts executable once
the check vocabulary and failure semantics are defined.

## CLI specification

### `plax up`

```
plax up [--intent <file>] [--parent <instance>] [--ref <ref>] <name>
```

- `--intent <file>` is required for a tracked root record.
- `--parent <instance>` requires an existing instance and selects its
  current worktree `HEAD` as the child branch base.
- A child may also supply `--intent <file>` for its specific delegated task.
- `--parent` without `--intent` is rejected in this phase; the record needs
  explicit child intent rather than silently inheriting a broad parent task.
- `--parent` and `--ref` are mutually exclusive: the parent selects the Git
  base, while `--ref` selects an independent external base.
- With neither `--intent` nor `--parent`, retain current untracked behavior
  but warn on stderr that no work record will be created.
- Record creation is part of successful `up`; failure returns non-zero and
  rolls back all instance side effects.

### `plax log`

```
plax log <name> -- <text>
```

- Appends text to an existing record.
- Missing records are an error; `log` never creates a record implicitly.
- Record lookup uses the record file, not registry membership, so logging a
  preserved record after `down` remains supported.
- Diagnostics go to stderr; no record content is emitted unless a future
  explicit output mode is added.

### `plax record`

```
plax record [--json] <name>
```

- Default: complete record text to stdout.
- `--json`: parsed projection to stdout.
- Missing records and malformed records return non-zero.

`record` also resolves directly from the record file and therefore remains
available after `down`.

### `plax verdict`

```
plax verdict [--status pass|fail] [--contract pass|fail] <name> -- [summary]
```

- `--status` is required and is the task outcome declaration.
- `--contract` is required when the record declares a contract; it is omitted
  when no contract was declared.
- The optional summary is appended as verdict prose.
- A second verdict is rejected; use `plax log` for later historical notes.
- `plax verify` remains separate: it reports environment checks and does not
  automatically author a work verdict.

## Error handling

| Failure | Expected behavior |
|---|---|
| `--intent` file missing/unreadable/empty | Reject before side effects → exit non-zero |
| `--parent` names no instance | Reject before side effects → explain that parent must be registered |
| Parent has no accessible worktree | Reject before child creation → do not fall back to repository `HEAD` |
| Parent worktree has uncommitted changes | Reject → stacked ancestry must be an exact commit snapshot |
| `--parent` with `--ref` | Reject as ambiguous → choose one Git base |
| Parent record absent | Reject in the tracked-child form → parent lineage cannot be recorded honestly |
| Record already exists | Reject → creates do not collide; preserve the existing record |
| Record directory cannot be created | Fail `up` → roll back the instance |
| Record write fails during `up` | Fail `up` → roll back worktree, resources, and registry entry |
| Append target missing | Fail → `log` never creates a record implicitly |
| Append cannot complete | Fail → report the write error; never report success |
| Verdict status or contract value is not `pass` or `fail` | Reject → do not append a verdict |
| Verdict already exists | Reject → preserve the author-once first verdict; use `log` for later notes |
| Record malformed | `record` fails with path and parse error; preserve the file for inspection |
| Parent is down and its branch was deleted | Reject new child creation → use `resume` or recreate an explicit base; existing child records remain readable |
| Parent advances after child creation | No automatic mutation → child retains captured `base_commit` |
| `contract` contains unknown prose | Store it unchanged → arbitrary contracts are not executed in this phase |

## Tests

### Unit tests

- `TestRecord_Path_UsesRegistryDirectory` — records resolve under
  `.plax/records`, not a worktree.
- `TestRecord_CreateAndRead_RoundTripsHeadersAndBody` — required and
  optional fields round-trip without losing prose.
- `TestRecord_Create_MultilineIntentUsesHeaderSummaryAndBody` — the first
  non-empty line is the header summary and the complete intent remains in the
  body.
- `TestRecord_Read_RepeatedContractHeadersPreserveCommas` — contract entries
  remain distinct and commas require no escaping.
- `TestRecord_ParseSections_RejectsUnknownOrDuplicateVerdict` — body grammar
  is unambiguous and permits at most one author-once verdict while allowing
  log entries after it.
- `TestRecord_Create_RefusesExistingRecord` — create is non-destructive.
- `TestRecord_Create_IsAtomicOnFailure` — failed creation does not leave a
  parseable partial record.
- `TestRecord_Append_PreservesExistingBytes` — append never rewrites the
  original record.
- `TestRecord_Append_ConcurrentWriters` — exclusive appends remain parseable.
- `TestRecord_Read_UsesSharedLock` — reads do not observe a partial append.
- `TestRecord_LockFilePersists` — the sibling lock file remains after an
  operation.
- `TestRecord_Read_RejectsMissingRequiredHeaders` — malformed input fails
  clearly.
- `TestWorktree_CreateFromCommit_BasesBranchAtExactCommit` — child branch
  starts at the requested parent `HEAD`.
- `TestWorktree_CreateFromCommit_RejectsMissingCommit` — no fallback occurs.

### CLI tests (`cmd/plax/main_test.go`)

- `TestUp_WithIntent_CreatesRootRecord` — root `up` creates the instance and
  record, with intent on disk.
- `TestUp_WithParentAndIntent_CreatesStackedChild` — child branch starts at
  the parent's worktree `HEAD`, and record stores parent and base commit.
- `TestUp_ParentAndRefMutuallyExclusive` — invalid combination has no side
  effects.
- `TestUp_ParentMissing_FailsBeforeCreatingChild` — missing parent does not
  create a branch or worktree.
- `TestUp_ParentWithoutRecord_Fails` — tracked child requires a tracked
  parent.
- `TestUp_RecordFailure_RollsBack` — required record failure removes all
  resources created by `up`.
- `TestLog_AppendsToExistingRecord` — `plax log` adds timestamped prose.
- `TestLog_MissingRecord_Fails` — no implicit record creation.
- `TestLog_AfterDown_UsesPreservedRecord` — logging works without registry
  membership after teardown.
- `TestVerdict_AppendsStructuredVerdict` — status, contract status,
  timestamp, and summary are authored in the verdict section.
- `TestVerdict_RejectsSecondVerdict` — the first author-once outcome is preserved.
- `TestVerdict_DoesNotClaimVerifyResults` — task verdict authoring remains
  separate from fixed environment verification.
- `TestVerdict_AfterDown_UsesPreservedRecord` — verdict authoring works after
  the instance registry entry is removed.
- `TestRecord_DefaultPrintsOriginalText` — stdout is the complete file.
- `TestRecord_JSON_ProjectsParsedRecord` — JSON contains lineage, intent,
  contract, log, and verdict fields.
- `TestDown_PreservesRecord` — instance teardown does not remove its record.

### End-to-end tests (`cmd/plax/e2e_test.go`)

The real-Git scenario belongs in the existing end-to-end test file and follows
the repository convention of calling `t.Skip` when
`PLAX_TEST_POSTGRES_URL` is unset. It should create `i0`, commit a change,
create `i1` from `i0`, commit another change, and verify the Git ancestry is
`main -> i0 -> i1`. It should also verify that `i1` remains tied to its
captured commit when `i0` advances later. The test exercises real worktrees,
registry state, and teardown; Docker/Postgres are required because it calls
the actual `up` lifecycle.

## Acceptance criteria

- [x] `plax up --intent task.md i0` creates `.plax/records/i0.md` atomically and
  preserves it through `plax down i0`.
- [x] `plax up --parent i0 --intent child.md i1` requires an existing tracked
  parent and creates `i1` from i0's exact current worktree `HEAD`.
- [x] The child record contains `parent: i0` and the captured `base_commit`.
- [x] An external operator running from the main repository can create a root
  record without a synthetic parent instance.
- [x] `--parent` never silently falls back to repository `HEAD`, and cannot be
  combined with `--ref`.
- [x] `plax log <name> -- <text>` appends to an existing record without deleting
  or rewriting prior content.
- [x] `plax record <name>` prints the complete text record, and `--json` emits a
  stable structured projection.
- [x] `plax verdict <name> --status pass|fail` appends exactly one structured,
  author-once verdict and rejects subsequent verdicts while allowing later
  historical log entries.
- [x] Missing, malformed, duplicate, or failed records produce non-zero exits
  and actionable diagnostics.
- [x] Existing untracked `plax up <name>` behavior remains available with a
  warning; tracked records are opt-in.
- [x] `contract` is stored and surfaced, but no arbitrary contract syntax is
  claimed to be executed by the existing fixed-check `verify` command.
- [x] `plax verify` does not automatically create or overwrite a task verdict;
  environment health and task completion remain distinct facts.
- [x] Guide, manual, plan index, and the 2026-08-26 triage snapshot reference
  the shipped interface.

## Dependencies

No new external Go modules are required.

- Standard library: `encoding/json`, `errors`, `fmt`, `os`, `path/filepath`,
  `strings`, and `time` for record I/O and projection.
- Existing module: `golang.org/x/sys v0.47.0`, using `unix.Flock` for
  cross-process append serialization on the supported Unix platforms. A
  process-local `sync.Mutex` is insufficient because separate `plax log`
  invocations must not interleave writes.
- Existing module: `github.com/alecthomas/kong v1.16.0` for CLI parsing.
- Existing internal packages: `pkg/registry`, `pkg/worktree`, and the
  repository-root discovery helpers in `cmd/plax`.

The implementation must not add a database, daemon, agent runner, message
broker, or model dependency.
