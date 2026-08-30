package record

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plax.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRecord_Path_UsesRegistryDirectory(t *testing.T) {
	repo := initRepo(t)
	got := Path(repo, "i1")
	want := filepath.Join(repo, ".plax", "records", "i1.md")
	if got != want {
		t.Errorf("Path = %q, want %q (records live beside the registry, not in the worktree)", got, want)
	}
}

func TestRecord_CreateAndRead_RoundTripsHeadersAndBody(t *testing.T) {
	repo := initRepo(t)
	input := CreateInput{
		Instance:   "i1",
		Parent:     "i0",
		BaseCommit: "0123456789abcdef0123456789abcdef01234567",
		Intent:     "add retry coverage\nThe child task was assigned from i0's billing work.",
		Contract:   []string{"tests", "typecheck"},
		Body:       "operator notes about the approach.",
	}
	if err := Create(repo, input); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec, err := Read(repo, "i1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := Record{
		Instance:   "i1",
		Parent:     "i0",
		BaseCommit: "0123456789abcdef0123456789abcdef01234567",
		Intent:     "add retry coverage\nThe child task was assigned from i0's billing work.",
		Contract:   []string{"tests", "typecheck"},
		Body:       "operator notes about the approach.",
	}
	if diff := cmp.Diff(want, rec); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestRecord_Create_MultilineIntentUsesHeaderSummaryAndBody(t *testing.T) {
	repo := initRepo(t)
	input := CreateInput{
		Instance: "i1",
		Intent:   "\n\nadd retry coverage\nThe child task was assigned from i0's billing work.\n\n",
	}
	if err := Create(repo, input); err != nil {
		t.Fatalf("Create: %v", err)
	}

	text, err := ReadText(repo, "i1")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "intent: ") {
			if line != "intent: add retry coverage" {
				t.Errorf("header summary should be the first non-empty line, got %q", line)
			}
		}
	}
	if !strings.Contains(text, "## intent\nadd retry coverage\nThe child task was assigned from i0's billing work.\n") {
		t.Errorf("complete intent must be copied below ## intent:\n%s", text)
	}

	rec, err := Read(repo, "i1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.Intent != "add retry coverage\nThe child task was assigned from i0's billing work." {
		t.Errorf("parsed intent = %q, want the complete prose", rec.Intent)
	}
}

func TestRecord_Read_RepeatedContractHeadersPreserveCommas(t *testing.T) {
	repo := initRepo(t)
	input := CreateInput{
		Instance: "i1",
		Intent:   "task",
		Contract: []string{"unit tests, integration tests pass", "typecheck", "lint, vet clean"},
	}
	if err := Create(repo, input); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec, err := Read(repo, "i1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{"unit tests, integration tests pass", "typecheck", "lint, vet clean"}
	if diff := cmp.Diff(want, rec.Contract); diff != "" {
		t.Errorf("contract entries (-want +got):\n%s", diff)
	}
}

func TestRecord_ParseSections_RejectsUnknownOrDuplicateVerdict(t *testing.T) {
	cases := []struct {
		name  string
		extra string
	}{
		{"unknown section", "\n## notes\nsome prose\n"},
		{"duplicate verdict", "\n## verdict\nstatus: pass\nat: 2026-08-29T12:00:00Z\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err != nil {
				t.Fatal(err)
			}
			if tc.name == "duplicate verdict" {
				if err := WriteVerdict(repo, "i1", Verdict{Status: "pass"}, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			}
			path := Path(repo, "i1")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// A log section after the verdict must parse; an unknown section
			// or a second verdict must not.
			if err := os.WriteFile(path, append(data, []byte(tc.extra)...), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = Read(repo, "i1")
			if err == nil {
				t.Fatal("Read should reject the malformed body")
			}
		})
	}

	// Log entries after the verdict remain legal.
	repo := initRepo(t)
	if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteVerdict(repo, "i1", Verdict{Status: "pass"}, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WriteVerdict: %v", err)
	}
	if err := Append(repo, "i1", "historical note after the verdict", time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Append after verdict: %v", err)
	}
	rec, err := Read(repo, "i1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.Verdict == nil || len(rec.Log) != 1 || rec.Log[0].Text != "historical note after the verdict" {
		t.Errorf("log after verdict not preserved: verdict=%v log=%v", rec.Verdict, rec.Log)
	}
}

func TestRecord_Create_RefusesExistingRecord(t *testing.T) {
	repo := initRepo(t)
	input := CreateInput{Instance: "i1", Intent: "first"}
	if err := Create(repo, input); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := Create(repo, CreateInput{Instance: "i1", Intent: "second"}); err == nil {
		t.Fatal("second Create should fail")
	}
	rec, err := Read(repo, "i1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.Intent != "first" {
		t.Errorf("existing record was clobbered: intent = %q", rec.Intent)
	}
}

func TestRecord_Create_IsAtomicOnFailure(t *testing.T) {
	repo := initRepo(t)
	// A directory squatting on the target path makes the atomic link step
	// fail: no partial record may appear and no temp file may remain.
	dir := filepath.Join(repo, ".plax", "records")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "i1.md"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err == nil {
		t.Fatal("Create should fail when the record path is blocked")
	}
	if _, err := Read(repo, "i1"); err == nil {
		t.Error("failed creation left a parseable record")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind after failed create: %s", e.Name())
		}
	}
}

