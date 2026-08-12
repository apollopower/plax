# Plan 14 — Env scrub: block dangerous secrets from reaching instances

## Objective

Add a `scrub` field to the blueprint's `env` block so teams can declare
keys whose real values must never propagate from the developer's `.env`
into any instance's derived `.env`.

---

## Package layout

```
pkg/blueprint/
  blueprint.go          # EnvConfig gains Scrub []string field
  validate.go           # Validation: scrub keys must exist in template (warning)
pkg/derive/env/
  derive.go             # DeriveMerged gains scrub parameter; skips scrubbed overrides
  derive_test.go        # New tests: scrubbed keys use template value, not user value
pkg/doctor/
  doctor.go             # New check: scrubbed key has non-trivial value in user .env
  doctor_test.go        # Tests for the new check
pkg/instance/
  up.go                 # Pass scrub into env.Derive call
  rederive.go           # Pass scrub into env.DeriveMerged call
docs/plans/
  index.md              # Add plan 14 to phase table
```

---

## Type specifications

### `pkg/blueprint/blueprint.go` — `EnvConfig`

```go
type EnvConfig struct {
    Template string            `json:"template"`
    Holes    map[string]string `json:"holes"`
    Scrub    []string          `json:"scrub,omitempty"`
}
```

`Scrub` — a list of env var keys whose real values must not reach instances.
When a key is present in both `scrub` and the developer's `.env`, derivation
skips the developer's value and uses the template's line verbatim (placeholder
or empty). If a scrubbed key is absent from the template entirely, it is
omitted from the derived `.env`.

Duplicates in the `scrub` list are silently deduplicated at usage time.

### `pkg/derive/env/derive.go` — `DeriveMerged`

Signature change:

```go
func DeriveMerged(templatePath string, overrides map[string]string, holes map[string]string, values map[string]string, scrub map[string]bool, outputPath string) error
```

`scrub` — set of keys whose real values must not be used. When a key is in
`scrub`, the user's override is ignored and the template value (or absence)
wins. Nil/empty means no scrubbing.

### `pkg/derive/env/derive.go` — `Derive`

Signature change:

```go
func Derive(templatePath string, overridesPath string, holes map[string]string, values map[string]string, scrub map[string]bool, outputPath string) error
```

`scrub` is passed through to `DeriveMerged`.

---

## Algorithms

### `DeriveMerged` — skip scrubbed keys in the user override path

Two changes in the template-scan loop and the post-loop:

**Template-scan loop** (around line 51 in `derive.go`):

```
} else if userVal, ok := merged[key]; ok {
```

Change to:

```
} else if userVal, ok := merged[key]; ok && !scrub[key] {
```

When `scrub[key]` is true, the template line is copied verbatim (the `else`
branch at line 54-56 fires). The scrubbed key is NOT marked `found`, so if the
key also has no hole definition, it will not be appended by the post-loop — the
template line itself is the only mention. 

⚠ If a scrubbed key is also a hole, holes take precedence (the hole branch
fires first, line 44-50). Scrubbing is irrelevant for holes — holes are
already rendered from per-instance values, not from the developer's `.env`.

**Post-loop** (around line 74-83 in `derive.go`):

```go
for key := range merged {
    if !found[key] {
        unwritten = append(unwritten, key)
    }
}
```

Change to:

```go
for key := range merged {
    if !found[key] && !scrub[key] {
        unwritten = append(unwritten, key)
    }
}
```

A scrubbed key absent from the template should not appear as a user-only key
in the output.

### Doctor — warn when a scrubbed key has a non-trivial value in user `.env`

New check function `runScrubbedKeysWithRealValues`:

1. If blueprint has no `Env.Scrub` entries, emit a pass-level check immediately.
2. Read the user's `.env` from `filepath.Join(deps.RepoRoot, ".env")`.
   If it doesn't exist or is unreadable, emit a pass (nothing to check against).
