package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// headingNumberRe strips leading section numbers ("1. ", "3.4 ") so headings
// can be compared across documents regardless of numbering scheme.
var headingNumberRe = regexp.MustCompile(`^\d+(\.\d+)*\.?\s+`)

// normalizedHeadings extracts lowercase heading text from a markdown body.
func normalizedHeadings(md string) map[string]bool {
	set := make(map[string]bool)
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		h := strings.TrimSpace(strings.TrimLeft(t, "#"))
		h = headingNumberRe.ReplaceAllString(h, "")
		set[strings.ToLower(h)] = true
	}
	return set
}

// guideManualShared is the set of sections that must exist in both the
// agent-facing guide and the human manual, so a semantic described in one
// cannot silently disappear from the other.
var guideManualShared = []string{
	"what plax does",
	"the blueprint",
	"isolation strategies",
	"the base",
	"working with instances",
	"verify an instance",
	"mailbox",
	"doctor",
	"output conventions",
	"environment variables",
	"known limitations",
}

func TestGuide_PrintsEmbeddedDoc(t *testing.T) {
	bin := buildPlax(t)

	// Deliberately an empty directory: `plax guide` must work with no
	// plax.json, no registry, nothing — an agent reads it before a repo
	// exists.
	stdout, stderr, err := runPlax(bin, t.TempDir(), "guide")
	if err != nil {
		t.Fatalf("plax guide: %v", err)
	}
	if stdout != guideDoc {
		t.Fatalf("plax guide output differs from the embedded document:\n--- got (first 200) ---\n%s\n--- want (first 200) ---\n%s",
			firstN(stdout, 200), firstN(guideDoc, 200))
	}
	if stderr != "" {
		t.Fatalf("plax guide wrote to stderr: %q", stderr)
	}
}

func TestGuide_HasRequiredSections(t *testing.T) {
	got := normalizedHeadings(guideDoc)
	for _, want := range []string{
		"what plax does",
		"the blueprint",
		"command reference",
		"lifecycle states",
		"working with instances",
		"drift model",
		"verify an instance",
		"mailbox",
		"doctor",
		"output conventions",
		"known limitations",
		"decision rules for agents",
	} {
		if !got[want] {
			t.Errorf("guide.md is missing required section %q", want)
		}
	}

	// The operational semantics the triage scoped must be present verbatim:
	// lifecycle states, drift model, verification checks, send/recv IPC.
	for _, want := range []string{
		"running",
		"suspended",
		"tcp-reachability",
		"env-completeness",
		"process-liveness",
		"db-provenance",
		"plax send",
		"plax recv",
		"plax status",
		"plax verify",
	} {
		if !strings.Contains(guideDoc, want) {
			t.Errorf("guide.md does not mention %q", want)
		}
	}
}

func TestGuide_ManualHeadingSync(t *testing.T) {
	manual, err := os.ReadFile(filepath.Join("..", "..", "docs", "manual.md"))
	if err != nil {
		t.Fatalf("reading docs/manual.md: %v", err)
	}
	guide := normalizedHeadings(guideDoc)
	human := normalizedHeadings(string(manual))

	for _, want := range guideManualShared {
		if !guide[want] {
			t.Errorf("guide.md is missing shared section %q", want)
		}
		if !human[want] {
			t.Errorf("docs/manual.md is missing shared section %q — update the manual or the shared set", want)
		}
	}
}

func TestVersionFlag_PrintsAndExits(t *testing.T) {
	bin := buildPlax(t)
	stdout, stderr, err := runPlax(bin, t.TempDir(), "--version")
	if err != nil {
		t.Fatalf("plax --version: %v", err)
	}
	if !strings.HasPrefix(stdout, "plax ") {
		t.Fatalf("plax --version output does not start with the binary name: %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("plax --version wrote to stderr: %q", stderr)
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
