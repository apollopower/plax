// Package upgrade detects how the plax binary was installed and updates it
// along the method-appropriate path: Homebrew, go install, or direct binary
// replacement.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Method is how the running binary was installed.
type Method string

const (
	MethodBrew      Method = "brew"       // tracked by Homebrew
	MethodGoInstall Method = "go-install" // in GOBIN / GOPATH/bin
	MethodDirect    Method = "direct"     // anywhere else
	MethodUnknown   Method = "unknown"    // detection could not decide
)

// probeTimeout bounds the brew/go probes; a slow or hung package manager
// must not stall detection.
const probeTimeout = 2 * time.Second

// Detect returns the install method for the binary at exePath. exePath must
// be the already-symlink-resolved path (the caller uses EvalSymlinks): a
// brew install symlinks $(brew --prefix)/bin/plax into the Cellar, so only
// the resolved path and the brew probe tell the two apart.
func Detect(exePath string) Method {
	if brewTracksPlax() {
		return MethodBrew
	}
	if IsGoInstallPath(exePath) {
		return MethodGoInstall
	}
	return MethodDirect
}

// brewTracksPlax reports whether the brew formula for plax is installed.
// `brew list --versions plax` exits 0 only for formula-tracked installs,
// which makes it authoritative where path heuristics can lie.
func brewTracksPlax() bool {
	brew, err := exec.LookPath("brew")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, brew, "list", "--versions", "plax").Run() == nil
}

// IsGoInstallPath reports whether path is a go-installed plax: the file
// GOBIN or (go env GOPATH)/bin would place it at. GOBIN wins when set; `go
// env GOPATH` answers even when GOPATH is unset (it prints the $HOME/go
// default); without go on PATH the same default is assumed directly.
func IsGoInstallPath(path string) bool {
	return path == GoBinPath()
}

// GoBinPath returns the path a go-installed plax would live at: $GOBIN/plax
// when GOBIN is set, else $(go env GOPATH)/bin/plax, else $HOME/go/bin/plax.
// Empty when none of those is resolvable.
func GoBinPath() string {
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		return filepath.Join(gobin, "plax")
	}
	if gopath := goEnvGOPATH(); gopath != "" {
		return filepath.Join(gopath, "bin", "plax")
	}
	return ""
}

// goEnvGOPATH resolves GOPATH the way `go env GOPATH` would: the variable,
// else $HOME/go. Empty when go is on PATH but refuses to answer.
func goEnvGOPATH() string {
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		return gopath
	}
	if gp, err := exec.LookPath("go"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, gp, "env", "GOPATH").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "go")
}

// Outdated reports whether latest is newer than current. Both use the
// "vX.Y.Z" release-tag shape. A current version that cannot be compared
// ("dev" builds, empty, malformed) is never outdated; a malformed latest is
// an error — plax must not upgrade into a version it cannot compare.
func Outdated(current, latest string) (bool, error) {
	cur, err := parseVersion(current)
	if err != nil {
		return false, nil
	}
	lat, err := parseVersion(latest)
	if err != nil {
		return false, fmt.Errorf("latest release %q is not a comparable version: %w", latest, err)
	}
	return compareVersion(cur, lat) < 0, nil
}

// version is a parsed semver major.minor.patch triple.
type version struct {
	major, minor, patch uint64
}

// parseVersion accepts "v0.1.1" or "0.1.1". Anything with more than three
// dot-separated parts, non-numeric parts, or pre-release/build suffixes is
// rejected: our release tags are always plain vX.Y.Z.
func parseVersion(v string) (version, error) {
	s := strings.TrimPrefix(v, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, fmt.Errorf("not a vX.Y.Z version")
	}
	var out version
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return version{}, fmt.Errorf("not a vX.Y.Z version")
		}
		switch i {
		case 0:
			out.major = n
		case 1:
			out.minor = n
		default:
			out.patch = n
		}
	}
	return out, nil
}

// compareVersion returns -1, 0, or +1 by major, then minor, then patch.
func compareVersion(a, b version) int {
	for _, c := range []struct{ x, y uint64 }{
		{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch},
	} {
		switch {
		case c.x < c.y:
			return -1
		case c.x > c.y:
			return 1
		}
	}
	return 0
}

// RunChild executes argv with stdio wired through and returns the child's
// exit code, or -1 when the process could not be started at all.
func RunChild(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return -1
	}
	return 0
}
