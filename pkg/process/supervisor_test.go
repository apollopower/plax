package process

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSpawn_Success(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	pgid, err := Spawn("sleeper", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if pgid <= 0 {
		t.Fatalf("pgid = %d, want > 0", pgid)
	}
	if !IsAlive(pgid) {
		t.Error("process should be alive after spawn")
	}

	// Clean up.
	_ = Terminate(pgid, 2*time.Second)
}

func TestSpawn_WithEnv(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "env.log")

	env := append(os.Environ(), "PLAX_TEST_VAR=hello123")
	pgid, err := Spawn("envtest", "echo $PLAX_TEST_VAR", env, dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Give the echo a moment to write.
	time.Sleep(200 * time.Millisecond)

	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "hello123") {
		t.Errorf("log should contain custom env var value, got: %s", data)
	}

	_ = Terminate(pgid, 2*time.Second)
}

func TestSpawn_WithDir(t *testing.T) {
	workDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "pwd.log")

	pgid, err := Spawn("pwdtest", "pwd", os.Environ(), workDir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	data, _ := os.ReadFile(logPath)
	got := strings.TrimSpace(string(data))
	// Resolve symlinks (e.g. /tmp on macOS).
	wantResolved, _ := filepath.EvalSymlinks(workDir)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("pwd = %q, want %q", got, workDir)
	}

	_ = Terminate(pgid, 2*time.Second)
}

func TestSpawn_LogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hello.log")

	pgid, err := Spawn("echotest", "echo hello-plax", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "hello-plax") {
		t.Errorf("log should contain output, got: %s", data)
	}

	_ = Terminate(pgid, 2*time.Second)
}

func TestSpawn_ProcessGroup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "pg.log")

	// Spawn a shell that forks a child. Both should be in the same group.
	pgid, err := Spawn("pgtest", "sleep 60 & sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// pgid should equal the PID of the shell we started.
	if !IsAlive(pgid) {
		t.Error("process group should be alive")
	}

	// Verify the group leader is pgid.
	pid := pgid
	actualPgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if actualPgid != pgid {
		t.Errorf("Getpgid(%d) = %d, want %d", pid, actualPgid, pgid)
	}

	_ = Terminate(pgid, 2*time.Second)
}

func TestTerminate_Graceful(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "term.log")

	pgid, err := Spawn("termtest", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if !IsAlive(pgid) {
		t.Fatal("process should be alive before terminate")
	}

	if err := Terminate(pgid, 2*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	if IsAlive(pgid) {
		t.Error("process should be dead after terminate")
	}
}

func TestTerminate_ChildrenKilled(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "children.log")

	// Spawn a parent that creates a child. Both should die.
	pgid, err := Spawn("childtest", "sleep 300 & sleep 300", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := Terminate(pgid, 3*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	// Give the SIGKILL a moment to propagate.
	time.Sleep(100 * time.Millisecond)

	if IsAlive(pgid) {
		t.Error("process group should be dead after terminate")
	}
}

func TestTerminate_AlreadyDead(t *testing.T) {
	// Use a PID that's very unlikely to exist.
	err := Terminate(999999, 100*time.Millisecond)
	if err != nil {
		t.Errorf("Terminate on dead process should be no-op: %v", err)
	}
}

func TestIsAlive_Running(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "alive.log")

	pgid, err := Spawn("alivetest", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if !IsAlive(pgid) {
		t.Error("IsAlive should return true for running process")
	}

	_ = Terminate(pgid, 2*time.Second)
}

func TestIsAlive_Dead(t *testing.T) {
	if IsAlive(999999) {
		t.Error("IsAlive should return false for nonexistent pgid")
	}
}
