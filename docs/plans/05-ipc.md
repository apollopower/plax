# Phase 5 — Agent IPC (Mailbox)

## Objective

Add inter-instance message passing so parallel agents can coordinate:
`send` writes a message into another instance's mailbox, `recv` reads and
removes messages from its own. Built on filesystem primitives — no daemon,
no locking, no network. Survives suspend/resume because messages wait on
disk.

---

## Package layout

```
cmd/plax/main.go               # Add SendCmd, RecvCmd to kong CLI
pkg/
  mailbox/
    mailbox.go                  # Message struct, Write/Read/Count functions
    mailbox_test.go             # Read/write round-trip, ordering, concurrent write
  instance/
    up.go                       # Create .plax/mail/<name>/ on up
    down.go                     # Remove .plax/mail/<name>/ on down (best-effort, after worktree removal)
```

Mailbox is filesystem-only. No new registry fields — the unread count is a
directory listing, not stored state. No new blueprint fields — the mailbox
is always present; hiding it would be a no-op.

---

## Type specifications

### `pkg/mailbox/mailbox.go`

```go
package mailbox

import "time"

type Message struct {
	From      string `json:"from"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"` // RFC 3339, set by sender
}

// Write atomically creates a new message file in dir. The filename is
// <unixnanos>_<base64url_nonce>.json so concurrent senders never collide
// (O_CREATE|O_EXCL|O_WRONLY via os.OpenFile) and default lexical sort is
// chronological. Returns the filename written so callers can echo it.
func Write(dir string, msg *Message) (filename string, err error)

// ReadOldest returns the N oldest messages in dir (lexicographic sort on
// filenames, which are nanosecond timestamps). Files are removed after
// being read. A partial failure (one file unreadable) continues with the
// rest. len(result) gives the number actually read and removed.
func ReadOldest(dir string, n int) ([]Message, error)

// ReadAll reads and removes every message in dir.
func ReadAll(dir string) ([]Message, error)

// Count returns the number of message files in dir. A missing directory
// returns 0, nil.
func Count(dir string) (int, error)
```

**Message filename:** `<unix_nano>_<8 random bytes base64url>.json`.
Nanosecond wall-clock time from the sender guarantees monotonic ordering
across concurrent writes from different processes, and the random suffix
prevents two writes in the same nanosecond from colliding.

**Validation rules:**

| # | Rule | Error |
|---|---|---|
| M1 | `dir` must be an existing directory | `mailbox: directory <dir> does not exist` |
| M2 | `msg.Body` must be non-empty | `mailbox: message must have a body` |
| M3 | `msg.Timestamp` if empty is set to `time.Now().UTC().Format(time.RFC3339)` by `Write` | — |

On-disk format:

```json
{
  "from": "i2",
  "subject": "review request",
  "body": "Can you look at PR #42 when you get a chance?",
  "timestamp": "2026-08-03T10:00:00Z"
}
```

Every field except `body` may be empty. `subject` is optional.
No size limit is enforced — this is the machine's own filesystem.

### `pkg/instance` changes

No new types. `up.go` creates the mailbox directory; `down.go` removes it.

| Function | Change |
|---|---|
| `Up` | After port/branch setup, before process spawn: `os.MkdirAll(".plax/mail/<name>", 0755)`. Error fails `up`. |
| `Down` | After worktree removal (step 6), before registry removal (step 7): remove `".plax/mail/<name>/"` (`os.RemoveAll`). Best-effort — os.RemoveAll on a missing dir is a no-op. |

---

## Algorithms

### `plax send` — `Write`

1. **Resolve target.** `deps.Registry.GetInstance(cmd.Name)` must find the
   instance (any state — suspended instances can receive mail).
   ⚠ Not found → `instance "<name>" not found`. Exit 1.

2. **Build message.**
   - `From`: `cmd.From`; if empty and `PLAX_INSTANCE` env var is set, use
     that; otherwise `""`.
   - `Subject`: `cmd.Subject` (may be empty).
   - `Body`: `cmd.Body` (the positional args joined with spaces).
   - `Timestamp`: current time in RFC 3339.
   ⚠ Empty `from` is allowed but logged as a warning to stderr:
     `send: no sender set — pass --from or set PLAX_INSTANCE`.

3. **Validate** M2. `from` empty → warning only (not an error; the
   receiver can inspect the filename for origin, and the design doc expects
   prose as the first message format). `body` empty → error
   `send: body is required; use '-- <text>'`.

4. **Write.** `mailbox.Write(mailDir, msg)`. The directory was created by
   `up`; missing directory → error `mailbox for "<name>" missing — instance
   may have been created before Phase 5; run 'plax down <name>' and 'plax
   up <name>' to rebuild`.

5. **Print** filename to stderr: `message written: <filename>`.

### `plax recv` — `ReadOldest` / `ReadAll`

