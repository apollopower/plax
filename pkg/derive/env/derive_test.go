package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnv_DeriveBasicSubstitution(t *testing.T) {
	holes := map[string]string{
		"DATABASE_URL": "postgres://localhost:5432/{{DB_NAME}}",
		"REDIS_PORT":   "{{REDIS_PORT}}",
	}
	values := map[string]string{
		"DB_NAME":    "plax_i1",
		"REDIS_PORT": "6380",
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", "", holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "DATABASE_URL=postgres://localhost:5432/plax_i1") {
		t.Error("DATABASE_URL not substituted")
	}
	if !strings.Contains(content, "REDIS_PORT=6380") {
		t.Error("REDIS_PORT not substituted")
	}
}

func TestEnv_DeriveNonHoleLinesPreserved(t *testing.T) {
	holes := map[string]string{
		"PORT": "{{PORT}}",
	}
	values := map[string]string{"PORT": "3001"}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", "", holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "# Application") {
		t.Error("comment not preserved")
	}
	if !strings.Contains(content, "NEXTAUTH_SECRET=supersecret") {
		t.Error("non-hole var not preserved")
	}
	if !strings.Contains(content, "PORT=3001") {
		t.Error("PORT not substituted")
	}
}

func TestEnv_DeriveMissingHoleInTemplate(t *testing.T) {
	holes := map[string]string{
		"PORT":          "{{PORT}}",
		"GOTENBERG_URL": "http://localhost:{{GOTENBERG_PORT}}",
	}
	values := map[string]string{
		"PORT":           "3001",
		"GOTENBERG_PORT": "3031",
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/sparse.env.example", "", holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "PORT=3001") {
		t.Error("PORT not substituted")
	}
	if !strings.Contains(content, "GOTENBERG_URL=http://localhost:3031") {
		t.Error("GOTENBERG_URL should be appended")
	}
}

func TestEnv_DeriveDBNameSubstitution(t *testing.T) {
	holes := map[string]string{
		"DATABASE_URL": "postgres://localhost/{{DB_NAME}}",
	}
	values := map[string]string{"DB_NAME": "plax_test1"}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", "", holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "plax_test1") {
		t.Error("DB_NAME not substituted")
	}
}

func TestEnv_DeriveMultipleHolesInOneValue(t *testing.T) {
	holes := map[string]string{
		"DATABASE_URL": "postgres://localhost:{{PG_PORT}}/{{DB_NAME}}",
	}
	values := map[string]string{
		"PG_PORT": "5432",
		"DB_NAME": "plax_i1",
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", "", holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "postgres://localhost:5432/plax_i1") {
		t.Errorf("multiple holes not substituted, got:\n%s", data)
	}
}

func TestEnv_DeriveUnknownVar(t *testing.T) {
	holes := map[string]string{
		"PORT": "{{NONEXISTENT}}",
	}
	values := map[string]string{}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", "", holes, values, out)
	if err == nil {
		t.Fatal("expected error for unknown variable")
	}
	if !strings.Contains(err.Error(), "unknown variable") {
		t.Errorf("error should mention unknown variable: %v", err)
	}
}

func TestEnv_DeriveOverridesFromUserEnv(t *testing.T) {
	// Write a user .env with real secrets.
	userEnv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(userEnv, []byte("NEXTAUTH_SECRET=real-secret-from-user\nOPENAI_KEY=sk-real\n"), 0644); err != nil {
		t.Fatal(err)
	}

	holes := map[string]string{
		"PORT": "{{PORT}}",
	}
	values := map[string]string{"PORT": "3001"}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", userEnv, holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	// Hole gets the per-instance value.
	if !strings.Contains(content, "PORT=3001") {
		t.Error("PORT not substituted")
	}
	// User's .env value overrides the template's placeholder.
	if !strings.Contains(content, "NEXTAUTH_SECRET=real-secret-from-user") {
		t.Error("user .env should override template for non-hole keys")
	}
	// Template-only keys are still preserved.
	if !strings.Contains(content, "REDIS_PORT=6379") {
		t.Error("template-only keys should be preserved")
	}
	// User-only keys (absent from template) are appended.
	if !strings.Contains(content, "OPENAI_KEY=sk-real") {
		t.Error("user-only key OPENAI_KEY should be appended")
	}
}

