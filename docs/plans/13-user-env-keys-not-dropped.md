# Plan 13 — User `.env` keys absent from template are no longer dropped

## Objective

Ensure every key in the user's `.env` file reaches every instance's derived
`.env`, even when that key is absent from the template — whether commented out
or entirely missing. Pair with a `doctor` check that warns about such keys
before an instance is built.

---

## Package layout

```
pkg/derive/env/
  derive.go            # DeriveMerged: after hole loop, append leftover user keys
  derive_test.go       # New tests: user-only keys preserved, commented-out keys preserved
pkg/doctor/
  doctor.go            # New check: user-env-keys-missing-from-template
  doctor_test.go       # Tests for the new check
pkg/instance/
  up.go                # Optional: thread user .env path into doctor dep if needed
docs/plans/
  index.md             # Add plan 13 to phase table
```

---

## Type specifications

No new types. The fix is entirely algorithmic.

### `pkg/derive/env/derive.go` — `DeriveMerged`

No struct changes. The function signature stays the same:

```go
func DeriveMerged(templatePath string, overrides map[string]string, holes map[string]string, values map[string]string, outputPath string) error
```

### `pkg/doctor/doctor.go` — no new types

The `Deps` struct gains no new fields — the blueprint's `Env.Template` path
and the user's `.env` path can be read inside the check function.

---

## Algorithms

### `DeriveMerged` — append user-only keys

After the existing hole-append loop (lines 60-69), insert:

```go
// Append user .env keys that were not emitted by the template scan
// or the hole loop. Sort for deterministic output.
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
```

`found` is currently only populated for hole keys (line 49). It must also
be set when a user override matches a template line (line 51):

```go
} else if userVal, ok := merged[key]; ok {
    lines = append(lines, key+"="+userVal)
    found[key] = true   // NEW: mark as emitted so the post-loop doesn't duplicate
}
```

`found` must be initialized with the right capacity. Currently:

```go
found := make(map[string]bool, len(holes))
```

Change to:

```go
found := make(map[string]bool, len(holes)+len(merged))
```

⚠ The `isSkippable` check on line 37 skips comment lines and blank lines
verbatim. A commented-out `#SOME_KEY=value` in the template is copied
verbatim, and the user's `SOME_KEY=real-value` would NOT be caught by the
template-scan loop (the key is not extracted from comment lines). With this
fix, `SOME_KEY` is in `merged` and NOT in `found`, so it is correctly
appended by the new post-loop.

⚠ Deterministic ordering: `sort.Strings(unwritten)` ensures that runs of
`rederive` produce the same file content.

### `doctor` — warn about user `.env` keys missing from template

Add a new check function `runUserEnvMissingFromTemplate`:

1. Read the user's `.env` from `filepath.Join(deps.RepoRoot, ".env")`.
   If it doesn't exist or is unreadable, skip the check with a pass.
2. Read the template file from `filepath.Join(deps.RepoRoot, deps.Blueprint.Env.Template)`.
   Extract live (non-skippable) keys into a set.
3. For every key in the user's `.env` that is NOT in the template's live keys,
   and NOT in the blueprint's `Env.Holes` (holes get rendered regardless),
   emit a warn-level check:
   ```
   "user .env key %q is not in the template — it will appear in derived .env
    files but may be surprising"
   ```
4. If no such keys exist, emit a pass-level check.

The check area is `"user-env-vs-template"`.

⚠ The check does not fail — it warns. The key will still appear in derived
   `.env` files after the algorithmic fix. The warning is informational:
   "you have a key in `.env` that isn't in `.env.example` — make sure this
   is intentional."

⚠ Holes are excluded because they are rendered from blueprint values and
   don't need to be in the template (they are appended by the hole loop).

---

## CLI specification

No new CLI commands or flags.

| Command | Change |
|---|---|
| `plax doctor` | New check `user-env-vs-template`: warns about user `.env` keys absent from the template |
| `plax up <name>` | No change in syntax; instance `.env` now includes user-only keys |
| `plax rederive` | No change in syntax; previously-dropped keys now appear in diffs |

Commands with no change: `ls`, `attach`, `exec`, `suspend`, `resume`, `status`,
`send`, `recv`, `down`, `base *`.

---

## Error handling

