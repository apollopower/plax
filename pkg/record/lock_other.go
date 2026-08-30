//go:build !unix

package record

// recLock is a no-op on unsupported platforms, mirroring the registry
// package's tolerance: plax's process management is Unix-only anyway.
type recLock struct{}

func acquireLock(path string, shared bool) (*recLock, error) {
	return &recLock{}, nil
}

func (l *recLock) close() {}