func TestEnv_DeriveOverridesIgnoredForHoles(t *testing.T) {
	// Even if the user's .env has a value for a hole key, the hole wins.
	userEnv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(userEnv, []byte("PORT=9999\n"), 0644); err != nil {
		t.Fatal(err)
	}

	holes := map[string]string{
		"PORT": "{{PORT}}",
	}
	values := map[string]string{"PORT": "3001"}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", userEnv, holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "PORT=3001") {
		t.Error("hole value should take precedence over user .env")
	}
}

func TestEnv_DeriveNoOverridesFile(t *testing.T) {
	// Missing overrides file is fine — template is the fallback.
	holes := map[string]string{
		"PORT": "{{PORT}}",
	}
	values := map[string]string{"PORT": "3001"}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive("testdata/basic.env.example", "/nonexistent/.env", holes, values, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "PORT=3001") {
		t.Error("PORT not substituted")
	}
	if !strings.Contains(string(data), "NEXTAUTH_SECRET=supersecret") {
		t.Error("template value should be used when no overrides exist")
	}
}

func TestEnv_ParseFileBasic(t *testing.T) {
	m, err := ParseFile("testdata/basic.env.example")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if m["PORT"] != "3000" {
		t.Errorf("PORT = %q, want %q", m["PORT"], "3000")
	}
	if m["NEXTAUTH_SECRET"] != "supersecret" {
		t.Errorf("NEXTAUTH_SECRET = %q, want %q", m["NEXTAUTH_SECRET"], "supersecret")
	}
}

func TestEnv_ParseFileComments(t *testing.T) {
	m, err := ParseFile("testdata/comments.env.example")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := m["DATABASE_URL"]; ok {
		t.Error("commented-out line should not be parsed")
	}
	if m["PORT"] != "3000" {
		t.Errorf("PORT = %q, want %q", m["PORT"], "3000")
	}
	if m["INLINE_COMMENT"] != "somevalue" {
		t.Errorf("INLINE_COMMENT = %q, want %q", m["INLINE_COMMENT"], "somevalue")
	}
	if m["NO_STRIP"] != "val#no-strip" {
		t.Errorf("NO_STRIP = %q, want %q (no whitespace before #)", m["NO_STRIP"], "val#no-strip")
	}
	if m["QUOTED_HASH"] != "value # not a comment" {
		t.Errorf("QUOTED_HASH = %q, want %q (quoted # preserved)", m["QUOTED_HASH"], "value # not a comment")
	}
}

func TestEnv_ParseFileQuotedValues(t *testing.T) {
	m, err := ParseFile("testdata/comments.env.example")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if m["QUOTED_VAR"] != "hello world" {
		t.Errorf("QUOTED_VAR = %q, want %q", m["QUOTED_VAR"], "hello world")
	}
}

func TestEnv_ParseFileEmptyValue(t *testing.T) {
	m, err := ParseFile("testdata/comments.env.example")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if v, ok := m["EMPTY_VAR"]; !ok || v != "" {
		t.Errorf("EMPTY_VAR = %q (present=%v), want empty string", v, ok)
	}
}

func TestEnv_ParseFileExportPrefix(t *testing.T) {
	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte("export EXPORTED=val\nPLAIN=x\n"), 0600); err != nil {
		t.Fatal(err)
	}

	m, err := ParseFile(f)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if m["EXPORTED"] != "val" {
		t.Errorf("EXPORTED = %q, want %q (export prefix normalized)", m["EXPORTED"], "val")
	}
	if _, ok := m["export EXPORTED"]; ok {
		t.Error("export prefix should not be part of the key")
	}
}

