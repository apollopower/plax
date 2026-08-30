package instance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/apollopower/plax/pkg/derive/env"
)

// CommandResult describes one completed instance command.
type CommandResult struct {
	Output string
}

// outputLimit caps the retained command output. Output streams to the
// caller's stderr in full; the retained tail exists only for failure
// diagnostics, so a broken migration cannot consume unbounded memory.
const outputLimit = 8 * 1024

// tailWriter keeps only the last outputLimit bytes written to it.
type tailWriter struct {
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > outputLimit {
		w.buf = w.buf[len(w.buf)-outputLimit:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	return string(w.buf)
}

// RunCommand executes command through the user's shell in the instance
// worktree (workdir relative to worktreePath, matching seed.workdir
// semantics) with the instance environment. It returns captured output on
// success and includes command output in errors on failure.
func RunCommand(ctx context.Context, worktreePath string, workdir string, ports map[string]int, command string) (CommandResult, error) {
	if strings.TrimSpace(command) == "" {
		return CommandResult{}, fmt.Errorf("instance command: empty command")
	}

	// A blueprint without env.template has no derived .env at all. Fail
	// with the real cause instead of the generic "was the instance created
	// with 'plax up'?" message from LoadInstanceEnv.
	if _, err := os.Stat(filepath.Join(worktreePath, ".env")); err != nil {
		if os.IsNotExist(err) {
			return CommandResult{}, fmt.Errorf("instance command: no derived .env at %s — the blueprint's env.template is required to derive an instance environment", worktreePath)
		}
		return CommandResult{}, fmt.Errorf("instance command: stat .env: %w", err)
	}

	envList, err := env.LoadInstanceEnv(worktreePath, ports)
	if err != nil {
		return CommandResult{}, err
	}

	// A host DATABASE_URL must not redirect an instance command to a
	// non-instance database: keep the variable only when the derived .env
	// actually defines it. The base path pins it for the same reason.
	derived, err := env.ParseFile(filepath.Join(worktreePath, ".env"))
	if err != nil {
		return CommandResult{}, err
	}
	if _, ok := derived["DATABASE_URL"]; !ok {
		envList = stripEnv(envList, "DATABASE_URL")
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = filepath.Join(worktreePath, workdir)
	cmd.Env = envList

	// Own process group so cancellation can kill the whole tree. Killing
	// only sh would leave its children holding the output pipes open and
	// block Wait on EOF.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	tail := &tailWriter{}
	cmd.Stdout = io.MultiWriter(os.Stderr, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	if err := cmd.Start(); err != nil {
		return CommandResult{}, fmt.Errorf("instance command: start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			return CommandResult{Output: tail.String()}, nil
		}
		out := strings.TrimSpace(tail.String())
		if out == "" {
			return CommandResult{}, err
		}
		return CommandResult{}, fmt.Errorf("%s: %w", out, err)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return CommandResult{}, ctx.Err()
	}
}

// stripEnv removes every KEY= entry from env.
func stripEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}
