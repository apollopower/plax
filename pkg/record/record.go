// Package record persists a durable, append-only work record per instance
// under .plax/records/<name>.md. The record is text rather than JSON so it
// stays greppable, diffable, and useful with ordinary UNIX tools.
package record

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Record is the parsed representation of an instance work record.
type Record struct {
	Instance   string     `json:"instance"`
	Parent     string     `json:"parent,omitempty"`
	BaseCommit string     `json:"base_commit,omitempty"`
	Intent     string     `json:"intent"`
	Contract   []string   `json:"contract,omitempty"`
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

// nameRe mirrors pkg/instance's instance-name rule. The record package is a
// leaf and cannot import instance, so the rule is duplicated here — keep the
// two in sync when one changes.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// fullSHARe matches a complete, unabbreviated Git commit ID.
var fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validateName(name string) error {
	if len(name) == 0 || len(name) > 32 {
		return fmt.Errorf("record: invalid instance name %q: must be 1-32 characters", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("record: invalid instance name %q: must match ^[a-z][a-z0-9_]*$", name)
	}
	return nil
}

// Path returns the repository-scoped record path.
func Path(repoRoot, name string) string {
	return filepath.Join(repoRoot, ".plax", "records", name+".md")
}

// Create writes a new record atomically and fails if the record already
// exists. The intent header is the first non-empty line of the supplied
// intent; the complete intent is copied below `## intent` in the body.
func Create(repoRoot string, input CreateInput) error {
	if err := validateName(input.Instance); err != nil {
		return err
	}
	intent := strings.TrimSpace(input.Intent)
	if intent == "" {
		return errors.New("record: intent is required")
	}
	if (input.Parent == "") != (input.BaseCommit == "") {
		return errors.New("record: 'parent' and 'base_commit' must be set together")
	}
	if input.BaseCommit != "" && !fullSHARe.MatchString(input.BaseCommit) {
		return fmt.Errorf("record: base_commit %q is not a full 40-hex commit SHA", input.BaseCommit)
	}
	for _, c := range input.Contract {
		if strings.ContainsAny(c, "\r\n") {
			return fmt.Errorf("record: contract entry must be a single line: %q", c)
		}
	}

	path := Path(repoRoot, input.Instance)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("record: creating directory: %w", err)
	}

	// The sibling lock file is persistent metadata: created with the record
	// directory and never removed after an operation.
	lockP := lockPath(path)
	if f, err := os.OpenFile(lockP, os.O_CREATE|os.O_RDWR, 0600); err != nil {
		return fmt.Errorf("record: creating lock file: %w", err)
	} else {
		_ = f.Close()
	}

	// Write the full record to a temp file, then hard-link it into place:
	// the link fails when the record already exists (creates never collide)
	// and the record appears fully formed or not at all.
	tmp, err := os.CreateTemp(dir, "."+input.Instance+"-*.tmp")
	if err != nil {
		return fmt.Errorf("record: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(renderRecord(input, intent)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("record: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("record: closing temp file: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		if os.IsExist(err) {
			return fmt.Errorf("record: record for instance %q already exists", input.Instance)
		}
		return fmt.Errorf("record: linking record into place: %w", err)
	}
	return os.Remove(tmpPath)
}

// renderRecord serializes a record: single-line headers, the `---`
// separator, operator body prose (when any), and the `## intent` section.
func renderRecord(input CreateInput, intent string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "instance: %s\n", input.Instance)
	if input.Parent != "" {
		fmt.Fprintf(&b, "parent: %s\n", input.Parent)
	}
	if input.BaseCommit != "" {
		fmt.Fprintf(&b, "base_commit: %s\n", input.BaseCommit)
	}
	firstLine := intent
	if i := strings.IndexByte(intent, '\n'); i >= 0 {
		firstLine = intent[:i]
	}
	fmt.Fprintf(&b, "intent: %s\n", firstLine)
	for _, c := range input.Contract {
		fmt.Fprintf(&b, "contract: %s\n", c)
	}
	b.WriteString("---\n")
	if body := strings.TrimSpace(input.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	b.WriteString("## intent\n")
	b.WriteString(intent)
	b.WriteString("\n")
	return []byte(b.String())
}

// Append appends a timestamped prose entry to an existing record. It never
// creates a record implicitly and never rewrites prior bytes.
func Append(repoRoot, name, text string, now time.Time) error {
	if err := validateName(name); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("record: log text is required")
	}
	path := Path(repoRoot, name)
	if !fileExists(path) {
		return fmt.Errorf("record: no record for instance %q — 'plax log' never creates a record", name)
	}
	lk, err := acquireLock(lockPath(path), false)
	if err != nil {
		return err
	}
	defer lk.close()

	entry := "\n## log\nat: " + now.Format(time.RFC3339) + "\n" + text + "\n"
	return appendBytes(path, entry)
}

// WriteVerdict appends the single author-once verdict section to an existing
// record, rejecting a second verdict. The verdict is the operator's
// declaration; it does not claim plax independently validated the contract.
func WriteVerdict(repoRoot, name string, v Verdict, now time.Time) error {
	if err := validateName(name); err != nil {
		return err
	}
	if v.Status != "pass" && v.Status != "fail" {
		return fmt.Errorf("record: verdict status must be %q or %q, got %q", "pass", "fail", v.Status)
	}
	if v.Contract != "" && v.Contract != "pass" && v.Contract != "fail" {
		return fmt.Errorf("record: verdict contract must be %q or %q, got %q", "pass", "fail", v.Contract)
	}
	path := Path(repoRoot, name)
	if !fileExists(path) {
		return fmt.Errorf("record: no record for instance %q", name)
	}
	lk, err := acquireLock(lockPath(path), false)
	if err != nil {
		return err
	}
	defer lk.close()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("record: reading %s: %w", path, err)
	}
	rec, err := parseRecord(data, name)
	if err != nil {
		return fmt.Errorf("record: %s is malformed — fix or delete it before authoring a verdict: %w", path, err)
	}
	if rec.Verdict != nil {
		return fmt.Errorf("record: verdict for instance %q already exists — author-once; use 'plax log' for later notes", name)
	}
	if len(rec.Contract) > 0 && v.Contract == "" {
		return fmt.Errorf("record: --contract is required for instance %q because its record declares a contract", name)
	}

	var b strings.Builder
	b.WriteString("\n## verdict\n")
	fmt.Fprintf(&b, "status: %s\n", v.Status)
	if v.Contract != "" {
		fmt.Fprintf(&b, "contract: %s\n", v.Contract)
	}
	fmt.Fprintf(&b, "at: %s\n", now.Format(time.RFC3339))
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	return appendBytes(path, b.String())
}

