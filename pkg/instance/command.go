package instance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

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

// RunCommand executes command through the user's shell in worktreePath with
// the instance environment. It returns captured output on success and
// includes command output in errors on failure.
func RunCommand(ctx context.Context, worktreePath string, ports map[string]int, command string) (CommandResult, error) {
	if strings.TrimSpace(command) == "" {
		return CommandResult{}, fmt.Errorf("instance command: empty command")
	}

	envList, err := env.LoadInstanceEnv(worktreePath, ports)
	if err != nil {
		return CommandResult{}, err
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = worktreePath
	cmd.Env = envList

	tail := &tailWriter{}
	cmd.Stdout = io.MultiWriter(os.Stderr, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CommandResult{}, ctxErr
		}
		out := strings.TrimSpace(tail.String())
		if out == "" {
			return CommandResult{}, err
		}
		return CommandResult{}, fmt.Errorf("%s: %w", out, err)
	}
	return CommandResult{Output: tail.String()}, nil
}
