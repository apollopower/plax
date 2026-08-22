package upgrade

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeScript writes an executable shell script named name into dir and
// returns its path.
func fakeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// cleanPath returns a PATH containing only dir, so real brew/go binaries on
// the host cannot leak into detection tests.
func cleanPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir)
}

func TestUpgrade_Detect_BrewInstalled(t *testing.T) {
	bin := t.TempDir()
	cleanPath(t, bin)
	fakeScript(t, bin, "brew", `exit 0`)

	if got := Detect(filepath.Join(bin, "plax")); got != MethodBrew {
		t.Fatalf("Detect = %q, want %q", got, MethodBrew)
	}
}

func TestUpgrade_Detect_BrewNotTracking(t *testing.T) {
	bin := t.TempDir()
	cleanPath(t, bin)
	// A brew binary that knows nothing about plax must fall through to the
	// path checks, not classify as MethodBrew.
	fakeScript(t, bin, "brew", `exit 1`)
	t.Setenv("GOBIN", bin)

	if got := Detect(filepath.Join(bin, "plax")); got != MethodGoInstall {
		t.Fatalf("Detect = %q, want %q", got, MethodGoInstall)
	}
}

func TestUpgrade_Detect_BrewAbsent(t *testing.T) {
	bin := t.TempDir()
	cleanPath(t, bin)
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	if got := Detect(filepath.Join(bin, "plax")); got != MethodDirect {
		t.Fatalf("Detect = %q, want %q", got, MethodDirect)
	}
}

func TestUpgrade_Detect_GoInstallPath(t *testing.T) {
	gopath := t.TempDir()
	cleanPath(t, gopath)
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	// A fake `go` on PATH that answers `go env GOPATH`.
	fakeScript(t, gopath, "go", `[ "$1" = "env" ] && [ "$2" = "GOPATH" ] && echo "`+gopath+`"`)

	exe := filepath.Join(gopath, "bin", "plax")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := Detect(exe); got != MethodGoInstall {
		t.Fatalf("Detect = %q, want %q", got, MethodGoInstall)
	}
}

func TestUpgrade_Detect_GoInstall_NoGoOnPath(t *testing.T) {
	home := t.TempDir()
	cleanPath(t, t.TempDir())
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", home)
	exe := filepath.Join(home, "go", "bin", "plax")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := Detect(exe); got != MethodGoInstall {
		t.Fatalf("Detect = %q, want %q", got, MethodGoInstall)
	}
}

func TestUpgrade_Detect_Direct(t *testing.T) {
	bin := t.TempDir()
	cleanPath(t, bin)
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	if got := Detect(filepath.Join(bin, "plax")); got != MethodDirect {
		t.Fatalf("Detect = %q, want %q", got, MethodDirect)
	}
}

func TestUpgrade_GoBinPath_GOBIN(t *testing.T) {
	t.Setenv("GOBIN", "/custom/bin")
	if got := GoBinPath(); got != "/custom/bin/plax" {
		t.Fatalf("GoBinPath = %q, want %q", got, "/custom/bin/plax")
	}
}

func TestUpgrade_GoBinPath_GOPATHEnv(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "/custom/gopath")
	if got := GoBinPath(); got != "/custom/gopath/bin/plax" {
		t.Fatalf("GoBinPath = %q, want %q", got, "/custom/gopath/bin/plax")
	}
}

func TestUpgrade_GoBinPath_NoGoInstalled(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	cleanPath(t, t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, "go", "bin", "plax")
	if got := GoBinPath(); got != want {
		t.Fatalf("GoBinPath = %q, want %q", got, want)
	}
}

func TestUpgrade_Outdated_NewerPatch(t *testing.T) {
	if out, err := Outdated("v0.1.1", "v0.1.2"); err != nil || !out {
		t.Fatalf("Outdated(v0.1.1, v0.1.2) = %v, %v; want true, nil", out, err)
	}
}

func TestUpgrade_Outdated_NewerMinor(t *testing.T) {
	if out, err := Outdated("v0.1.9", "v0.2.0"); err != nil || !out {
		t.Fatalf("Outdated(v0.1.9, v0.2.0) = %v, %v; want true, nil", out, err)
	}
}

func TestUpgrade_Outdated_Equal(t *testing.T) {
	if out, err := Outdated("v0.1.1", "v0.1.1"); err != nil || out {
		t.Fatalf("Outdated(v0.1.1, v0.1.1) = %v, %v; want false, nil", out, err)
	}
}

func TestUpgrade_Outdated_LocalNewer(t *testing.T) {
	if out, err := Outdated("v0.2.0", "v0.1.1"); err != nil || out {
		t.Fatalf("Outdated(v0.2.0, v0.1.1) = %v, %v; want false, nil", out, err)
	}
}

func TestUpgrade_Outdated_DevNeverOutdated(t *testing.T) {
	if out, err := Outdated("dev", "v9.9.9"); err != nil || out {
		t.Fatalf("Outdated(dev, v9.9.9) = %v, %v; want false, nil", out, err)
	}
}

func TestUpgrade_Outdated_PrereleaseTag_Errors(t *testing.T) {
	if _, err := Outdated("v0.1.1", "v0.2.0-rc1"); err == nil {
		t.Fatal("Outdated(v0.1.1, v0.2.0-rc1) = nil error; want error")
	}
}

func TestUpgrade_Outdated_BareVersion(t *testing.T) {
	if out, err := Outdated("0.1.1", "v0.1.2"); err != nil || !out {
		t.Fatalf("Outdated(0.1.1, v0.1.2) = %v, %v; want true, nil", out, err)
	}
}

func TestUpgrade_Outdated_CurrentMalformed(t *testing.T) {
	if out, err := Outdated("banana", "v0.1.2"); err != nil || out {
		t.Fatalf("Outdated(banana, v0.1.2) = %v, %v; want false, nil", out, err)
	}
}

func TestUpgrade_RunChild_PropagatesExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixtures are POSIX-only")
	}
	bin := t.TempDir()
	cleanPath(t, bin)
	fakeScript(t, bin, "child", `exit 3`)

	if got := RunChild([]string{filepath.Join(bin, "child")}); got != 3 {
		t.Fatalf("RunChild = %d, want 3", got)
	}
}

func TestUpgrade_RunChild_NotFound(t *testing.T) {
	if got := RunChild([]string{"/nonexistent/plax-fake-binary"}); got != -1 {
		t.Fatalf("RunChild = %d, want -1", got)
	}
}
