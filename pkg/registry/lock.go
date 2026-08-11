//go:build unix

package registry

import (
	"fmt"
	"os"
	"syscall"
)

type fileLock struct {
	f *os.File
}

func lockFile(path string, shared bool) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("registry: open lock file: %w", err)
	}
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("registry: acquire lock: %w", err)
	}
	return &fileLock{f: f}, nil
}

func unlockFile(l *fileLock) {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