1. **Resolve own instance.** `deps.Registry.GetInstance(cmd.Name)` — must
   exist.
   ⚠ Not found → error.

2. **Read.**
   - `--all` → `mailbox.ReadAll(mailDir)`.
   - Otherwise → `mailbox.ReadOldest(mailDir, cmd.Count)` (default 1).
   - Empty mailbox → print `no messages` to stderr, exit 0.
   ⚠ Missing directory → 0 messages (defensive; directory should
     always exist for a registered instance).

3. **Output.** Default format (stdout):
   ```
   From: i2
   Subject: review request
   ---
   Can you look at PR #42 when you get a chance?
   ---
   N message(s) remaining
   ```
   `--json` → JSON array of `Message` structs to stdout. The remaining
   count is printed to stderr.

   Multiple messages are separated by a blank line between `---` and the
   next `From:` header.

   ⚠ Unknown fields in the JSON are tolerated (`json.Decoder` without
     `DisallowUnknownFields` — forward-compat).

4. **Remove.** Files are deleted after being successfully read. A read
   failure on one file skips it and continues (stderr warning); the
   remaining count accounts only for removed files.

### `plax ls` — mailbox column

1. After loading the registry and blueprint, the CLI adds a loop over
   instances sorted by name.
2. For each instance: `mailbox.Count(".plax/mail/<name>")`.
3. The table header and rows gain a `MAIL` column between `BRANCH` and
   `PORTS`. Format: `%-8s %-10s %-20s %-5s %-24s %s`.

   ```
   NAME     STATE     BRANCH               MAIL  PORTS                    CREATED
   i1       running   plax/feature-auth    2     3001 5433 6380 3031      5m ago
   i2       running   plax/feature-fix     0     3002 5434 6381 3032      3m ago
   ```

   ⚠ `--json` output is unchanged: the count is derived, not stored;
     adding a `mail` key to each instance record in the JSON output is
     acceptable but not required for this phase.

### `plax attach` — mailbox notification

After the existing suspended/drift notices and before spawning the shell:

1. `mailbox.Count(".plax/mail/<name>")`.
2. If > 0, print to stderr:
   `note: <N> unread message(s) — run 'plax recv <name>' to read`.

No notification is printed for `plax exec` — `exec` is for running
commands, not interactive sessions.

### Mailbox lifecycle