func TestEnv_ParseFileQuotedWithTrailingComment(t *testing.T) {
	f := filepath.Join(t.TempDir(), ".env")
	content := `QUOTED_COMMENT="abc" # note
QUOTED_HASH_COMMENT="abc # literal" # note
SINGLE='single quoted' # note
`
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	m, err := ParseFile(f)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if m["QUOTED_COMMENT"] != "abc" {
		t.Errorf("QUOTED_COMMENT = %q, want %q", m["QUOTED_COMMENT"], "abc")
	}
	if m["QUOTED_HASH_COMMENT"] != "abc # literal" {
		t.Errorf("QUOTED_HASH_COMMENT = %q, want %q", m["QUOTED_HASH_COMMENT"], "abc # literal")
	}
	if m["SINGLE"] != "single quoted" {
		t.Errorf("SINGLE = %q, want %q", m["SINGLE"], "single quoted")
	}
}

func TestEnv_DeriveExportPrefixedHole(t *testing.T) {
	tmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(tmpl, []byte("export PORT=3000\n"), 0600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := Derive(tmpl, "", map[string]string{"PORT": "{{PORT}}"}, map[string]string{"PORT": "3001"}, out)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)
	if !strings.Contains(content, "PORT=3001") {
		t.Errorf("exported hole not substituted, got:\n%s", content)
	}
	if strings.Contains(content, "export PORT") {
		t.Errorf("stale export line should be replaced, got:\n%s", content)
	}
}

func TestEnv_DeriveExportPrefixedOverride(t *testing.T) {
	tmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(tmpl, []byte("export API_KEY=placeholder\n"), 0600); err != nil {
		t.Fatal(err)
	}
	userEnv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(userEnv, []byte("API_KEY=real-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	if err := Derive(tmpl, userEnv, nil, nil, out); err != nil {
		t.Fatalf("Derive: %v", err)
	}

	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "API_KEY=real-secret") {
		t.Errorf("override should match export-prefixed template key, got:\n%s", data)
	}
}

