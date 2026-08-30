package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeEnv writes a derived-style .env into dir for RunCommand tests.
func writeEnv(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRunCommand_UsesInstanceEnvAndWorktree(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "MY_VAR=derived\n")

	res, err := RunCommand(context.Background(), dir, map[string]int{"PORT_VAR": 4321},
		`printf 'var=%s port=%s cwd=%s' "$MY_VAR" "$PORT_VAR" "$(pwd)"`)
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	want := fmt.Sprintf("var=derived port=4321 cwd=%s", dir)
	if res.Output != want {
		t.Errorf("output = %q, want %q", res.Output, want)
	}
}

func TestRunCommand_StreamsOutputAndIncludesFailureOutput(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "A=1\n")

	_, err := RunCommand(context.Background(), dir, nil, `echo streamed-line; printf 'boom\n'; exit 3`)
	if err == nil {
		t.Fatal("RunCommand should fail on non-zero exit")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("err = %v, want wrapped exit code 3", err)
	}
	for _, want := range []string{"streamed-line", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestRunCommand_CancellationStopsProcess(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "A=1\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// A multi-command script forces sh to spawn sleep as a child rather
		// than exec-optimizing it away, so the whole process group must be
		// killed for Wait to return.
		_, err := RunCommand(ctx, dir, nil, "sleep 30; exit 0")
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCommand did not return after cancel")
	}
}

func TestRunCommand_RejectsEmptyCommand(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "A=1\n")

	if _, err := RunCommand(context.Background(), dir, nil, "  "); err == nil {
		t.Fatal("empty command should fail")
	}
}
