// Package process manages native process lifecycle for Plax instances:
// spawning detached process groups, terminating them, and checking liveness.
package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrStaleProcess marks a process group whose ID has been reused by an
// unrelated process since it was recorded. Signaling it would kill the
// wrong process, so callers must treat this as "already gone".
var ErrStaleProcess = errors.New("process: pgid reused by another process")

// ErrGroupSurvivors marks a process group whose leader has exited but
// whose members (children) are still alive. The children could not be
// terminated because they inherited a different parent and are no longer
// in the original process group.
var ErrGroupSurvivors = errors.New("process: group leader dead but children survive")

// Spawn starts command as a detached process group leader.
// The command runs in dir with the given environment (merged over os.Environ
// by the caller). Stdout and stderr are appended to logPath.
// Returns the process group ID (equals the leader's PID) and the leader's
// start time, which callers persist so a later Terminate can detect PID
// reuse. startTime is 0 on platforms without /proc.
//
// The process is NOT waited on — it outlives the plax command that spawned it.
func Spawn(name, command string, env []string, dir string, logPath string) (int, int64, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return 0, 0, fmt.Errorf("process: mkdir for log: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, 0, fmt.Errorf("process: open log %s: %w", logPath, err)
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
		return 0, 0, fmt.Errorf("process: spawn %s: %w", name, err)
	}

	pgid := cmd.Process.Pid
	startTime := StartTime(pgid)

	// Release the process so it doesn't become a zombie when it exits.
	// We don't wait on it — the process outlives this plax invocation.
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()

	return pgid, startTime, nil
}

// StartTime returns the process's start time (clock ticks since boot) from
// /proc/<pid>/stat. Two processes can share a PGID across a PID reuse, but
// not a (PGID, start time) pair, so this disambiguates them. Returns 0 when
// the process is gone or the platform has no /proc (e.g. macOS).
func StartTime(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// comm (field 2) is parenthesized and may contain spaces; everything we
	// need follows the last ')'.
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(s[idx+1:])
	// fields[0] is state (field 3); starttime is field 22 → index 19.
	if len(fields) < 20 {
		return 0
	}
	v, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// Terminate sends SIGTERM to the process group, waits for the timeout,
// then sends SIGKILL. No-op if the process group is already dead.
//
// startTime is the value recorded at Spawn time. When nonzero, the group is
// only signaled if its leader still has that start time; a mismatch returns
// ErrStaleProcess and nothing is signaled, so a reused PGID is never killed.
// Pass 0 on platforms without /proc to skip identity verification.
func leaderDead(pgid int, startTime int64) bool {
	return startTime != 0 && StartTime(pgid) != startTime
}

func Terminate(pgid int, startTime int64, timeout time.Duration) error {
	if pgid <= 0 {
		return fmt.Errorf("process: invalid pgid %d", pgid)
	}
	if startTime != 0 {
		switch cur := StartTime(pgid); cur {
		case 0:
			// Leader is dead but children may survive — check.
			if !IsAlive(pgid) {
				return nil
			}
			// Group still has members; fall through to SIGKILL.
		case startTime:
			// identity confirmed
		default:
			return ErrStaleProcess
		}
	} else if !IsAlive(pgid) {
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
		if !aliveAs(pgid, startTime) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Before SIGKILL, check whether the group leader has died. If the leader
	// is gone but the group is still alive, children survived — report it.
	if leaderDead(pgid, startTime) {
		if IsAlive(pgid) {
			return ErrGroupSurvivors
		}
		return nil
	}

	// The original process may have died and its PGID been reused while we
	// waited; never SIGKILL a group whose identity we cannot confirm.
	if !aliveAs(pgid, startTime) {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("process: sigkill pgid %d: %w", pgid, err)
	}

	return nil
}

// IsAlive reports whether a process group exists.
func IsAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	// Signal 0 checks for existence without sending a signal.
	err := syscall.Kill(-pgid, 0)
	return err == nil
}

// aliveAs reports whether the group exists and, when startTime is nonzero and
// the leader is still alive, still belongs to the process that was originally
// recorded. When the leader is dead but the group still has members (orphaned
// children), aliveAs falls back to IsAlive so the wait loop does not exit
// prematurely.
func aliveAs(pgid int, startTime int64) bool {
	if startTime == 0 {
		return IsAlive(pgid)
	}
	cur := StartTime(pgid)
	if cur == 0 {
		// Leader dead — check if the group still has members.
		return IsAlive(pgid)
	}
	return cur == startTime
}
