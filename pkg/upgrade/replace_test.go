package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpgrade_AtomicReplace_SwapsFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	dest := filepath.Join(dir, "plax")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(src, dest); err != nil {
		t.Fatalf("AtomicReplace: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("dest content = %q, want new", data)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still exists after replace")
	}
	newInfo, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Fatal("dest inode unchanged — replacement must be a new inode")
	}
	if newInfo.Mode().Perm() != oldInfo.Mode().Perm() {
		t.Fatalf("dest mode = %o, want %o (preserved)", newInfo.Mode().Perm(), oldInfo.Mode().Perm())
	}
}

func TestUpgrade_AtomicReplace_ExistingModeKept(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	dest := filepath.Join(dir, "plax")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(src, dest); err != nil {
		t.Fatalf("AtomicReplace: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dest mode = %o, want 700", info.Mode().Perm())
	}
}

func TestUpgrade_AtomicReplace_MissingDest_Gets755(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	dest := filepath.Join(dir, "plax")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(src, dest); err != nil {
		t.Fatalf("AtomicReplace: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dest mode = %o, want 755", info.Mode().Perm())
	}
}

func TestUpgrade_AtomicReplace_SrcMissing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "plax")
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(filepath.Join(dir, "nope"), dest); err == nil {
		t.Fatal("AtomicReplace = nil error, want error")
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Fatalf("dest content = %q, want old (untouched)", data)
	}
}

func TestUpgrade_ReplaceCopy_CrossDeviceFallback(t *testing.T) {
	// replaceCopy is the fallback AtomicReplace uses when rename(2) crosses
	// devices — simulated here directly, since CI cannot cheaply fabricate a
	// second filesystem.
	dirA := t.TempDir()
	dirB := t.TempDir()
	src := filepath.Join(dirA, "new-binary")
	dest := filepath.Join(dirB, "plax")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := replaceCopy(src, dest, 0o700); err != nil {
		t.Fatalf("replaceCopy: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("dest content = %q, want new", data)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("src still exists after replaceCopy")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dest mode = %o, want 700", info.Mode().Perm())
	}
}