| Event | Action |
|---|---|
| `plax up` | `os.MkdirAll(".plax/mail/<name>", 0755)` after worktree/branch setup |
| `plax down` | `os.RemoveAll(".plax/mail/<name>")` after worktree removal, best-effort |
| `plax suspend` | No action — mail survives (it's files on disk) |
| `plax resume` | No action — directory already exists |
| `plax base refresh` | No action — mailbox is per-instance, not per-base |

---

## CLI specification

### `plax send <name>`

```
plax send <name> [--subject <text>] [--from <instance>] -- <body...>
```

| Aspect | Value |
|---|---|
| Args | `<name>` — target instance (required); `<body...>` — message body (passthrough after `--`, required) |
| Flags | `--root <path>` (optional, default `.`), `--subject <text>`, `--from <instance>` |
| Env | `PLAX_INSTANCE` — fallback sender identity when `--from` is not set |
| Exit 0 | Message written |
| Exit 1 | Instance not found; body empty; mailbox dir missing; write failed |
| Stdout | Nothing |
| Stderr | `message written: <filename>`; warnings |

The body is read from the positional arguments after `--`. Kong's
`arg:""` with `passthrough:""` supports this pattern (same as `exec`
without the shell — the CLI separates the body from the flags).

### `plax recv <name>`

```
plax recv <name> [--all | --count <N>] [--json]
```

| Aspect | Value |
|---|---|
| Args | `<name>` — instance name (required) |
| Flags | `--root <path>` (optional, default `.`), `--all` (read all), `--count <N>` (default 1), `--json` |
| Exit 0 | Messages read (including empty mailbox) |
| Exit 1 | Instance not found; read error on all messages |
| Stdout | Formatted text (default) or JSON array |
| Stderr | Remaining count (text mode); read warnings |

`--all` and `--count` are mutually exclusive; Kong enforces this with a
`xor` group. When neither is set, default is `--count 1`.

### Modified commands

| Command | Change |
|---|---|
| `plax up` | Creates `.plax/mail/<name>/` directory. Failure fails `up`. |
| `plax down` | Removes `.plax/mail/<name>/` directory (best-effort). |
| `plax ls` | Adds `MAIL` column between `BRANCH` and `PORTS`. |
| `plax attach` | Prints unread-message notification to stderr before spawning the shell. |

`plax exec`, `plax suspend`, `plax resume`, `plax status`, `plax doctor`,
`plax rederive`, and `plax base *` are unchanged.

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| `send` to unknown instance | `GetInstance` false | `instance "<name>" not found`. Exit 1. |
| `send` with empty body | `len(body) == 0` | `send: body is required; use '-- <text>'`. Exit 1. |
| `send` with empty from | `cmd.From == "" && os.Getenv("PLAX_INSTANCE") == ""` | Warning to stderr, continue. Message written with `"from": ""`. |
| `send` to missing mailbox dir | `os.Stat` → `ErrNotExist` | `mailbox for "<name>" missing — instance may have been created before Phase 5`. Exit 1. |
| `send` with write failure | `os.OpenFile` / `json.Encode` error | `send: write: <err>`. Exit 1. No partial file (O_EXCL ensures atomic create). |
| `recv` on unknown instance | `GetInstance` false | `instance "<name>" not found`. Exit 1. |
| `recv` on empty mailbox | `Count` returns 0 | `no messages`. Exit 0. |
| `recv` file unreadable (permission, corrupt JSON) | `os.ReadFile` / `json.Decode` error | Warning to stderr: `recv: skipping <file>: <err>`. Continue with remaining files. |
| `recv` all files unreadable | All reads fail | `recv: all <N> messages unreadable`. Exit 1. |
| `recv` remove failure | `os.Remove` error | Warning. File is skipped. Remaining count excludes it. |
| `ls` count failure | `Count` error | Show `?` in the MAIL column. Continue listing other instances. |
| `attach` count failure | `Count` error | Silently skip the notification. Attach proceeds. |
| `up` mailbox creation failure | `os.MkdirAll` error | `mailbox: create: <err>`. Exit 1. Rollback as normal. |
| `down` mailbox removal failure | `os.RemoveAll` error | Warning to stderr. Continue. The mailbox directory is at the repo root and is not swept by worktree removal, but rm errors are non-fatal. |

---

## Tests

### Test prerequisites

No external dependencies. Mailbox is filesystem-only — tests run with
`go test -race` on temp directories. No Postgres, no Docker, no git.
They run everywhere.

### Unit tests

**`pkg/mailbox/mailbox_test.go`:**

- `TestWrite_Success` — write a message, file exists, content deserializes
  back to the same struct
- `TestWrite_FillsTimestamp` — empty timestamp is set by Write
- `TestWrite_EmptyBody` — returns error (M2)
- `TestWrite_EmptyFrom` — succeeds (warning is the CLI's job);
  `"from": ""` in the serialized JSON
- `TestWrite_NonExistentDir` — returns error (M1)
- `TestWrite_ConcurrentNoCollision` — 100 goroutines each write one message
  concurrently; 100 unique files created, none overwritten
- `TestWrite_FilenameOrdering` — write messages 10ms apart; lexicographic
  sort on filenames matches chronological order
- `TestReadOldest_NMessages` — write 5 messages, read 2 → 2 returned, 3
  files remain
- `TestReadOldest_MoreThanExist` — write 1, ask for 5 → 1 returned, all
  removed, `read` count is 1
- `TestReadOldest_EmptyDir` — 0 messages, no error
- `TestReadOldest_RemovesAfterRead` — read 1 → 1 returned, file deleted
- `TestReadOldest_SkipsUnreadable` — write 3 messages, chmod one to 0000 →
  2 returned, unreadable skipped with `read` count excluding it
- `TestReadAll_Success` — write 10 → all returned, dir empty
- `TestReadAll_EmptyDir` — nil, nil
- `TestCount_ReturnsFileCount` — write 3 → Count returns 3
- `TestCount_MissingDir` — Count on nonexistent path returns 0, nil
- `TestCount_EmptyDir` — Count on empty dir returns 0, nil

**`pkg/instance/up_test.go`** (addition):

- `TestUp_CreatesMailboxDir` — after `up`, `.plax/mail/<name>/` exists
  with 0755 permissions

**`pkg/instance/down_test.go`** (addition):

- `TestDown_RemovesMailboxDir` — after `down`, `.plax/mail/<name>/` does
  not exist
- `TestDown_NoMailboxDir` — down on instance created before Phase 5 (no
  mail dir) still succeeds (best-effort)

### End-to-end tests (`cmd/plax/e2e_test.go` additions)

`TestEndToEnd_Mailbox` (requires git + Postgres + Docker + python3; skipped
otherwise, holds the advisory lock):

- `up i1` and `up i2` → `send i2 --subject "hello" --from i1 -- hi there`
  exits 0 → `ls` shows `MAIL` column with 1 for i2, 0 for i1 → `recv i2`
  prints the message with From/Subject/Body → `ls` shows 0 for i2
- `send i2 --from i1 -- msg1` → `send i2 --from i1 -- msg2` → `send i2
  --from i1 -- msg3` → `recv i2 --count 2` prints 2 messages, 1 remaining
  → `recv i2 --all` prints 1 message, `no messages` on next call
- `recv i2 --json` exits 0 with valid JSON array
- `attach i1` (stdin `exit`) with mail for i1 prints the notification to
  stderr
- `send nonexistent -- hi` exits 1 with `instance "nonexistent" not found`
- `send i2 -- ` (empty body) exits 1 with `body is required`
- `suspend i2` → `send i2 --from i1 -- still works` → `resume i2` → mail
  survives → `recv i2` prints it
- `down i1` → `send i1 --from i2 -- hi` exits 1 (instance not found), not
  0 with a dangling mailbox
- `recv i2` on empty mailbox prints `no messages`, exits 0
- `--json` flag on `recv` plus `| jq` to verify the JSON structure

---

## Acceptance criteria

- [x] `plax send i2 --subject "hello" --from i1 -- hi there` writes a
  message file; `plax ls` shows `MAIL: 1` for i2
- [x] `plax recv i2` prints the message with From/Subject/Body and the
  remaining count; the file is removed
- [x] `plax recv i2 --json` prints a valid JSON array (empty or populated)
- [x] `plax recv i2 --all` reads every message; subsequent `recv` prints
  `no messages`
- [x] `plax recv i2 --count 2` reads exactly 2 messages
- [x] `plax ls` table output includes a `MAIL` column between `BRANCH` and
  `PORTS`; `--json` output is backward-compatible (unchanged structure)
- [x] `plax attach i1` with unread mail prints `note: <N> unread
  message(s) — run 'plax recv <name>' to read`
- [x] `plax exec` does NOT print a mailbox notification
- [x] Mail survives `suspend` / `resume`: messages sent before suspend are
  readable after resume
- [x] `plax down` removes the mailbox directory cleanly
- [x] `plax send` to a suspended instance succeeds; the instance does not
  need to be running
- [x] Sending with an empty `--from` and no `PLAX_INSTANCE` prints a
  warning but succeeds with `"from": ""`
- [x] Concurrent `plax send` calls to the same instance from two terminals
  create two distinct files with no collision
- [x] `plax send` with empty body (no `-- <text>`) exits 1 with a clear
  error
- [x] `plax recv nonexistent` exits 1 with a clear error
- [x] `gofmt -s`, `go vet ./...`, `go test -race -count=1 ./...`, and
  `golangci-lint run` all pass
- [x] End-to-end mailbox test passes with `go test -race ./cmd/plax/ -run
  TestEndToEnd_Mailbox -count=1` (skipped without Postgres/Docker)

---

## Dependencies

No new external modules. Everything builds on the standard library and what
is already in `go.mod`:

| Need | Import | Already used by |
|---|---|---|
| CLI framework | `github.com/alecthomas/kong` | `main.go` |
| JSON marshal/unmarshal | `encoding/json` | `blueprint`, `registry`, `status` |
| Atomic file create | `os.OpenFile` (O_CREATE\|O_EXCL\|O_WRONLY) | — |
| Random nonce | `crypto/rand` | — |
| Directory listing | `os.ReadDir` | — |
| File I/O | `os`, `io` | everywhere |
| Time formatting | `time` | `registry`, `provenance` |
| Base64url encoding | `encoding/base64` | — |

`crypto/rand` and `encoding/base64` are new imports in `pkg/mailbox/` but
are standard library — no module changes.

---

## Concurrency note

Two concurrent `send` calls write to the same mailbox directory. The
filename format (`<unix_nanos>_<base64url_nonce>.json`) and `O_CREATE|O_EXCL`
semantics prevent collision: `O_EXCL` fails if the file already exists, and
in the same nanosecond the random suffix guarantees a unique filename. Two
processes writing the same nanosecond is a 1-in-2^64 chance per byte of
nonce; 8 random bytes makes it effectively impossible.

Reading and removing is not locked. `recv` removes each file after
reading it, so a concurrent `recv` from two terminals may race (each
reads some subset). This is acceptable — the same sender would otherwise
need their own coordination, and instances are single-occupant by design
(design §6).

---

## Deferred items

| Item | Deferred to | Reason |
|---|---|---|
| Structured message protocol | When two agents actually need to negotiate | Prose covers `send` today; the design doc says "expect the first honest answer to be prose" (§7) |
| Reply-to / thread headers | When asked | Adds parsing surface without a demonstrated need |
| Message delivery acknowledgement | When a receiver needs to signal "I handled this" | Same as above; `recv` removes on read, which is ack-by-consumption |
| Binary message bodies | When files need to pass between instances | git is for code; the mailbox is for coordination prose |
| Mailbox size limits / pruning | When mailboxes fill up over many suspend cycles | `down` removes them; long-running instances may accumulate noise but that's operator policy |
| `recv --keep` (read without remove) | When an agent needs to peek | `--json` with a follow-up `recv` covers the probe-then-read pattern |