func TestEnv_DeriveOverridePreservesQuoting(t *testing.T) {
	tmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(tmpl, []byte("TOKEN=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	userEnv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(userEnv, []byte(`TOKEN="abc # def"`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	if err := Derive(tmpl, userEnv, nil, nil, out); err != nil {
		t.Fatalf("Derive: %v", err)
	}

	// The derived file must re-parse to the original secret.
	m, err := ParseFile(out)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if m["TOKEN"] != "abc # def" {
		t.Errorf("TOKEN round-trip = %q, want %q", m["TOKEN"], "abc # def")
	}
}

func TestEnv_RenderBasic(t *testing.T) {
	got, err := Render("http://localhost:{{PORT}}", map[string]string{"PORT": "3001"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "http://localhost:3001" {
		t.Errorf("got %q, want %q", got, "http://localhost:3001")
	}
}

func TestEnv_RenderUnknownVar(t *testing.T) {
	_, err := Render("{{FOO}}", map[string]string{"BAR": "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "FOO") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestEnv_RenderNoPlaceholders(t *testing.T) {
	got, err := Render("no holes here", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "no holes here" {
		t.Errorf("got %q", got)
	}
}

func TestEnv_DeriveMerged_UserKeysAbsentFromTemplate_Appended(t *testing.T) {
	overrides := map[string]string{
		"NEXTAUTH_SECRET": "real-secret",
		"OPENAI_KEY":      "sk-real",
	}
	holes := map[string]string{
		"PORT": "{{PORT}}",
	}
	values := map[string]string{"PORT": "3001"}

	out := filepath.Join(t.TempDir(), ".env")
	fakeTmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(fakeTmpl, []byte("PORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := DeriveMerged(fakeTmpl, overrides, holes, values, out)
	if err != nil {
		t.Fatalf("DeriveMerged: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "PORT=3001") {
		t.Error("PORT hole not rendered")
	}
	if !strings.Contains(content, "NEXTAUTH_SECRET=real-secret") {
		t.Error("user-only key NEXTAUTH_SECRET not appended")
	}
	if !strings.Contains(content, "OPENAI_KEY=sk-real") {
		t.Error("user-only key OPENAI_KEY not appended")
	}
}

func TestEnv_DeriveMerged_CommentedOutKeysInTemplate_UserValAppended(t *testing.T) {
	overrides := map[string]string{
		"NEXTAUTH_SECRET": "real-secret",
	}

	fakeTmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(fakeTmpl, []byte("#NEXTAUTH_SECRET=placeholder\nPORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := DeriveMerged(fakeTmpl, overrides, nil, nil, out)
	if err != nil {
		t.Fatalf("DeriveMerged: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "#NEXTAUTH_SECRET=placeholder") {
		t.Error("commented-out template line should be preserved")
	}
	if !strings.Contains(content, "NEXTAUTH_SECRET=real-secret") {
		t.Error("user value for commented-out key should be appended")
	}
}

func TestEnv_DeriveMerged_UserKeyDoesNotShadowHole(t *testing.T) {
	overrides := map[string]string{
		"PORT":            "9999",
		"NEXTAUTH_SECRET": "real-secret",
	}
	holes := map[string]string{
		"PORT": "{{PORT}}",
	}
	values := map[string]string{"PORT": "3001"}

	fakeTmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(fakeTmpl, []byte("PORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := DeriveMerged(fakeTmpl, overrides, holes, values, out)
	if err != nil {
		t.Fatalf("DeriveMerged: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "PORT=3001") {
		t.Error("hole value should take precedence, got PORT not substituted")
	}
	if strings.Contains(content, "PORT=9999") {
		t.Error("user override should NOT replace the hole value")
	}
	if !strings.Contains(content, "NEXTAUTH_SECRET=real-secret") {
		t.Error("user-only key should still be appended")
	}
}

func TestEnv_DeriveMerged_DeterministicOrder(t *testing.T) {
	overrides := map[string]string{
		"Z_KEY": "z",
		"A_KEY": "a",
		"M_KEY": "m",
	}

	fakeTmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(fakeTmpl, []byte("BASE=1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := DeriveMerged(fakeTmpl, overrides, nil, nil, out)
	if err != nil {
		t.Fatalf("DeriveMerged: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 lines, got %d", len(lines))
	}

	// BASE=1 should be first (from template), then user keys in alpha order.
	if lines[0] != "BASE=1" {
		t.Errorf("first line should be BASE=1, got %q", lines[0])
	}
	if lines[1] != "A_KEY=a" {
		t.Errorf("second line should be A_KEY=a, got %q", lines[1])
	}
	if lines[2] != "M_KEY=m" {
		t.Errorf("third line should be M_KEY=m, got %q", lines[2])
	}
	if lines[3] != "Z_KEY=z" {
		t.Errorf("fourth line should be Z_KEY=z, got %q", lines[3])
	}
}

func TestEnv_DeriveMerged_EmptyOverrides(t *testing.T) {
	fakeTmpl := filepath.Join(t.TempDir(), ".env.example")
	if err := os.WriteFile(fakeTmpl, []byte("PORT=3000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), ".env")
	err := DeriveMerged(fakeTmpl, map[string]string{}, nil, nil, out)
	if err != nil {
		t.Fatalf("DeriveMerged: %v", err)
	}

	data, _ := os.ReadFile(out)
	content := string(data)

	if !strings.Contains(content, "PORT=3000") {
		t.Error("template line should be preserved")
	}
	if strings.Count(content, "\n") != 1 {
		t.Errorf("expected 1 newline (trailing newline only), got %d newlines", strings.Count(content, "\n"))
	}
}
