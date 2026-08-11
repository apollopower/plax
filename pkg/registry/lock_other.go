//go:build !unix

package registry

import "fmt"

type fileLock struct{}

func lockFile(path string, shared bool) (*fileLock, error) {
	return &fileLock{}, nil
}

func unlockFile(l *fileLock) {
}

// lockPath is defined in registry.go (single definition for all platforms).
