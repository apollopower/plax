# AGENTS.md

## Project

Plax is a CLI tool that provisions parallel, isolated development environments
from one repo, so multiple coding agents can work without colliding on ports,
databases, or state. It manages the environments; it does not orchestrate agents
and it ships no UI. Think of it as a UNIX tool: it does one thing and composes.

---

## Values

These guide decisions. They are not absolute — context matters and trade-offs
are real — but when in doubt, follow them.

1. **UNIX philosophy.** Plax is a tool, not a platform. It reads input, produces
   output, and composes with other tools (tmux, shell scripts, agent runners).
   It does not own windows, scheduling, or agent lifecycle. If something can be
   done outside Plax, it probably should be.

2. **Simple, modular, composable.** Prefer small packages with clear boundaries.
   A function should do one thing. A package should have one responsibility.
   When two things can vary independently, separate them.

3. **CSP for concurrency.** Go's goroutine+channel model is the right tool for
   managing multiple simultaneous workstreams in a single process. When Plax
   needs to coordinate concurrent operations (port allocation, container
   lifecycle, instance state), reach for channels and select, not mutexes.

4. **Comments explain why, not what.** Code should be readable on its own.
   Add comments only when the reason behind a decision is not obvious from the
   code — a non-obvious edge case, a performance trade-off, a constraint from
   an external system. Keep them short. Avoid walls of text.

5. **Go conventions first.** Follow standard Go idioms: short variable names in
   narrow scopes, early returns, zero-value initialization, explicit error
   handling. Run the full CI checks locally before committing:
   ```
   gofmt -s -w .
   go mod tidy
   go vet ./...
   go test -race -count=1 ./...
   golangci-lint run
   ```

6. **Clarity with brevity.** The simplest solution that works is the best
   solution. Clever code is a liability — it breaks unexpectedly and resists
   modification. Write code a tired teammate (or agent) can read at 2 AM.

7. **Thoroughness.** We care about edge cases, error handling, test coverage,
   and getting the design right before writing the code. Software should shine.
   But thoroughness serves the user, not itself — don't gold-plate.

8. **Pragmatism.** Beautiful software that ships late is worse than good
   software that ships now. Know when a heuristic is good enough, when a
   hardcoded default beats a config option, and when a TODO marker is the
   honest answer.

---

## Conventions

### Go style

- Follow `gofmt` formatting. Use `-s` for simplifications.
- Short names: `bp` not `blueprint`, `svc` not `service`, `errs` not
  `errorList`. Scope dictates length.
- Early returns over deep nesting. Flatten.
- Always handle errors explicitly. No `_` for errors unless the caller is
  documented to never fail.
- Test files use `t.Helper()` for setup helpers. Test names follow
  `Test<Package>_<Behavior>` (e.g. `TestInit_SampleRepo_GoldenMatch`).

### Packages

- `cmd/plax/` — CLI entrypoint only. No business logic.
- `pkg/<domain>/` — one package per domain (blueprint, registry, portpool,
  derive, etc.). No circular imports.
- `docs/` — design doc and phased plans. Plans track implementation status
  with checkboxes.

### JSON

- No JSON comments. `plax.json` is plain JSON.
- Struct tags use `json:"field_name"` for all exported fields.
- Omitempty for optional fields only.

### Testing

- Tests run with `-race`. No exceptions.
- Golden files in `testdata/<fixture>/` directories. Compare with `go-cmp`.
- Test fixtures are generic (no proprietary references). The sample/reference
  repo is a representative Next.js + Postgres + Redis app — no real product
  name appears anywhere.

### Blueprint

- The blueprint is a contract between the repo and the tool. It declares what
  the repo needs, not what the tool guesses.
- `plax init` is a scaffold, not a detector. It gives the agent a starting
  point; the agent fills in the gaps (processes, seed command, isolation
  overrides). See `docs/design.md` §2.1.

### CLI

- Kong for argument parsing.
- Records go to stdout, human chatter to stderr.
- `--json` flag for structured output where applicable.
