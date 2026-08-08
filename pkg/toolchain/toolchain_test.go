package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePins_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tool-versions")
	if err := os.WriteFile(path, []byte("nodejs 22.19.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pins, err := ParsePins(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := pins["nodejs"]; got != "22.19.0" {
		t.Errorf("nodejs = %q, want 22.19.0", got)
	}
}

func TestParsePins_CommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tool-versions")
	content := "# comment\n\nnodejs 22.19.0\n# another\n\ngolang 1.26\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	pins, err := ParsePins(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 2 {
		t.Fatalf("got %d pins, want 2", len(pins))
	}
	if pins["nodejs"] != "22.19.0" {
		t.Errorf("nodejs = %q", pins["nodejs"])
	}
	if pins["golang"] != "1.26" {
		t.Errorf("golang = %q", pins["golang"])
	}
}

func TestParsePins_MultiVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tool-versions")
	if err := os.WriteFile(path, []byte("python 3.12.1 3.11.7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pins, err := ParsePins(path)
	if err != nil {
		t.Fatal(err)
	}
	if pins["python"] != "3.12.1" {
		t.Errorf("python = %q, want 3.12.1", pins["python"])
	}
}

func TestParsePins_Missing(t *testing.T) {
	pins, err := ParsePins(filepath.Join(t.TempDir(), ".tool-versions"))
	if err != nil {
		t.Fatal(err)
	}
	if pins != nil {
		t.Errorf("expected nil map for missing file, got %v", pins)
	}
}

func TestCompareVersions(t *testing.T) {
	recorded := map[string]string{"nodejs": "v22.19.0", "bun": "1.3.11"}
	current := map[string]string{"nodejs": "v22.20.1", "golang": "1.26.5"}
	diffs := CompareVersions(recorded, current)
	if len(diffs) != 3 {
		t.Fatalf("got %d diffs, want 3", len(diffs))
	}
	for _, d := range diffs {
		switch d.Tool {
		case "nodejs":
			if d.Recorded != "v22.19.0" || d.Current != "v22.20.1" {
				t.Errorf("nodejs diff wrong: %+v", d)
			}
		case "bun":
			if d.Recorded != "1.3.11" || d.Current != "" {
				t.Errorf("bun diff wrong: %+v", d)
			}
		case "golang":
			if d.Recorded != "" || d.Current != "1.26.5" {
				t.Errorf("golang diff wrong: %+v", d)
			}
		}
	}
}

func TestMatchesPin(t *testing.T) {
	tests := []struct {
		pin, resolved string
		want          bool
	}{
		{"22.19.0", "v22.19.0", true},
		{"1.26", "go version go1.26.5 linux/amd64", true},
		{"1.26", "go1.26.0", true},
		{"1.2", "11.2.3", false},
		{"lts", "v20.0.0", false},
		{"latest", "v20.0.0", false},
		{"22.19.0", "v22.20.0", false},
	}
	for _, tt := range tests {
		got := MatchesPin(tt.pin, tt.resolved)
		if got != tt.want {
			t.Errorf("MatchesPin(%q, %q) = %v, want %v", tt.pin, tt.resolved, got, tt.want)
		}
	}
}
