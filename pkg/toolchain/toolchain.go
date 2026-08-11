// Package toolchain detects and validates available language toolchains.
package toolchain

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var toolBinary = map[string]struct {
	binary string
	flag   string
}{
	"nodejs": {"node", "--version"},
	"golang": {"go", "version"},
	"python": {"python3", "--version"},
}

func binaryInfo(name string) (binary, flag string) {
	if info, ok := toolBinary[name]; ok {
		return info.binary, info.flag
	}
	return name, "--version"
}

// ParsePins reads a .tool-versions file and returns tool→version mappings.
// Returns (nil, nil) if the file does not exist.
func ParsePins(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	pins := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if _, exists := pins[parts[0]]; !exists {
			pins[parts[0]] = parts[1]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(pins) == 0 {
		return nil, nil
	}
	return pins, nil
}

// ResolveVersions probes each tool's binary and returns tool→resolved-version.
// Tools whose binaries are missing are silently omitted.
func ResolveVersions(pins map[string]string) map[string]string {
	resolved := make(map[string]string)
	for name := range pins {
		binary, flag := binaryInfo(name)
		ver := tryFlag(binary, flag)
		if ver == "" && flag == "--version" {
			ver = tryFlag(binary, "version")
		}
		if ver != "" {
			resolved[name] = ver
		}
	}
	return resolved
}

func tryFlag(binary, flag string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, flag)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(firstLine(string(out)))
}

// PinMatch is the result of comparing a resolved version against a pin.
type PinMatch int

const (
	PinMatchYes PinMatch = iota
	PinMatchNo
	PinMatchUnverifiable
)

func (m PinMatch) String() string {
	switch m {
	case PinMatchYes:
		return "yes"
	case PinMatchNo:
		return "no"
	case PinMatchUnverifiable:
		return "unverifiable"
	default:
		return "unknown"
	}
}

// Diff describes a tool version difference between a recorded baseline and
// the current machine state. Recorded or Current may be empty for added/removed.
type Diff struct {
	Tool     string `json:"tool"`
	Recorded string `json:"recorded"`
	Current  string `json:"current"`
}

// CompareVersions returns the names of tools whose resolved versions differ
// from the baseline, or that were added/removed. Sorted for determinism.
func CompareVersions(recorded, current map[string]string) []Diff {
	var diffs []Diff
	seen := map[string]bool{}
	for tool, rec := range recorded {
		cur := current[tool]
		if rec != cur {
			diffs = append(diffs, Diff{Tool: tool, Recorded: rec, Current: cur})
		}
		seen[tool] = true
	}
	for tool, cur := range current {
		if seen[tool] {
			continue
		}
		diffs = append(diffs, Diff{Tool: tool, Current: cur})
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Tool < diffs[j].Tool })
	return diffs
}

// MatchesPin reports whether a resolved version satisfies a pin.
// Non-semver pins (lts, latest) return PinMatchUnverifiable.
func MatchesPin(pin, resolved string) PinMatch {
	lower := strings.ToLower(pin)
	if lower == "lts" || lower == "latest" {
		return PinMatchUnverifiable
	}

	pin = strings.TrimPrefix(pin, "v")
	pin = strings.TrimPrefix(pin, "go")

	tokens := strings.Fields(resolved)
	for _, tok := range tokens {
		tok = strings.TrimPrefix(tok, "v")
		tok = strings.TrimPrefix(tok, "go")
		if tok == pin {
			return PinMatchYes
		}
		if strings.HasPrefix(tok, pin+".") {
			return PinMatchYes
		}
	}
	return PinMatchNo
}

func firstLine(s string) string {
	idx := strings.IndexByte(s, '\n')
	if idx < 0 {
		return s
	}
	return s[:idx]
}