// Read parses an existing record under a shared lock, so it never observes
// a concurrent partial append.
func Read(repoRoot, name string) (Record, error) {
	if err := validateName(name); err != nil {
		return Record{}, err
	}
	path := Path(repoRoot, name)
	if !fileExists(path) {
		return Record{}, fmt.Errorf("record: no record for instance %q — run 'plax up --intent <file> %s' to create one", name, name)
	}
	lk, err := acquireLock(lockPath(path), true)
	if err != nil {
		return Record{}, err
	}
	defer lk.close()

	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("record: reading %s: %w", path, err)
	}
	rec, err := parseRecord(data, name)
	if err != nil {
		return Record{}, fmt.Errorf("record: parsing %s: %w", path, err)
	}
	return rec, nil
}

// ReadText returns the complete record text under a shared lock, preserving
// the original bytes for default output.
func ReadText(repoRoot, name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	path := Path(repoRoot, name)
	if !fileExists(path) {
		return "", fmt.Errorf("record: no record for instance %q — run 'plax up --intent <file> %s' to create one", name, name)
	}
	lk, err := acquireLock(lockPath(path), true)
	if err != nil {
		return "", err
	}
	defer lk.close()

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("record: reading %s: %w", path, err)
	}
	return string(data), nil
}

// lockPath is the persistent sibling lock file: <name>.lock next to
// <name>.md. Writers hold it exclusively and readers share it.
func lockPath(path string) string {
	return strings.TrimSuffix(path, ".md") + ".lock"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// appendBytes writes under the already-held lock, rolling the file back to
// its prior size if the write cannot complete so a partial entry is never
// left behind.
func appendBytes(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("record: opening %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("record: stat %s: %w", path, err)
	}
	orig := info.Size()
	n, err := f.WriteString(data)
	if err != nil || n != len(data) {
		// Never claim a successful append when the write did not complete.
		_ = f.Truncate(orig)
		_ = f.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("record: appending to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Truncate(path, orig)
		return fmt.Errorf("record: closing %s after append: %w", path, err)
	}
	return nil
}

// parseRecord parses the on-disk grammar: headers, `---`, then body
// sections. Sections run until the next `##` header; plain prose before the
// first section is the record body.
func parseRecord(data []byte, wantName string) (Record, error) {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	sep := -1
	for i, ln := range lines {
		if ln == "---" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return Record{}, errors.New("missing '---' separator")
	}
	if sep == 0 {
		return Record{}, errors.New("no headers before '---'")
	}

	var rec Record
	intentHeaderSeen := false
	for _, ln := range lines[:sep] {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		key, val, ok := strings.Cut(ln, ": ")
		if !ok {
			return Record{}, fmt.Errorf("malformed header line %q", ln)
		}
		switch key {
		case "instance":
			rec.Instance = val
		case "parent":
			rec.Parent = val
		case "base_commit":
			rec.BaseCommit = val
		case "intent":
			// The header is only the summary line; the complete intent
			// prose comes from the `## intent` section below.
			intentHeaderSeen = true
		case "contract":
			rec.Contract = append(rec.Contract, val)
		default:
			return Record{}, fmt.Errorf("unknown header %q", key)
		}
	}
	if rec.Instance == "" {
		return Record{}, errors.New("missing required header 'instance'")
	}
	if rec.Instance != wantName {
		return Record{}, fmt.Errorf("instance %q does not match requested name %q", rec.Instance, wantName)
	}
	if !intentHeaderSeen {
		return Record{}, errors.New("missing required header 'intent'")
	}
	if (rec.Parent == "") != (rec.BaseCommit == "") {
		return Record{}, errors.New("'parent' and 'base_commit' must be set together")
	}

	body := lines[sep+1:]
	var bodyProse []string
	i := 0
	for i < len(body) && !strings.HasPrefix(body[i], "## ") {
		bodyProse = append(bodyProse, body[i])
		i++
	}
	rec.Body = strings.TrimSpace(strings.Join(bodyProse, "\n"))

	intentSeen := false
	for i < len(body) {
		ln := body[i]
		if !strings.HasPrefix(ln, "## ") {
			// Prose is only valid before the first section; anything else
			// belongs to the previous section's content.
			i++
			continue
		}
		name := strings.TrimSpace(ln[len("## "):])
		var content []string
		i++
		for i < len(body) && !strings.HasPrefix(body[i], "## ") {
			content = append(content, body[i])
			i++
		}
		text := strings.TrimSpace(strings.Join(content, "\n"))
		switch name {
		case "intent":
			if intentSeen {
				return Record{}, errors.New("duplicate '## intent' section")
			}
			intentSeen = true
			rec.Intent = text
		case "log":
			entry, err := parseLogSection(text)
			if err != nil {
				return Record{}, err
			}
			rec.Log = append(rec.Log, entry)
		case "verdict":
			if rec.Verdict != nil {
				return Record{}, errors.New("duplicate '## verdict' section")
			}
			v, err := parseVerdictSection(text)
			if err != nil {
				return Record{}, err
			}
			rec.Verdict = &v
		default:
			return Record{}, fmt.Errorf("unknown body section '## %s'", name)
		}
	}
	if !intentSeen {
		return Record{}, errors.New("missing '## intent' section")
	}
	return rec, nil
}

func parseLogSection(text string) (LogEntry, error) {
	lines := strings.Split(text, "\n")
	key, val, ok := strings.Cut(lines[0], ": ")
	if !ok || key != "at" {
		return LogEntry{}, errors.New("log section missing 'at: <RFC3339>' line")
	}
	ts, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		return LogEntry{}, fmt.Errorf("log section has unparseable timestamp %q: %w", val, err)
	}
	return LogEntry{At: ts, Text: strings.TrimSpace(strings.Join(lines[1:], "\n"))}, nil
}

func parseVerdictSection(text string) (Verdict, error) {
	lines := strings.Split(text, "\n")
	key, val, ok := strings.Cut(lines[0], ": ")
	if !ok || key != "status" {
		return Verdict{}, errors.New("verdict section missing 'status' line")
	}
	if val != "pass" && val != "fail" {
		return Verdict{}, fmt.Errorf("verdict status %q is not pass or fail", val)
	}
	v := Verdict{Status: val}
	i := 1
	if i < len(lines) {
		if k2, v2, ok := strings.Cut(lines[i], ": "); ok && k2 == "contract" {
			if v2 != "pass" && v2 != "fail" {
				return Verdict{}, fmt.Errorf("verdict contract %q is not pass or fail", v2)
			}
			v.Contract = v2
			i++
		}
	}
	if i >= len(lines) {
		return Verdict{}, errors.New("verdict section missing 'at' line")
	}
	k3, v3, ok := strings.Cut(lines[i], ": ")
	if !ok || k3 != "at" {
		return Verdict{}, errors.New("verdict section missing 'at' line")
	}
	ts, err := time.Parse(time.RFC3339Nano, v3)
	if err != nil {
		return Verdict{}, fmt.Errorf("verdict section has unparseable timestamp %q: %w", v3, err)
	}
	v.At = ts
	v.Summary = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
	return v, nil
}
