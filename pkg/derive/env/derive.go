// Package env derives per-instance .env files from a template by
// substituting hole variables with allocated values.
package env

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var holeRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

func DeriveMerged(templatePath string, overrides map[string]string, holes map[string]string, values map[string]string, outputPath string) error {
	merged := make(map[string]string, len(overrides))
	for k, v := range overrides {
		merged[k] = v
	}

	f, err := os.Open(templatePath)
	if err != nil {
		return fmt.Errorf("env: open template: %w", err)
	}
	defer func() { _ = f.Close() }()

	found := make(map[string]bool, len(holes)+len(merged))
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
		} else if userVal, ok := merged[key]; ok {
			lines = append(lines, key+"="+userVal)
			found[key] = true
		} else {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("env: read template: %w", err)
	}

	for key, tmpl := range holes {
		if found[key] {
			continue
		}
		rendered, err := Render(tmpl, values)
		if err != nil {
			return fmt.Errorf("env: hole %q: %w", key, err)
		}
		lines = append(lines, key+"="+rendered)
		found[key] = true
	}

	unwritten := make([]string, 0, len(merged))
	for key := range merged {
		if !found[key] {
			unwritten = append(unwritten, key)
		}
	}
	sort.Strings(unwritten)
	for _, key := range unwritten {
		lines = append(lines, key+"="+merged[key])
	}

	out := strings.Join(lines, "\n") + "\n"

	dir := filepath.Dir(outputPath)
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return fmt.Errorf("env: create temp: %w", err)
	}
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("env: write temp: %w", err)
	}
	_ = tmp.Close()
	if err := os.Rename(tmp.Name(), outputPath); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("env: rename: %w", err)
	}

	return nil
}

// ParseFileRaw reads a .env file keeping each value's raw text (quotes
// intact). For rederive so secrets round-trip without corruption.
func ParseFileRaw(path string) (map[string]string, error) {
	return parseFileRaw(path)
}

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
	overrides := map[string]string{}
	if overridesPath != "" {
		var err error
		overrides, err = parseFileRaw(overridesPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("env: load overrides: %w", err)
		}
	}
	return DeriveMerged(templatePath, overrides, holes, values, outputPath)
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
// An optional "export " prefix is normalized away so exported assignments
// match the same keys as plain ones.
func extractKey(line string) string {
	k, _, _ := strings.Cut(line, "=")
	k = strings.TrimSpace(k)
	k = strings.TrimPrefix(k, "export ")
	return strings.TrimSpace(k)
}

// extractValue returns the part after the first '=' with quotes stripped
// and trailing comments removed. A '#' starts a comment only when preceded
// by whitespace and never inside quotes.
func extractValue(line string) string {
	return unquote(rawValue(line))
}

// rawValue returns the value text after the first '=' with any trailing
// comment removed but surrounding quotes intact.
func rawValue(line string) string {
	_, v, _ := strings.Cut(line, "=")
	v = strings.TrimSpace(v)
	return stripComment(v)
}

// stripComment removes a trailing comment: '#' preceded by whitespace and
// outside quotes.
func stripComment(v string) string {
	var quote byte
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && i > 0 && (v[i-1] == ' ' || v[i-1] == '\t'):
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// unquote strips one layer of surrounding quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// parseFileRaw reads a .env file like ParseFile but keeps each value's raw
// text (surrounding quotes intact). Used for override files whose values
// will be written back out.
func parseFileRaw(path string) (map[string]string, error) {
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
		m[extractKey(line)] = rawValue(line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("env: read file: %w", err)
	}

	return m, nil
}

// LoadInstanceEnv reads the derived .env from a worktree and merges it
// over the host environment, then layers allocated ports on top. Returns
// a list of KEY=VALUE strings suitable for exec.Cmd.Env.
func LoadInstanceEnv(worktreePath string, ports map[string]int) ([]string, error) {
	envPath := filepath.Join(worktreePath, ".env")
	derived, err := ParseFile(envPath)
	if err != nil {
		return nil, fmt.Errorf("env: .env not found at %s — was the instance created with 'plax up'?", envPath)
	}

	envMap := map[string]string{}
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}
	for k, v := range derived {
		envMap[k] = v
	}
	for k, v := range ports {
		envMap[k] = strconv.Itoa(v)
	}

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result, nil
}