func TestRecord_Append_PreservesExistingBytes(t *testing.T) {
	repo := initRepo(t)
	if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	before, err := ReadText(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(repo, "i1", "Found the retry path; adding a regression test.", time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after, err := ReadText(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(after, before) {
		t.Errorf("append rewrote prior bytes:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !strings.Contains(after, "at: 2026-08-29T12:00:00Z\nFound the retry path; adding a regression test.") {
		t.Errorf("timestamped entry missing:\n%s", after)
	}
}

func TestRecord_Append_ConcurrentWriters(t *testing.T) {
	repo := initRepo(t)
	if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_ = Append(repo, "i1", fmt.Sprintf("note %d", i), time.Now())
		}(i)
	}
	close(start)
	wg.Wait()

	rec, err := Read(repo, "i1")
	if err != nil {
		t.Fatalf("Read after concurrent appends: %v", err)
	}
	if len(rec.Log) != n {
		t.Errorf("log entries = %d, want %d", len(rec.Log), n)
	}
	seen := map[string]bool{}
	for _, e := range rec.Log {
		seen[e.Text] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("note %d", i)] {
			t.Errorf("missing entry note %d", i)
		}
	}
}

func TestRecord_Read_UsesSharedLock(t *testing.T) {
	repo := initRepo(t)
	if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}

	lk, err := acquireLock(lockPath(Path(repo, "i1")), false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Read(repo, "i1")
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("Read completed while the exclusive lock was held")
	case <-time.After(200 * time.Millisecond):
	}
	lk.close()
	if err := <-done; err != nil {
		t.Fatalf("Read after unlock: %v", err)
	}
}

func TestRecord_LockFilePersists(t *testing.T) {
	repo := initRepo(t)
	if err := Create(repo, CreateInput{Instance: "i1", Intent: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := Append(repo, "i1", "note", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(repo, "i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".plax", "records", "i1.lock")); err != nil {
		t.Errorf("sibling lock file missing after operations: %v", err)
	}
}

func TestRecord_Read_RejectsMissingRequiredHeaders(t *testing.T) {
	repo := initRepo(t)
	path := Path(repo, "i1")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing instance", "intent: task\n---\n## intent\ntask\n", "instance"},
		{"missing intent", "instance: i1\n---\n## intent\ntask\n", "intent"},
		{"unknown header", "instance: i1\nintnet: typo\n---\n## intent\ntask\n", "intnet"},
		{"missing separator", "instance: i1\nintent: task\n", "separator"},
		{"missing intent section", "instance: i1\nintent: task\n---\n", "## intent"},
		{"parent without base", "instance: i1\nintent: task\nparent: i0\n---\n## intent\ntask\n", "together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Read(repo, "i1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Read = %v, want error naming %q", err, tc.want)
			}
		})
	}

	// A record for i1 must not be readable as i2.
	if err := os.WriteFile(path, []byte("instance: i1\nintent: task\n---\n## intent\ntask\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(repo, "i2"); err == nil {
		t.Error("Read(i2) should reject a record whose instance is i1")
	}
}

func TestRecord_Create_RejectsParentWithoutBaseCommit(t *testing.T) {
	repo := initRepo(t)
	if err := Create(repo, CreateInput{Instance: "i1", Parent: "i0", Intent: "task"}); err == nil {
		t.Error("Create with parent but no base_commit should fail")
	}
	if err := Create(repo, CreateInput{Instance: "i1", BaseCommit: strings.Repeat("0", 40), Intent: "task"}); err == nil {
		t.Error("Create with base_commit but no parent should fail")
	}
	if err := Create(repo, CreateInput{Instance: "i1", Parent: "i0", BaseCommit: "abc", Intent: "task"}); err == nil {
		t.Error("Create with an abbreviated base_commit should fail")
	}
	if _, err := os.Stat(filepath.Join(repo, ".plax", "records")); !os.IsNotExist(err) {
		t.Error("validation failures must not create the records directory")
	}
}

// TestRecord_WireFormat_LocksTheOnDiskGrammar pins the exact rendered text
// against the documented wire representation.
func TestRecord_WireFormat_LocksTheOnDiskGrammar(t *testing.T) {
	repo := initRepo(t)
	if err := Create(repo, CreateInput{
		Instance:   "i1",
		Parent:     "i0",
		BaseCommit: "0123456789abcdef0123456789abcdef01234567",
		Intent:     "add retry coverage\nThe child task was assigned from i0's billing work.",
		Contract:   []string{"tests", "typecheck"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := Append(repo, "i1", "Found the retry path; adding a regression test.", time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := WriteVerdict(repo, "i1", Verdict{Status: "pass", Contract: "pass", Summary: "Tests and typecheck pass."}, time.Date(2026, 8, 29, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	got, err := ReadText(repo, "i1")
	if err != nil {
		t.Fatal(err)
	}
	want := `instance: i1
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
`
	if got != want {
		t.Errorf("rendered record differs from the wire format:\n%s", cmp.Diff(want, got))
	}
}
