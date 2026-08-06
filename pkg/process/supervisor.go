// Package process manages native process lifecycle for Plax instances:
// spawning detached process groups, terminating them, and checking liveness.
package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Spawn starts command as a detached process group leader.
// The command runs in dir with the given environment (merged over os.Environ
// by the caller). Stdout and stderr are appended to logPath.
// Returns the process group ID (equals the leader's PID).
//
// The process is NOT waited on — it outlives the plax command that spawned it.
func Spawn(name, command string, env []string, dir string, logPath string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return 0, fmt.Errorf("process: mkdir for log: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, fmt.Errorf("process: open log %s: %w", logPath, err)
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	// Create a new process group so we can signal the entire tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return 0, fmt.Errorf("process: spawn %s: %w", name, err)
	}

	pgid := cmd.Process.Pid

	// Release the process so it doesn't become a zombie when it exits.
	// We don't wait on it — the process outlives this plax invocation.
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()

	return pgid, nil
}

// Terminate sends SIGTERM to the process group, waits for the timeout,
// then sends SIGKILL. No-op if the process group is already dead.
func Terminate(pgid int, timeout time.Duration) error {
	if !IsAlive(pgid) {
		return nil
	}

	// Signal the process group (negative PID).
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("process: sigterm pgid %d: %w", pgid, err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !IsAlive(pgid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Force kill.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("process: sigkill pgid %d: %w", pgid, err)
	}

	// Reap the leader to prevent zombies.
	// The Go runtime reaps children of this process; the group leader
	// was started by us so cmd.Wait() in Spawn handles it.
	return nil
}

// IsAlive reports whether a process group exists.
func IsAlive(pgid int) bool {
	// Signal 0 checks for existence without sending a signal.
	err := syscall.Kill(-pgid, 0)
	return err == nil
}
