package upgrade

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// AtomicReplace replaces the file at dest with the file at src via
// rename(2). src must be a sibling of dest so both live on one filesystem.
// Renaming over a running executable succeeds on Linux and macOS, which is
// what makes replacing the currently-running binary safe: an in-place write
// would hit ETXTBSY on a truncated, executing file.
//
// When rename fails with a cross-device error (src arrived on another
// filesystem despite the caller's intent), it falls back to copying src's
// content into a fresh sibling temp file and renaming that. dest's existing
// mode is preserved; src is removed either way.
func AtomicReplace(src, dest string) error {
	mode := fs.FileMode(0o755)
	if info, err := os.Stat(dest); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Normalize the source's mode before rename so a temp-file source (0600
	// from CreateTemp) lands with the target's mode, not the temp's.
	if err := os.Chmod(src, mode); err != nil {
		return err
	}

	if err := os.Rename(src, dest); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("renaming %s onto %s: %w", src, dest, err)
	}

	return replaceCopy(src, dest, mode)
}

// replaceCopy is the cross-device fallback: copy src's bytes into a temp
// file next to dest, then rename it into place.
func replaceCopy(src, dest string, mode fs.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".plax-replace-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("renaming %s onto %s: %w", tmp.Name(), dest, err)
	}
	return os.Remove(src)
}
