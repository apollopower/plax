//go:build unix

package registry

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRegistry_OpenHoldsFileLock(t *testing.T) {
	path := tempPath(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	lockP := lockPath(path)
	fd, err := syscall.Open(lockP, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		t.Error("expected lock to be held by Open, but lock was not exclusive")
	}
}

func TestRegistry_OpenReleaseAllowsLock(t *testing.T) {
	path := tempPath(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Close()

	lockP := lockPath(path)
	fd, err := syscall.Open(lockP, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatalf("lock should be available after Close: %v", err)
	}
}

func TestRegistry_OpenCreateDirBeforeLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "dir", "registry.json")

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open with nested dirs: %v", err)
	}
	r.Close()

	if _, err := os.Stat(lockPath(path)); err != nil {
		t.Errorf("lock file should exist: %v", err)
	}
}
