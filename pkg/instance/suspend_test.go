package instance

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/apollopower/plax/pkg/process"
	"github.com/apollopower/plax/pkg/registry"
)

func TestSuspend_Success(t *testing.T) {
	deps, bm, drv := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rec, _ := deps.Registry.GetInstance("i1")
	pgid := rec.PIDs["app"]

	if err := Suspend(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not in registry")
	}
	if rec.State != registry.StateSuspended {
		t.Errorf("state = %s, want suspended", rec.State)
	}
	if len(rec.PIDs) != 0 {
		t.Errorf("PIDs should be cleared, got %v", rec.PIDs)
	}
	if process.IsAlive(pgid) {
		t.Error("process should be dead after suspend")
	}
	if len(drv.stopped) != 1 {
		t.Errorf("container should be stopped, stopped=%v", drv.stopped)
	}
	_ = bm
}

func TestSuspend_AlreadySuspended(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := Suspend(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("first Suspend: %v", err)
	}
	if err := Suspend(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}

	rec, _ := deps.Registry.GetInstance("i1")
	if rec.State != registry.StateSuspended {
		t.Errorf("state = %s, want suspended", rec.State)
	}
}

func TestSuspend_NotFound(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	err := Suspend(context.Background(), deps, "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Suspend = %v, want not found error", err)
	}
}

func TestSuspend_NilDocker(t *testing.T) {
	deps, _, drv := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}

	deps.Docker = nil

	if err := Suspend(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec, _ := deps.Registry.GetInstance("i1")
	if rec.State != registry.StateSuspended {
		t.Errorf("state = %s, want suspended", rec.State)
	}

	deps.Docker = drv
	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("cleanup Down: %v", err)
	}
}

func TestResume_Success(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	firstRec, _ := deps.Registry.GetInstance("i1")
	firstPorts := firstRec.Ports

	if err := Suspend(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	envPath := filepath.Join(firstRec.WorktreePath, ".env")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf(".env missing after suspend: %v", err)
	}

	if err := Resume(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not in registry")
	}
	if rec.State != registry.StateRunning {
		t.Errorf("state = %s, want running", rec.State)
	}
	for k, v := range firstPorts {
		if rec.Ports[k] != v {
			t.Errorf("port %s changed: %d -> %d", k, v, rec.Ports[k])
		}
	}
	if len(rec.PIDs) != 1 {
		t.Errorf("PIDs not repopulated after resume")
	}

	if _, err := os.ReadFile(envPath); err != nil {
		t.Fatalf("read .env: %v", err)
	}

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("cleanup Down: %v", err)
	}
}

func TestResume_NotSuspended(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = Down(context.Background(), deps, "i1") }()

	err := Resume(context.Background(), deps, "i1")
	if err == nil || (!strings.Contains(err.Error(), "already running") && !strings.Contains(err.Error(), "expected suspended")) {
		t.Fatalf("Resume on running instance = %v, want error", err)
	}
}

func TestResume_PortTaken(t *testing.T) {
	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rec, _ := deps.Registry.GetInstance("i1")
	if err := Suspend(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	var bound net.Listener
	for _, port := range rec.Ports {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		bound = ln
		break
	}
	if bound == nil {
		t.Fatal("could not bind to any instance port for test")
	}
	defer func() { _ = bound.Close() }()

	err := Resume(context.Background(), deps, "i1")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("Resume = %v, want port in use error", err)
	}

	recAfter, _ := deps.Registry.GetInstance("i1")
	if recAfter.State != "suspended" {
		t.Errorf("state should still be suspended after failed resume, got %s", recAfter.State)
	}

	_ = bound.Close()

	if err := Down(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("cleanup Down: %v", err)
	}
}

func TestUp_RecordsBaseRefAndToolVersions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH — cannot resolve toolchain versions")
	}

	deps, _, _ := testDeps(t, testBlueprint())

	if err := Up(context.Background(), deps, "i1"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = Down(context.Background(), deps, "i1") }()

	rec, found := deps.Registry.GetInstance("i1")
	if !found {
		t.Fatal("instance not in registry")
	}
	if rec.BaseRef == "" {
		t.Error("BaseRef not recorded")
	}
	if rec.BaseCommit == "" {
		t.Error("BaseCommit not recorded")
	}
	if len(rec.Provenance.ToolVersions) == 0 {
		t.Error("ToolVersions not recorded")
	}
}
