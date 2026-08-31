//go:build unix

package record

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type recLock struct {
	f *os.File
}

// acquireLock opens (creating if needed) the record's sibling lock file and
// takes an advisory flock. A process-local mutex is insufficient: separate
// plax invocations must not interleave writes, so the lock is on the file
// description, not in memory.
func acquireLock(path string, shared bool) (*recLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("record: open lock file: %w", err)
	}
	how := unix.LOCK_EX
	if shared {
		how = unix.LOCK_SH
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("record: acquire lock: %w", err)
	}
	return &recLock{f: f}, nil
}

func (l *recLock) close() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
}
