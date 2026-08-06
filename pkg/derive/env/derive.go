// Package env derives per-instance .env files from a template by
// substituting hole variables with allocated values.
package env

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var holeRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// Derive reads the env template file, substitutes holes with rendered
// values, and writes the result to outputPath.
//
// templatePath: absolute path to the env template (e.g. .env.example).
// overridesPath: absolute path to the user's own .env file (may not exist).
//
//	Non-hole values from this file take precedence over the template.
//
// holes: KEY → template string with {{VAR}} placeholders (from blueprint).
// values: VAR → resolved value (e.g. "DB_NAME" → "plax_i1", "REDIS_PORT" → "6380").
// outputPath: absolute path where the derived .env is written.
//
// Precedence for each key:
//  1. Hole keys → rendered template (per-instance values)
//  2. Keys in overrides file → user's value (secrets, machine-specific config)
//  3. Template lines → copied verbatim (defaults, comments)
//
// Hole keys absent from the template are appended.
func Derive(templatePath string, overridesPath string, holes map[string]string, values map[string]string, outputPath string) error {
	// Load the user's overrides (e.g. the main checkout's .env with real secrets).
	// Not an error if absent — the template is the fallback.
	overrides := map[string]string{}
	if overridesPath != "" {
		var err error
		overrides, err = ParseFile(overridesPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("env: load overrides: %w", err)
		}
	}

	f, err := os.Open(templatePath)
	if err != nil {
		return fmt.Errorf("env: open template: %w", err)
	}
	defer func() { _ = f.Close() }()

	found := make(map[string]bool, len(holes))
	var lines []string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		if isSkippable(line) {
			lines = append(lines, line)
			continue
		}

		key := extractKey(line)
		if tmpl, ok := holes[key]; ok {
			rendered, err := Render(tmpl, values)
			if err != nil {
				return fmt.Errorf("env: hole %q: %w", key, err)
			}
			lines = append(lines, key+"="+rendered)
			found[key] = true
		} else if userVal, ok := overrides[key]; ok {
			lines = append(lines, key+"="+userVal)
		} else {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("env: read template: %w", err)
	}

	// Append holes that were not present in the template.
	for key, tmpl := range holes {
		if found[key] {
			continue
		}
		rendered, err := Render(tmpl, values)
		if err != nil {
			return fmt.Errorf("env: hole %q: %w", key, err)
		}
		lines = append(lines, key+"="+rendered)
	}

	out := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(outputPath, []byte(out), 0600); err != nil {
		return fmt.Errorf("env: write output: %w", err)
	}

	return nil
}

// ParseFile reads a .env file and returns key-value pairs.
// Skips blank lines and comments (#). Strips surrounding quotes from values.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("env: open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	m := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if isSkippable(line) {
			continue
		}
		key := extractKey(line)
		val := extractValue(line)
		m[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("env: read file: %w", err)
	}

	return m, nil
}

// Render replaces {{VAR}} placeholders in tmpl with values.
// Returns error if a placeholder references a variable not in values.
func Render(tmpl string, values map[string]string) (string, error) {
	var err error
	result := holeRe.ReplaceAllStringFunc(tmpl, func(match string) string {
		if err != nil {
			return match
		}
		varName := holeRe.FindStringSubmatch(match)[1]
		v, ok := values[varName]
		if !ok {
			err = fmt.Errorf("template references unknown variable {{%s}}", varName)
			return match
		}
		return v
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// isSkippable reports whether a line is blank or a comment.
func isSkippable(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// extractKey returns the part before the first '=' in a KEY=value line.
func extractKey(line string) string {
	k, _, _ := strings.Cut(line, "=")
	return strings.TrimSpace(k)
}

// extractValue returns the part after the first '=' with quotes stripped
// and trailing comments removed. A '#' starts a comment only when preceded
// by whitespace and never inside quotes.
func extractValue(line string) string {
	_, v, _ := strings.Cut(line, "=")
	v = strings.TrimSpace(v)

	// Strip surrounding quotes.
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}

	// Strip trailing comment: '#' preceded by whitespace.
	if idx := strings.Index(v, " #"); idx >= 0 {
		v = strings.TrimSpace(v[:idx])
	}

	return v
}
