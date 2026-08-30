package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/apollopower/plax/pkg/upgrade"
)

// fakeUpgradeServer serves a canned latest-release payload (or HTTP status)
// for the GitHub API shape runUpgrade consumes.
func fakeUpgradeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runUpgradeAsChild re-executes the test binary in child mode so the
// os.Exit paths inside runUpgrade are exercised for real. Returns the exit
// code and combined output.
func runUpgradeAsChild(t *testing.T, apiBase, ver string) (int, string) {
	t.Helper()
	clean := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestUpgradeChild_CheckMode")
	cmd.Env = append(os.Environ(),
		"PLAX_TEST_UPGRADE_CHILD=1",
		"PLAX_TEST_UPGRADE_API="+apiBase,
		"PLAX_TEST_UPGRADE_VERSION="+ver,
		"PATH="+clean,
		"GOBIN=",
		"GOPATH=",
		"HOME="+clean,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running child: %v", err)
		}
		code = ee.ExitCode()
	}
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("child test failed:\n%s", out)
	}
	return code, string(out)
}

// TestUpgradeChild_CheckMode is the child-mode entry point driven by
// runUpgradeAsChild; skipped when run directly.
func TestUpgradeChild_CheckMode(t *testing.T) {
	if os.Getenv("PLAX_TEST_UPGRADE_CHILD") != "1" {
		t.Skip("child mode only")
	}
	t.Cleanup(func() { upgrade.APIBase = upgrade.DefaultAPIBase })
	upgrade.APIBase = os.Getenv("PLAX_TEST_UPGRADE_API")
	version = os.Getenv("PLAX_TEST_UPGRADE_VERSION")

	_ = runUpgrade(UpgradeCmd{Check: true})
}

func TestUpgrade_Check_Outdated_Exit1(t *testing.T) {
	srv := fakeUpgradeServer(t, 200, `{"tag_name":"v0.2.0","assets":[]}`)

	code, out := runUpgradeAsChild(t, srv.URL, "v0.1.1")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"current: v0.1.1", "latest:  v0.2.0", "method:  direct"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUpgrade_Check_Current_Exit0(t *testing.T) {
	srv := fakeUpgradeServer(t, 200, `{"tag_name":"v0.1.1","assets":[]}`)

	code, out := runUpgradeAsChild(t, srv.URL, "v0.1.1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "latest:  v0.1.1") {
		t.Fatalf("output missing latest line:\n%s", out)
	}
}

func TestUpgrade_Check_LookupFailure_Exit2(t *testing.T) {
	srv := fakeUpgradeServer(t, 500, "")

	code, out := runUpgradeAsChild(t, srv.URL, "v0.1.1")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", code, out)
	}
}

func TestUpgrade_UpgradeMode_DevBuild_Refuses(t *testing.T) {
	version = "dev"

	err := runUpgrade(UpgradeCmd{})
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Fatalf("runUpgrade on dev build = %v, want dev-build refusal", err)
	}
}

func TestUpgrade_UpgradeMode_DevBuild_Force_Proceeds(t *testing.T) {
	// --force on a dev build skips the refusal and runs the direct path. A
	// release without a matching asset must fail cleanly before touching
	// anything.
	srv := fakeUpgradeServer(t, 200, `{"tag_name":"v0.2.0","assets":[]}`)
	t.Cleanup(func() { upgrade.APIBase = upgrade.DefaultAPIBase })
	upgrade.APIBase = srv.URL
	version = "dev"

	err := runUpgrade(UpgradeCmd{Force: true})
	if err == nil || !strings.Contains(err.Error(), "no archive for") {
		t.Fatalf("runUpgrade(force) = %v, want no-archive error", err)
	}
}

// TestUp_KongParsesSkip verifies both --skip forms (comma-separated and
// repeated) reach the Up command struct for the canonical parse set.
func TestUp_KongParsesSkip(t *testing.T) {
	var cli CLI
	k, err := kong.New(&cli)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := k.Parse([]string{"up", "--skip", "migrate,verify", "--skip", "verify", "i1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Kong splits comma-separated slice values; repeated flags accumulate.
	// Both forms resolve to the same set in ParseSkip.
	if len(cli.Up.Skip) != 3 || cli.Up.Skip[0] != "migrate" || cli.Up.Skip[1] != "verify" || cli.Up.Skip[2] != "verify" {
		t.Errorf("Skip = %v, want [migrate verify verify]", cli.Up.Skip)
	}
}

func TestUp_UnknownSkipStep_FailsBeforeSideEffects(t *testing.T) {
	err := runUp(UpCmd{Name: "i1", Skip: []string{"migrate,bogus"}})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("runUp = %v, want unknown-step error", err)
	}
	if !strings.Contains(err.Error(), "migrate, verify") {
		t.Errorf("error should list valid steps: %v", err)
	}
}

func TestUp_EmptySkipStep_FailsBeforeSideEffects(t *testing.T) {
	if err := runUp(UpCmd{Name: "i1", Skip: []string{"migrate,"}}); err == nil {
		t.Fatal("runUp with an empty skip step should fail")
	}
}
