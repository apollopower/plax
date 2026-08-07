package process

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSpawn_Success(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	pgid, startTime, err := Spawn("sleeper", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if pgid <= 0 {
		t.Fatalf("pgid = %d, want > 0", pgid)
	}
	if runtime.GOOS == "linux" && startTime == 0 {
		t.Error("startTime should be nonzero on linux")
	}
	if !IsAlive(pgid) {
		t.Error("process should be alive after spawn")
	}

	// Clean up.
	_ = Terminate(pgid, startTime, 2*time.Second)
}

func TestSpawn_WithEnv(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "env.log")

	env := append(os.Environ(), "PLAX_TEST_VAR=hello123")
	pgid, _, err := Spawn("envtest", "echo $PLAX_TEST_VAR", env, dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Give the echo a moment to write.
	time.Sleep(200 * time.Millisecond)

	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "hello123") {
		t.Errorf("log should contain custom env var value, got: %s", data)
	}

	_ = Terminate(pgid, 0, 2*time.Second)
}

func TestSpawn_WithDir(t *testing.T) {
	workDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "pwd.log")

	pgid, _, err := Spawn("pwdtest", "pwd", os.Environ(), workDir, logPath)
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

	_ = Terminate(pgid, 0, 2*time.Second)
}

func TestSpawn_LogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hello.log")

	pgid, _, err := Spawn("echotest", "echo hello-plax", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "hello-plax") {
		t.Errorf("log should contain output, got: %s", data)
	}

	_ = Terminate(pgid, 0, 2*time.Second)
}

func TestSpawn_ProcessGroup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "pg.log")

	// Spawn a shell that forks a child. Both should be in the same group.
	pgid, _, err := Spawn("pgtest", "sleep 60 & sleep 60", os.Environ(), dir, logPath)
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

	_ = Terminate(pgid, 0, 2*time.Second)
}

func TestTerminate_Graceful(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "term.log")

	pgid, startTime, err := Spawn("termtest", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if !IsAlive(pgid) {
		t.Fatal("process should be alive before terminate")
	}

	if err := Terminate(pgid, startTime, 2*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	if IsAlive(pgid) {
		t.Error("process should be dead after terminate")
	}
}

func TestTerminate_StalePGIDNotSignaled(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stale.log")

	pgid, startTime, err := Spawn("staletest", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if startTime == 0 {
		t.Skip("start time unavailable on this platform")
	}

	// A start time that does not match means the PGID was reused: Terminate
	// must refuse to signal it.
	err = Terminate(pgid, startTime+1, 100*time.Millisecond)
	if !errors.Is(err, ErrStaleProcess) {
		t.Errorf("Terminate with wrong startTime = %v, want ErrStaleProcess", err)
	}
	if !IsAlive(pgid) {
		t.Error("process must not be signaled when its recorded start time does not match")
	}

	// The correct start time still terminates it.
	if err := Terminate(pgid, startTime, 2*time.Second); err != nil {
		t.Fatalf("Terminate with correct startTime: %v", err)
	}
}

func TestTerminate_ChildrenKilled(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "children.log")

	// Spawn a parent that creates a child. Both should die.
	pgid, _, err := Spawn("childtest", "sleep 300 & sleep 300", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := Terminate(pgid, 0, 3*time.Second); err != nil {
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
	if err := Terminate(999999, 0, 100*time.Millisecond); err != nil {
		t.Errorf("Terminate on dead process should be no-op: %v", err)
	}
	// A recorded start time for a process that is gone is also a no-op.
	if err := Terminate(999999, 12345, 100*time.Millisecond); err != nil {
		t.Errorf("Terminate on dead process with startTime should be no-op: %v", err)
	}
}

func TestIsAlive_Running(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "alive.log")

	pgid, _, err := Spawn("alivetest", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if !IsAlive(pgid) {
		t.Error("IsAlive should return true for running process")
	}

	_ = Terminate(pgid, 0, 2*time.Second)
}

func TestIsAlive_Dead(t *testing.T) {
	if IsAlive(999999) {
		t.Error("IsAlive should return false for nonexistent pgid")
	}
}

func TestStartTime_Running(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starttime.log")

	pgid, startTime, err := Spawn("starttimetest", "sleep 60", os.Environ(), dir, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if startTime == 0 {
		t.Skip("start time unavailable on this platform")
	}

	if got := StartTime(pgid); got != startTime {
		t.Errorf("StartTime(%d) = %d, want %d (stable while process lives)", pgid, got, startTime)
	}

	_ = Terminate(pgid, startTime, 2*time.Second)
}

func TestStartTime_Dead(t *testing.T) {
	if got := StartTime(999999); got != 0 {
		t.Errorf("StartTime(999999) = %d, want 0", got)
	}
}