3. For each key in `bp.Env.Scrub`:
   - Read the template value for that key (from `filepath.Join(deps.RepoRoot, bp.Env.Template)`).
     Extract the template value using `env.ParseFile` on the template.
   - ⚠ If the key is not in the template, the template value is considered empty.
   - Compare the user's `.env` value against the template value.
   - If the user's value is non-empty AND differs from the template's value,
     emit a warn-level check:
     ```
     "scrubbed key %q has a real value in your .env (%q) that differs from
      the template placeholder. It will NOT reach instances."
     ```
   - If the user's value matches the template value (both empty, or both
     placeholder), emit a pass-level check for this key.

The check area is `"scrubbed-env-keys"`.

⚠ The check does not fail — it warns. The scrubbing mechanism protects
   instances; the warning tells the developer that a credential they may have
   forgotten about is being blocked.

⚠ Holes are excluded from the check: scrubbing is irrelevant for hole keys
   (hole values come from per-instance allocation, not the user's `.env`).

### Validation — warn if a scrubbed key is not in the template

Extend `checkHolesInTemplate` (or add a new check in `ValidateBlueprint`):

For each key in `bp.Env.Scrub` that is NOT a hole (scrubbing is irrelevant
for holes), check if the key appears in the template file (using the same
`lineHasKey` helper). If it does not, emit a warning:

```
"blueprint: scrubbed key %q not found in template file %q (warning)
 — the key will be absent from every instance's .env"
```

This is a warning, not a fatal error — omitting a dangerous key is the safe
behavior.

---

## CLI specification

No new CLI commands or flags.

| Command | Change |
|---|---|
| `plax up <name>` | `.env` derivation now scrubs declared keys from user overrides |
| `plax rederive` | Same scrubbing applied during re-derivation |
| `plax doctor` | New check `scrubbed-env-keys`: warns when a scrubbed key has a real (non-template) value in the user's `.env` |

Commands with no change: `ls`, `attach`, `exec`, `suspend`, `resume`, `status`,
`send`, `recv`, `down`, `base *`.

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| Scrubbed key is also a hole | Hole branch in template scan fires first | Correct: hole wins. Scrubbing is irrelevant. |
| Scrubbed key not in template or holes | Post-loop skips it | Correct: key is absent from derived `.env`. |
| Scrubbed key in template, user has value | User override skipped, template line copied verbatim | Correct: instance gets the placeholder. |
| Scrubbed key in template with empty value, user has value | Same — template line preserved | Correct: `SENDGRID_API_KEY=` appears empty. |
| User has a scrubbed key that's not in template at all | Post-loop skips it, no template line | Correct: key is absent from instance. |
| Duplicate scrub entries | Build set at call site (`scrub map[string]bool`) | Deduplication is automatic. |
| Template unreadable (scrub check in doctor) | `os.Open` fails | Check emits a pass — can't compare values without the template. |
| User `.env` unreadable (scrub check in doctor) | `os.ReadFile` fails | Check emits a pass — nothing to warn about. |
| Scrubbed key appears in `Derive` overrides file | Same DeriveMerged scrub logic | User's value skipped, template line wins. |
| Scrub list changes between up and rederive | Rederive uses current blueprint | Scrub additions block keys; scrub removals unblock them. |

---

## Tests

### Unit tests — `pkg/derive/env/derive_test.go`

- `TestEnv_DeriveMerged_ScrubbedKeyUsesTemplateValue` —
  Template has `SENDGRID_API_KEY=placeholder`. Overrides has
  `SENDGRID_API_KEY=real-secret`. Scrub has `SENDGRID_API_KEY`.
  Verify output contains `SENDGRID_API_KEY=placeholder` (template value).

- `TestEnv_DeriveMerged_ScrubbedKeyNotInTemplate_Omitted` —
  Template is minimal (no `SENDGRID_API_KEY`). Overrides has
  `SENDGRID_API_KEY=real-secret`. Scrub has `SENDGRID_API_KEY`.
  Verify output does NOT contain `SENDGRID_API_KEY`.

- `TestEnv_DeriveMerged_ScrubbedKeyHoleWins` —
  Template has `PORT=3000`. Overrides has `PORT=9999`. Holes has
  `PORT: "{{PORT}}"`. Scrub has `PORT` (irrelevant for holes).
  Verify output contains `PORT=3001` (rendered hole value).

- `TestEnv_DeriveMerged_ScrubbedKeyWithEmptyTemplateValue` —
  Template has `SENDGRID_API_KEY=`. Overrides has
  `SENDGRID_API_KEY=real-secret`. Scrub has `SENDGRID_API_KEY`.
  Verify output contains `SENDGRID_API_KEY=` (empty from template).

- `TestEnv_DeriveMerged_ScrubDoesNotAffectNonScrubbedKeys` —
  Template has `NEXTAUTH_SECRET=placeholder`. Overrides has
  `NEXTAUTH_SECRET=real-secret`. Scrub has `SENDGRID_API_KEY` only.
  Verify output contains `NEXTAUTH_SECRET=real-secret` (not scrubbed).

- `TestEnv_DeriveMerged_ScrubbedKeyStillAllowsOtherUserKeys` —
  Template minimal. Overrides has `SENDGRID_API_KEY=real` and
  `OPENAI_KEY=real`. Scrub has `SENDGRID_API_KEY`. Verify output
  has `OPENAI_KEY=real` but NOT `SENDGRID_API_KEY`.

### Unit tests — `pkg/doctor/doctor_test.go`

- `TestDoctor_ScrubbedKeyHasRealValue_Warns` —
  Blueprint with scrub `["SENDGRID_API_KEY"]`. User `.env` has
  `SENDGRID_API_KEY=real`. Template has `SENDGRID_API_KEY=placeholder`.
  Verify warn-level check with area `scrubbed-env-keys`.

- `TestDoctor_ScrubbedKeyMatchesTemplate_Passes` —
  Blueprint with scrub `["SENDGRID_API_KEY"]`. User `.env` has
  `SENDGRID_API_KEY=placeholder`. Template has `SENDGRID_API_KEY=placeholder`.
  Verify pass-level check.

- `TestDoctor_ScrubbedKeyEmptyInBoth_Passes` —
  Blueprint with scrub `["SENDGRID_API_KEY"]`. User `.env` has
  `SENDGRID_API_KEY=`. Template has `SENDGRID_API_KEY=`.
  Verify pass-level check.

- `TestDoctor_ScrubbedKeyNoUserEnv_Skips` —
  Blueprint with scrub `["SENDGRID_API_KEY"]`. No user `.env`.
  Verify pass-level check (or skipped).

- `TestDoctor_ScrubbedKeyHoleExcluded` —
  Blueprint with scrub `["PORT"]` where PORT is also a hole.
  Verify pass-level check (scrubbing is irrelevant for holes).

- `TestDoctor_NoScrubList_Passes` —
  Blueprint with no scrub. Verify pass-level check.

### Unit tests — `pkg/blueprint/validate_test.go`

- `TestBlueprint_ScrubKeyNotInTemplate_Warns` —
  Blueprint with scrub `["NONEXISTENT_KEY"]`, template without it.
  Verify a warning (not a fatal error) is emitted.

### Integration tests

- `TestEnv_Derive_WithScrub_EndToEnd` —
  Write a template, user `.env`, and full blueprint with scrubbed keys.
  Call `Derive` with the scrub list. Verify output matches expected
  (scrubbed keys use template values, non-scrubbed keys use user values).

---

## Acceptance criteria

- [ ] A template with `SENDGRID_API_KEY=placeholder` and a user `.env` with
      `SENDGRID_API_KEY=real-secret` produces a derived `.env` with
      `SENDGRID_API_KEY=placeholder` when `SENDGRID_API_KEY` is in the scrub list
- [ ] A scrubbed key not present in the template is omitted from the derived `.env`
- [ ] Hole keys in the scrub list are ignored (holes take precedence regardless)
- [ ] Non-scrubbed keys continue to propagate from the user's `.env` as before
- [ ] `plax doctor` warns (warn level, not fail) when a scrubbed key has a
      real value in the user's `.env` that differs from the template
- [ ] `plax doctor` passes when scrubbed key values match the template
- [ ] `plax doctor` handles missing `.env` gracefully (pass, not error)
- [ ] Blueprint validation warns when a scrubbed key is not found in the template
- [ ] `plax rederive` applies scrubbing correctly (regression: rederive with
      scrub produces the same output as fresh `plax up`)
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (both with and without
      `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| No new external dependencies. | | | |

Standard library additions: none (all needed packages already imported).
