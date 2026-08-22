# Plan 19 — `plax upgrade`: install-method-aware self-update

## Objective

Add a `plax upgrade` command that updates the installed plax binary to the
latest release, honoring the install method (Homebrew, `go install`, direct
binary) that put the binary on disk, with atomic replacement for the
direct-binary path (issue #65).

## Decisions recorded from triage 2026-08-21

**Phase 3 ships as prescribed:** self-contained tooling polish, sequenced
after the #66 decision (now shipped as `plax guide`). Three decisions
confirmed with the maintainer:

**The package-manager paths auto-run, not instruct.** `plax upgrade` on a
Homebrew install shells out to `brew upgrade plax`; on a `go install` install
it shells out to `go install github.com/apollopower/plax/cmd/plax@latest`.
Both stream the child's stdout/stderr and propagate its exit code. The
package manager is detected from the install itself: `brew list --versions
plax` exits 0 only for formula-tracked installs (the Cellar symlink in the
executable path is a weaker signal — a binary could coincidentally live under
`Cellar`), and the resolved executable path is compared against `$GOBIN`,
`go env GOPATH/bin`, and the `$HOME/go/bin` default. Only the direct-binary
path self-replaces.

**`--check` is a report-only mode.** `plax upgrade --check` looks up the
latest release (the same GitHub code path for every install method),
compares against the current version, and prints the method-appropriate
update path. No writes, no child processes. Exit 0 when current, exit 1 when
outdated, exit 2 when the lookup fails. Agents and scripts get a cheap
liveness probe.

**`plax upgrade` is stateless, like `plax guide`.** No repo root, no
`plax.json`, no registry. It must work in an empty directory from a broken
or outdated install. No existing package is touched; the domain lives in a
new `pkg/upgrade`.

## Package layout

```
pkg/upgrade/upgrade.go      # Method detection, version comparison, orchestration
pkg/upgrade/upgrade_test.go # detection matrix, semver, orchestration tests
pkg/upgrade/github.go       # latest-release lookup + asset resolution via GitHub API
pkg/upgrade/github_test.go  # httptest-driven release lookup tests
pkg/upgrade/replace.go      # atomic binary replacement (temp + rename), go-path resolution
pkg/upgrade/replace_test.go # atomic-replace and go-path-resolution tests
cmd/plax/main.go            # UpgradeCmd (--check, --force), runUpgrade, dispatch
cmd/plax/main_test.go       # runUpgrade --check exit-code behavior with a fake release server
cmd/plax/guide.md           # "Command reference" gains plax upgrade
docs/manual.md              # §2 Installation gains an Upgrade section
README.md                   # command list gains plax upgrade
docs/plans/index.md         # add plan 19 to the phase table
docs/plans/triage/2026-08-21.md # check the #65 acceptance box when shipped
```

## Type specifications

### `pkg/upgrade/upgrade.go`

```go
// Method is how the running binary was installed.
type Method string

const (
    MethodBrew      Method = "brew"       // tracked by Homebrew
    MethodGoInstall Method = "go-install" // in GOBIN / GOPATH/bin
    MethodDirect    Method = "direct"     // anywhere else
    MethodUnknown   Method = "unknown"    // detection could not decide
)

// Detect returns the install method for the binary at exePath, which must
// be the already-symlink-resolved path (caller uses EvalSymlinks).
func Detect(exePath string) Method

// GoBinPath returns the path a go-installed plax would live at: $GOBIN/plax
// when GOBIN is set, else $(go env GOPATH)/bin/plax, else $HOME/go/bin/plax.
func GoBinPath() string

// IsGoInstallPath reports whether path equals GoBinPath().
func IsGoInstallPath(path string) bool

// Outdated compares current against latest ("v0.1.1" shape). A "dev" current
// version or an unparseable one returns (false, nil) — never outdated. A
// malformed latest version returns an error: we must not upgrade into a
// version we cannot compare.
func Outdated(current, latest string) (bool, error)

// RunChild executes argv with stdio wired through and returns the child's
// exit code, or -1 when the process could not be started.
func RunChild(argv []string) int
```

The orchestration (lookup → compare → method dispatch → replace) lives in
`cmd/plax` as `runUpgrade`, following the codebase's thin-CLI convention;
`pkg/upgrade` provides only the mechanics.

### `pkg/upgrade/github.go`

```go
// DefaultAPIBase is the production GitHub REST API base URL; APIBase is a
// var so tests point it at an httptest server.
const DefaultAPIBase = "https://api.github.com"
var APIBase = DefaultAPIBase

// Asset and Release are the decoded latest-release payload.
type Asset struct {
    Name string // e.g. plax_v0.2.0_linux_amd64.tar.gz
    URL  string // browser_download_url — the download host, not the API
}
type Release struct {
    Tag    string // e.g. v0.2.0
    Assets []Asset
}

// LatestRelease fetches the newest release of repo. A 404 (no releases
// yet) returns an empty Release with no error.
func LatestRelease(client *http.Client, repo string) (Release, error)

// AssetFor returns the archive asset matching goos/goarch, or false when
// the release carries none. Format is enforced: linux → tar.gz, darwin →
// zip — a darwin release never uses tar.gz. Asset names follow
// .goreleaser.yml: plax_<tag>_<goos>_<goarch>.tar.gz / .zip; matching
// compares the trailing goos/goarch so a tag with underscores cannot
// confuse it.
func AssetFor(assets []Asset, goos, goarch string) (Asset, bool)

// AssetURL is AssetFor + URL, for callers that only need the download URL.
func AssetURL(assets []Asset, goos, goarch string) string

// ChecksumsURL returns the download URL of the release's checksums.txt
// asset, or "" when the release carries none.
func ChecksumsURL(assets []Asset) string

// Download fetches rawURL into a temp file in dir and returns its path.
// The temp name keeps the URL's archive extension (.tar.gz / .zip) so
// ExtractArchive can pick the format; the caller renames into place or
// removes it on failure.
func Download(client *http.Client, rawURL, dir string) (string, error)

// ExtractArchive unpacks the archive at path into dir and returns the
// extracted binary path. The goreleaser archives carry LICENSE and
// README.md next to the binary, so extraction selects the entry named
// "plax" and errors when it is absent — never the first entry.
func ExtractArchive(path, dir string) (string, error)

// VerifyChecksum checks path against the release's checksums.txt (fetched
// from sumURL): the archive's SHA-256 must appear as "hash  <assetName>".
// assetName is the real asset name — the archive sits in a temp file whose
// random name never appears in the checksum file.
func VerifyChecksum(client *http.Client, path, sumURL, assetName string) error
```

### `pkg/upgrade/replace.go`

```go
// AtomicReplace replaces the file at dest with the contents of src via
// rename(2) (src must be a sibling temp file so both live on one
// filesystem). Rename-over-running-executable succeeds on Linux and macOS,
// which is what makes self-replacement safe. Falls back to a copy+chmod+
// rename (replaceCopy) when src cannot be renamed (cross-device). Keeps
// dest's existing mode when present, else 0755. Removes src either way.
func AtomicReplace(src, dest string) error
```

## Algorithms

### Install-method detection

```
exePath := os.Executable()
resolved := EvalSymlinks(exePath)
    ⚠ a brew install symlinks $(brew --prefix)/bin/plax into the Cellar;
      only the resolved path reveals it, and even that can lie — the
      `brew list --versions plax` probe is authoritative.

1. if brew is on PATH:
     run `brew list --versions plax` with a 2s timeout
     → exit 0:   MethodBrew
     → otherwise: continue     ⚠ brew installed but plax not formula-tracked
2. if IsGoInstallPath(resolved):  MethodGoInstall
3. MethodDirect (MethodUnknown exists as a type for future gaps; Detect
   never returns it today)
```

`GoBinPath()` / `IsGoInstallPath(path)`:

```
if GOBIN env set:            path == $GOBIN/plax
else if go is on PATH:
    `go env GOPATH` (2s)     path == $GOPATH/bin/plax
                             ⚠ an unset GOPATH makes go env print the
                               default $HOME/go — go covers it
else:                        path == $HOME/go/bin/plax
```

### Version comparison

```
normalize(v): strip a leading "v"; split on "."; each of the three parts
              must parse as a uint; any extra component (pre-release,
              build metadata) → error
compare(a, b): -1 / 0 / +1 by major, then minor, then patch

Outdated(current, latest):
    if current == "dev" or normalize(current) errors → (false, nil)
        ⚠ a from-source build cannot be compared; never claim outdated
    if normalize(latest) errors → (false, err)
    return compare(current, latest) < 0
```

### Latest-release lookup

```
GET https://api.github.com/repos/<repo>/releases/latest
    Accept: application/vnd.github+json
    User-Agent: plax/<version>

→ 200: parse {tag_name, assets}; the tag may carry a "v" prefix —
       normalization happens at comparison. Draft and pre-release tags
       never appear here (GitHub's latest endpoint excludes them).
→ 404: no releases yet → up to date; message "no releases found"
→ other / network error → error: upgrade cannot proceed without a
       version answer
       ⚠ the endpoint is unauthenticated; a 403 rate-limit is possible for
         machines sharing an IP — surface it as a plain error naming the
         URL so the user can retry
```

### Direct-binary upgrade (the self-replace path)

```
1. tag := LatestRelease(...);  error → return it
2. outdated, err := Outdated(current, tag)
     !outdated → check mode prints "up to date", exits 0; upgrade mode
     prints "already at latest (vX.Y.Z)", returns nil
     (a dev build with --force skips this short-circuit)
3. assets from the release payload; url := AssetURL(assets, GOOS, GOARCH)
       ⚠ no matching asset → error naming the platform — this is how a
         platform gap surfaces; plax ships linux/darwin × amd64/arm64
4. archive, err := Download(client, url, filepath.Dir(exePath))
       ⚠ download into the destination directory so the later rename stays
         on one filesystem (rename(2) fails EXDEV across mounts); the temp
         name keeps the .tar.gz/.zip extension for format detection
5. sumURL := ChecksumsURL(assets); missing → refuse the upgrade
   VerifyChecksum(client, archive, sumURL)
       ⚠ the archive is never trusted — and never renamed into place —
         before its SHA-256 matches the release's checksums.txt
6. extractDir := MkdirTemp(dir, ".plax-upgrade-*")
   bin := ExtractArchive(archive, extractDir)
7. AtomicReplace(bin, exePath)
       ⚠ rename over a running executable succeeds on Linux and macOS;
         this answers the triage's /proc/self/exe note with temp+rename
         instead of an in-place write (ETXTBSY on truncate)
8. print "plax updated: vOld → vNew"
       ⚠ long-running agents hold the old inode; nothing to do on our side
```

### brew / go-install upgrade

```
1. tag := LatestRelease(...);  error → return it
2. outdated := Outdated(current, tag)
       ⚠ a brew formula may lag the latest tag; "already up to date" is
         then still correct for that install method — the formula is what
         brew serves. When outdated, `brew upgrade plax` resolves the
         formula bump regardless.
3. --check: print current/latest/method and the command that would run;
     exit 0 if current, 1 if outdated. Never spawn children.
4. run the child command (brew/go-install) with Stdin/Stdout/Stderr wired
     through; on success print "plax updated"; propagate the child's exit
     code otherwise.
       ⚠ no re-teeing of child output — the child's output is the user's
         answer; plax adds one closing line on success
```

## CLI specification

```
plax upgrade [--check] [--force]

--check   report the latest release and the update path; never write,
          never spawn children. Exit 0 = current, 1 = outdated,
          2 = version lookup failed.
--force   for dev builds ("dev" version): skip the can't-compare refusal
          and perform the method-appropriate update anyway (brew/go-install
          children, or direct replacement of the current binary).

Behavior per install method (without --check):

brew        run `brew upgrade plax`; stream; exit with the child's code
go-install  run `go install github.com/apollopower/plax/cmd/plax@latest`;
            stream; exit with the child's code
direct      GitHub lookup → checksum → atomic replace; exit 0 on success,
            1 on failure
dev build   refuse with "cannot determine current version (dev build)"
            before any network I/O; --force overrides for every method
unknown     not reachable via Detect (it always returns a Method); kept as
            a type so future detection gaps have a home

Check mode on a dev build: prints "current: dev (source build)" with the
latest tag and the update path, and exits 1 when a release exists (any
release is newer than a from-source build), 0 when none does.

stdout      records: "plax updated: v0.1.1 → v0.2.0", "already at latest
            (v0.1.1)", check-mode status lines. Child output passes through
            untouched.
stderr      human chatter and warnings only.
exit codes  0 success / up to date; 1 failure / outdated (--check);
            2 lookup failure (--check only); child exit codes propagate
            verbatim on the brew/go-install paths.
```

### Example — up to date

```
$ plax upgrade
already at latest (v0.1.1)
```

### Example — direct-binary update

```
$ plax upgrade
plax updated: v0.1.1 → v0.2.0
```

### Example — brew install, check only

```
$ plax upgrade --check
current: v0.1.1
latest:  v0.2.0
method:  brew
run:     brew upgrade plax
$ echo $?
1
```

### Example — dev build refuses

```
$ plax upgrade
cannot determine current version: dev build — rebuild from source, or pass --force to replace the binary with the latest release
```

## Error handling

| Failure | Behavior |
|---|---|
| GitHub API unreachable | error naming the URL; nothing written, nothing run; exit 1 |
| GitHub API 403 (rate limit) | same as above; message suggests retrying later |
| 404 (no releases) | "no releases found" — treated as up to date, exit 0 |
| release has no asset for GOOS/GOARCH | error naming the platform, exit 1 |
| release has no checksums.txt | refusal — "refusing an unverified upgrade", exit 1 |
| checksum mismatch | downloaded archive discarded, target untouched, exit 1 |
| asset download fails mid-transfer | temp file removed; target untouched; exit 1 |
| archive extraction fails | temp archive removed; target untouched; exit 1 |
| rename fails (EACCES, EXDEV despite same-dir temp) | error naming both paths, exit 1 |
| `brew list --versions plax` times out (2s) | treated as not-brew; detection continues |
| `go env` fails or times out | fall back to `$HOME/go/bin` comparison only |
| `brew upgrade` exits non-zero | child's output already streamed; its exit code propagated |
| `go install` exits non-zero | same propagation |
| current version `dev` and no `--force` | refuse with the message above, exit 1 |
| `--check` lookup fails | exit 2, no partial output on stdout |

## Tests

### `pkg/upgrade/upgrade_test.go`

- `TestUpgrade_Detect_BrewInstalled` — fake `brew` on PATH (a shell script
  that exits 0 for `list --versions plax`) → `MethodBrew`.
- `TestUpgrade_Detect_BrewNotTracking` — fake `brew` exits 1 →
  falls through to the path checks (`MethodGoInstall` via `$GOBIN`).
- `TestUpgrade_Detect_BrewAbsent` — no `brew` on PATH → path checks.
- `TestUpgrade_Detect_GoInstallPath` — PATH includes a fake `go` printing a
  temp GOPATH; binary at `$GOPATH/bin/plax` → `MethodGoInstall`.
- `TestUpgrade_Detect_GoInstall_NoGoOnPath` — `$HOME/go/bin/plax` with no
  `go` binary → `MethodGoInstall` via the `$HOME/go` fallback.
- `TestUpgrade_Detect_Direct` — binary in a temp dir, brew absent, not
  under any go path → `MethodDirect`.
- `TestUpgrade_GoBinPath_GOBIN` — `GOBIN` set → `$GOBIN/plax`.
- `TestUpgrade_GoBinPath_GOPATHEnv` — `GOPATH` set → `$GOPATH/bin/plax`.
- `TestUpgrade_GoBinPath_NoGoInstalled` — neither `GOBIN`, `GOPATH`, nor
  `go` on PATH → `$HOME/go/bin/plax`.
- `TestUpgrade_Outdated_NewerPatch` — `v0.1.1` vs `v0.1.2` → true.
- `TestUpgrade_Outdated_NewerMinor` — `v0.1.9` vs `v0.2.0` → true.
- `TestUpgrade_Outdated_Equal` — `v0.1.1` vs `v0.1.1` → false.
- `TestUpgrade_Outdated_LocalNewer` — `v0.2.0` vs `v0.1.1` → false.
- `TestUpgrade_Outdated_DevNeverOutdated` — `dev` vs `v9.9.9` → false, nil.
- `TestUpgrade_Outdated_PrereleaseTag_Errors` — latest `v0.2.0-rc1` →
  error, never a silent downgrade.
- `TestUpgrade_Outdated_BareVersion` — current `0.1.1` (no `v`) parses and
  compares.
- `TestUpgrade_Outdated_CurrentMalformed` — current `banana` → false, nil.
- `TestUpgrade_RunChild_PropagatesExitCode` — fake script exits 3 →
  `RunChild` returns 3.
- `TestUpgrade_RunChild_NotFound` — missing binary → -1.

### `pkg/upgrade/github_test.go`

- `TestUpgrade_LatestRelease_Success` — httptest server returns a release
  with tag `v0.2.0`; `LatestRelease` returns it; the request carries the
  `Accept: application/vnd.github+json` header.
- `TestUpgrade_LatestRelease_NoReleases` — 404 → empty Release, nil error.
- `TestUpgrade_LatestRelease_ServerError` — 500 → error.
- `TestUpgrade_LatestRelease_RateLimited` — 403 → error naming the rate
  limit.
- `TestUpgrade_AssetURL_Linux` — assets include `plax_v0.2.0_linux_amd64.
  tar.gz` → that URL.
- `TestUpgrade_AssetURL_Darwin` — both a `.tar.gz` and a `.zip` asset →
  the zip wins (format is enforced on darwin).
- `TestUpgrade_AssetURL_Missing` — no asset for the platform → "".
- `TestUpgrade_AssetURL_UnderscoreTag` — tag containing underscores →
  trailing goos/goarch comparison still matches.
- `TestUpgrade_Download_FetchesAndExtracts` — server serves real tar.gz
  bytes; download keeps the extension; extraction yields an executable
  (0755) binary.
- `TestUpgrade_Download_HTTPError` — 500 → error, no file left behind.
- `TestUpgrade_Extract_TarGz` — a real tar.gz (LICENSE + README.md + plax)
  round-trips to the plax binary.
- `TestUpgrade_Extract_Zip` — a real zip round-trips.
- `TestUpgrade_Extract_PicksBinaryOverLicense` — LICENSE/README.md entries
  before plax are skipped; the plax entry is extracted. Regression for the
  real-release smoke test where the first entry (LICENSE) was extracted
  instead of the binary.
- `TestUpgrade_Extract_MissingBinary` — archive without a plax entry →
  error.
- `TestUpgrade_VerifyChecksum_Match` — matching `checksums.txt` line →
  nil.
- `TestUpgrade_VerifyChecksum_TempArchiveName` — the temp archive basename
  differs from the asset name; the asset name must match. Regression for
  the smoke-test checksum failure.
- `TestUpgrade_VerifyChecksum_Mismatch` — wrong hash → error.

### `pkg/upgrade/replace_test.go`

- `TestUpgrade_AtomicReplace_SwapsFile` — src/dest in one temp dir; dest
  replaced with a new inode, content matches, mode preserved.
- `TestUpgrade_AtomicReplace_ExistingModeKept` — dest has mode 0700; after
  replace it is still 0700.
- `TestUpgrade_AtomicReplace_MissingDest_Gets755` — new dest lands 0755.
- `TestUpgrade_AtomicReplace_SrcMissing` — error, dest untouched.
- `TestUpgrade_ReplaceCopy_CrossDeviceFallback` — `replaceCopy` (the
  cross-device fallback, exercised directly since CI cannot fabricate a
  second filesystem) yields correct content and mode and removes src.

### `cmd/plax/main_test.go`

The child-mode pattern: `runUpgradeAsChild` re-executes the test binary
with `PLAX_TEST_UPGRADE_CHILD=1` so the `os.Exit` paths run for real;
`TestUpgradeChild_CheckMode` is the guarded entry point.

- `TestUpgrade_Check_Outdated_Exit1` — `--check` against an httptest server
  reporting a newer version → exit 1, stdout reports current/latest/method.
- `TestUpgrade_Check_Current_Exit0` — server reports the same version →
  exit 0.
- `TestUpgrade_Check_LookupFailure_Exit2` — server 500s → exit 2.
- `TestUpgrade_UpgradeMode_DevBuild_Refuses` — upgrade mode on `dev`
  refuses before any network I/O.
- `TestUpgrade_UpgradeMode_DevBuild_Force_Proceeds` — `--force` skips the
  refusal and fails cleanly on a release with no matching asset.

### Fixtures

- Fake `brew`/`go` are inline shell scripts written to `t.TempDir()` and
  prepended to `PATH` — no repo fixtures needed.
- Release archives are built in-test with `archive/tar` + `compress/gzip`
  and `archive/zip` (stdlib only).

## Acceptance criteria

- [ ] `plax upgrade --check` prints current and latest versions; exit 0
      when current, 1 when outdated, 2 when the lookup fails
- [ ] A Homebrew install (detected via `brew --prefix plax`) runs
      `brew upgrade plax` with stdio wired through and the child's exit
      code
- [ ] A `go install` install (binary under GOBIN/GOPATH-bin, incl. the
      `$HOME/go/bin` fallback with no `go` on PATH) runs
      `go install github.com/apollopower/plax/cmd/plax@latest`
- [ ] A direct-binary install replaces the running binary atomically: a
      new inode is in place after `plax upgrade`, and the process can
      re-exec (verified by inode identity, not by spawn)
- [ ] `plax upgrade` on a dev build refuses with an explanatory message and
      exits 1; `plax upgrade --force` proceeds
- [ ] `plax upgrade` when already at the latest release prints "already at
      latest" and exits 0 without downloading anything
- [ ] Asset selection matches `.goreleaser.yml` naming
      (`plax_<tag>_<goos>_<goarch>.tar.gz|.zip`) for linux/amd64 and
      darwin/arm64
- [ ] Failed downloads, extraction failures, and rename failures leave the
      installed binary untouched
- [ ] `plax upgrade` succeeds from a directory with no plax.json, no git
      repo, and no registry
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes (with and without
      `PLAX_TEST_POSTGRES_URL`)
- [ ] `golangci-lint run` passes

## Dependencies

No new external dependencies.

| Package | Import path | Purpose |
|---|---|---|
| `archive/tar`, `archive/zip`, `compress/gzip` | stdlib | release archive extraction |
| `encoding/json` | stdlib | GitHub API payload parsing |
| `net/http` | stdlib | release lookup and download |
| `os/exec` | stdlib | brew/go probes and upgrade children |
| `runtime` | stdlib | GOOS/GOARCH for asset selection |