| Failure mode | Detection | Behavior |
|---|---|---|
| User `.env` unreadable (not exist) | `os.ReadFile` / `os.Stat` | Template-only keys continue as before. The new post-loop has nothing to append. Doctor skips the check. |
| User `.env` malformed (parse error) | `parseFileRaw` returns error | Today's behaviour: empty map. The new post-loop has nothing to append. Doctor skips the check. |
| Template unreadable | `os.Open` in `DeriveMerged` | Same error as today: `env: open template: ...`. Ref. In doctor, the check skips. |
| User-only key shadows a hole key | Hole loop runs first, sets `found[key]=true` | Correct: hole wins. The post-loop skips it because `found[key]` is true. |
| User-only key conflicts with another user-only key | Map merge: later value wins (same as today) | Correct: deterministic by map iteration — but `derived` ordering is stable via `sort.Strings`. |
| Deterministic output after rederive | `sort.Strings(unwritten)` | Two identical runs produce identical output. |
| User removes a key from `.env` that was previously user-only | Post-loop doesn't see it in `merged` | Correct: key disappears from rederived output (shown in diffs). |

---

## Tests

### Unit tests — `pkg/derive/env/derive_test.go`

- `TestEnv_DeriveMerged_UserKeysAbsentFromTemplate_Appended` —
  Create a template with `PORT=3000` only. Call `DeriveMerged` with
  `overrides` containing `NEXTAUTH_SECRET=real-secret`. Verify the output
  contains both `PORT=3000` and `NEXTAUTH_SECRET=real-secret`.

- `TestEnv_DeriveMerged_CommentedOutKeysInTemplate_UserValAppended` —
  Template has `#NEXTAUTH_SECRET=placeholder` (commented out). Overrides
  has `NEXTAUTH_SECRET=real`. Verify output has both the comment line AND
  the user's `NEXTAUTH_SECRET=real`.

- `TestEnv_DeriveMerged_UserKeyDoesNotShadowHole` —
  Template has `PORT=3000`. Blueprint declares `PORT` as a hole. Overrides
  has `PORT=9999` and `NEXTAUTH_SECRET=real-secret`. Verify output has
  `PORT={{PORT}}` (rendered hole value) and `NEXTAUTH_SECRET=real-secret`.

- `TestEnv_DeriveMerged_DeterministicOrder` —
  Template is minimal. Overrides has `Z_KEY=z`, `A_KEY=a`, `M_KEY=m`.
  Verify output lines for these keys appear in alphabetical order.

- `TestEnv_DeriveMerged_EmptyOverrides` —
  Same as today: no overrides map → no extra keys appended.

### Unit tests — `pkg/derive/env/derive_test.go` (existing, updated)

- `TestEnv_DeriveOverridesFromUserEnv` — currently expects `NEXTAUTH_SECRET`
  from user `.env` to override the template line. After the fix, it should
  also verify that user-only keys (like `OPENAI_KEY`) appear in output.
  Extend the assertions.

### Unit tests — `pkg/doctor/doctor_test.go`

- `TestDoctor_UserEnvKeysMissingFromTemplate_Warns` —
  Write a `.env` file with keys not in `.env.example`. Verify a
  warn-level check with area `user-env-vs-template`.

- `TestDoctor_UserEnvKeysAllInTemplate_Passes` —
  Write a `.env` file whose keys are a subset of template live keys.
  Verify a pass-level check.

- `TestDoctor_UserEnvNoFile_Skips` —
  Remove `.env`. Verify pass-level check (or skipped).

- `TestDoctor_UserEnvHoleKeysExcluded` —
  Write a `.env` with a key that is a hole in the blueprint. Verify it
  is NOT warned about (holes are expected to be absent from the template).

---

## Acceptance criteria

- [ ] A template with `PORT=3000` and a user `.env` with `NEXTAUTH_SECRET=real-secret` produces a derived `.env` with both keys
- [ ] A template with `#NEXTAUTH_SECRET=placeholder` (commented out) and a user `.env` with `NEXTAUTH_SECRET=real` produces a derived `.env` with both the comment and `NEXTAUTH_SECRET=real`
- [ ] Hole keys from the blueprint still take precedence over user `.env` values
- [ ] User-only keys are appended in sorted order (deterministic rederive diffs)
- [ ] `plax doctor` warns (warn level, not fail) when user `.env` has keys absent from the template
- [ ] `plax doctor` passes when user `.env` keys are all in the template
- [ ] `plax doctor` handles missing `.env` gracefully (pass, not error)
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (both with and without `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

---

## Dependencies

| Package | Import path | Version | Purpose |
|---|---|---|---|
| No new external dependencies. | | | |

Standard library additions:

- `sort` — `sort.Strings` for deterministic output ordering (already imported
  in `pkg/derive/env/derive.go` via `strings`, but `sort` is a new import).

---

## Deferred items

| Item | Deferred to | Reason |
|---|---|---|
| Doctor check suggesting the user add the key to `.env.example` | Future UX polish | The warning message is informational; suggesting edits to `.env.example` is a separate concern. |
| Doctor check comparing derived `.env` holes against user `.env` for stale holes | When `rederive` gains a `--check` mode | Holes changing in the blueprint is already handled by `rederive`. A doctor-level warning for stale hole values would be additive. |
