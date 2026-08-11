//go:build !unix

package registry

import "fmt"

type fileLock struct{}

func lockFile(path string, shared bool) (*fileLock, error) {
	return &fileLock{}, nil
}

func unlockFile(l *fileLock) {
}

func lockPath(path string) string {
	return path + ".lock"
}
