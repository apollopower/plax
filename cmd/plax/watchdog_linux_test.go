//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCLI_RunPlaxPackageWatchdogBoundary(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLI_RunPlaxWatchdogSupervisorChild$", "-test.timeout=0", "-test.v")
	cmd.Env = append(os.Environ(), "PLAX_TEST_WATCHDOG_DIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Only this disposable supervisor and its descendants share the group.
		return unix.Kill(-cmd.Process.Pid, unix.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		if cmd.Process != nil {
			if killErr := unix.Kill(-cmd.Process.Pid, unix.SIGKILL); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
				t.Error(killErr)
			}
		}
		t.Fatalf("supervisor exceeded independent parent watchdog: %v\n%s", err, out)
	}
	if err != nil {
		t.Fatalf("contained watchdog regression failed: %v\n%s", err, out)
	}
	for _, evidence := range []string{"readiness: runPlax worker", "Go package timeout after 3s", "child-cleanup marker absent", "parent recovery: worker stopped and reaped"} {
		if !strings.Contains(string(out), evidence) {
			t.Fatalf("missing watchdog evidence %q:\n%s", evidence, out)
		}
	}
	t.Log(strings.TrimSpace(string(out)))
}

func TestCLI_RunPlaxWatchdogSupervisorChild(t *testing.T) {
	dir := os.Getenv("PLAX_TEST_WATCHDOG_DIR")
	if dir == "" {
		t.Skip("child-only supervisor")
	}
	if unix.Getpgrp() != os.Getpid() {
		t.Fatal("supervisor must own its throwaway process group")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	signalCtx, stopSignals := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, 10*time.Second)
	defer cancel()
	fake := filepath.Join(dir, "fake-plax")
	const script = "#!/bin/sh\nPLAX_TEST_WATCHDOG_ROLE=worker exec \"$PLAX_TEST_WATCHDOG_BINARY\" -test.run=^TestCLI_RunPlaxWatchdogChild$ -test.timeout=0\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLI_RunPlaxWatchdogChild$", "-test.timeout=3s", "-test.v")
	cmd.Env = append(os.Environ(), "PLAX_TEST_WATCHDOG_ROLE=caller", "PLAX_TEST_WATCHDOG_BINARY="+os.Args[0])
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	var done chan error
	waited, recovered := false, false
	worker := 0
	recoverWorker := func() {
		if recovered {
			return
		}
		recovered = true
		// A group TERM also covers a worker that failed before writing readiness.
		// The supervisor catches it; no ordinary test runner is in this group.
		if err := unix.Kill(-os.Getpid(), unix.SIGTERM); err != nil {
			t.Error(err)
		}
		cancel()
		if done != nil && !waited {
			if err := <-done; err != nil {
				t.Logf("reaped caller during recovery: %v", err)
			}
			waited = true
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		reapedWorker := false
		for {
			var status unix.WaitStatus
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.ECHILD) {
				break
			}
			if err != nil {
				t.Error(err)
				break
			}
			if pid > 0 {
				if pid == worker {
					reapedWorker = true
				}
				continue
			}
			select {
			case <-cleanupCtx.Done():
				t.Error("owned descendants not reaped within recovery budget")
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		if worker > 0 {
			if !reapedWorker {
				t.Error("did not reap the adopted worker")
			}
			if err := unix.Kill(worker, 0); !errors.Is(err, unix.ESRCH) {
				t.Errorf("worker remains after recovery: %v", err)
			}
			if reapedWorker {
				t.Log("parent recovery: worker stopped and reaped")
			}
		}
	}
	// Defers run before stopSignals, including on a readiness assertion failure.
	defer recoverWorker()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done = make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	data := waitCLIFile(t, ctx, filepath.Join(dir, "worker"))
	var err error
	worker, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || worker <= 0 {
		t.Fatal("invalid owned worker PID")
	}
	group, err := unix.Getpgid(worker)
	if err != nil || group != os.Getpid() {
		t.Fatal("worker outside recorded supervisor group")
	}
	t.Logf("readiness: runPlax worker %d in owned group %d", worker, group)
	err = <-done
	waited = true
	if ctx.Err() != nil || err == nil || !strings.Contains(out.String(), "panic: test timed out after 3s") {
		t.Fatalf("expected child-package timeout, not supervisor termination: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "child-cleanup")); !os.IsNotExist(err) {
		t.Fatal("child cleanup marker unexpectedly present or unreadable")
	}
	var status unix.WaitStatus
	pid, err := unix.Wait4(worker, &status, unix.WNOHANG, nil)
	if err != nil || pid != 0 {
		t.Fatalf("expected a live adopted worker after caller timeout: pid=%d err=%v", pid, err)
	}
	t.Log("Go package timeout after 3s; child-cleanup marker absent; worker survived caller")
	recoverWorker()
}

func TestCLI_RunPlaxWatchdogChild(t *testing.T) {
	role := os.Getenv("PLAX_TEST_WATCHDOG_ROLE")
	if role == "" {
		t.Skip("child-only watchdog helper")
	}
	dir := os.Getenv("PLAX_TEST_WATCHDOG_DIR")
	if role == "worker" {
		if err := os.WriteFile(filepath.Join(dir, "worker"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
		}
		return
	}
	if role != "caller" {
		t.Fatal("unknown watchdog role")
	}
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(dir, "child-cleanup"), []byte("cleaned"), 0600); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := runPlax(filepath.Join(dir, "fake-plax"), dir); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fake CLI unexpectedly exited")
}
