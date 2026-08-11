package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "registry.json")
}

func openNew(t *testing.T, fn func(r *Registry)) {
	t.Helper()
	path := tempPath(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fn(r)
}

func TestOpen_NewFile(t *testing.T) {
	r, err := Open(tempPath(t))
	if err != nil {
		t.Fatalf("Open on new file: %v", err)
	}
	if len(r.Instances) != 0 {
		t.Errorf("expected empty instances, got %d", len(r.Instances))
	}
}

func TestOpen_ExistingFile(t *testing.T) {
	path := tempPath(t)
	r1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r1.Instances["i1"] = InstanceRecord{ID: "i1", State: StateRunning, CreatedAt: time.Now()}
	r1.PortAllocations[3000] = PortAllocation{Instance: "i1", Service: "app"}
	if err := r1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r2, err := Open(path)
	if err != nil {
		t.Fatalf("Open existing: %v", err)
	}
	rec, ok := r2.GetInstance("i1")
	if !ok {
		t.Fatal("expected i1 to exist")
	}
	if rec.State != StateRunning {
		t.Errorf("expected running, got %s", rec.State)
	}
	if len(r2.PortAllocations) != 1 {
		t.Errorf("expected 1 port allocation, got %d", len(r2.PortAllocations))
	}
}

func TestOpen_UnsupportedVersion(t *testing.T) {
	path := tempPath(t)
	data := `{"version":2,"instances":{}}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("expected unsupported version error, got %v", err)
	}
}

func TestState_Constants(t *testing.T) {
	if StateRunning != "running" {
		t.Errorf("StateRunning = %q, want running", StateRunning)
	}
	if StateSuspended != "suspended" {
		t.Errorf("StateSuspended = %q, want suspended", StateSuspended)
	}
	var s State = "running"
	if s != StateRunning {
		t.Error("State type should be comparable with constants")
	}
}

func TestOpen_InvalidJSON(t *testing.T) {
	path := tempPath(t)
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSave_ReadBack(t *testing.T) {
	openNew(t, func(r1 *Registry) {
		_ = r1.AddInstance("i2", InstanceRecord{ID: "i2", Branch: "feat/x", State: StateSuspended})
		if err := r1.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		r2, err := Open(r1.path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		rec, ok := r2.GetInstance("i2")
		if !ok {
			t.Fatal("expected i2")
		}
		if rec.Branch != "feat/x" || rec.State != StateSuspended {
			t.Errorf("round trip lost fields: %+v", rec)
		}
	})
}

func TestSave_AtomicWrite(t *testing.T) {
	openNew(t, func(r *Registry) {
		if err := r.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		dir := filepath.Dir(r.path)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				t.Error("tmp file should have been removed after rename")
			}
		}
		if _, err := os.Stat(r.path); err != nil {
			t.Errorf("target file should exist: %v", err)
		}
	})
}

func TestAddInstance_Duplicate(t *testing.T) {
	openNew(t, func(r *Registry) {
		r.Instances["i1"] = InstanceRecord{ID: "i1"}
		err := r.AddInstance("i1", InstanceRecord{ID: "i1"})
		if err == nil {
			t.Fatal("expected error for duplicate instance")
		}
	})
}

func TestRemoveInstance_NotFound(t *testing.T) {
	openNew(t, func(r *Registry) {
		err := r.RemoveInstance("nonexistent")
		if err == nil {
			t.Fatal("expected error for removing nonexistent instance")
		}
	})
}

func TestRemoveInstance_CleansPorts(t *testing.T) {
	openNew(t, func(r *Registry) {
		_ = r.AddInstance("i1", InstanceRecord{ID: "i1"})
		_ = r.AddInstance("i2", InstanceRecord{ID: "i2"})
		_ = r.AllocPort(3000, "i1", "app")
		_ = r.AllocPort(3001, "i1", "redis")
		_ = r.AllocPort(4000, "i2", "app")

		if err := r.RemoveInstance("i1"); err != nil {
			t.Fatal(err)
		}
		if len(r.PortAllocations) != 1 {
			t.Errorf("expected 1 remaining allocation, got %d", len(r.PortAllocations))
		}
		if _, exists := r.PortAllocations[4000]; !exists {
			t.Error("i2's port allocation should remain")
		}
	})
}

func TestAllocPort_Duplicate(t *testing.T) {
	openNew(t, func(r *Registry) {
		_ = r.AllocPort(3000, "i1", "app")
		err := r.AllocPort(3000, "i2", "db")
		if err == nil {
			t.Fatal("expected error for duplicate port allocation")
		}
	})
}

func TestReleasePort_NotAllocated(t *testing.T) {
	openNew(t, func(r *Registry) {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("ReleasePort panicked: %v", rec)
			}
		}()
		r.ReleasePort(9999)
	})
}

func TestGetInstance_NotFound(t *testing.T) {
	openNew(t, func(r *Registry) {
		_, ok := r.GetInstance("nonexistent")
		if ok {
			t.Fatal("expected false for nonexistent instance")
		}
	})
}

func TestGetInstance_Found(t *testing.T) {
	openNew(t, func(r *Registry) {
		_ = r.AddInstance("x", InstanceRecord{ID: "x", State: "running"})
		rec, ok := r.GetInstance("x")
		if !ok {
			t.Fatal("expected true")
		}
		if rec.State != "running" {
			t.Errorf("expected running, got %s", rec.State)
		}
	})
}

func TestDBNamesFromRecord_WithDBNames(t *testing.T) {
	rec := InstanceRecord{
		DBName:  "plax_old",
		DBNames: map[string]string{"": "plax_i1", "test": "plax_i1_test"},
	}
	names := DBNamesFromRecord(rec)
	if len(names) != 2 {
		t.Fatalf("got %d names, want 2: %v", len(names), names)
	}
	hasPrimary, hasTest := false, false
	for _, n := range names {
		if n == "plax_i1" {
			hasPrimary = true
		}
		if n == "plax_i1_test" {
			hasTest = true
		}
	}
	if !hasPrimary || !hasTest {
		t.Errorf("missing names: got %v", names)
	}
}

func TestDBNamesFromRecord_OldFormatFallback(t *testing.T) {
	rec := InstanceRecord{DBName: "plax_legacy"}
	names := DBNamesFromRecord(rec)
	if len(names) != 1 || names[0] != "plax_legacy" {
		t.Errorf("got %v, want [plax_legacy]", names)
	}
}

func TestDBNamesFromRecord_Empty(t *testing.T) {
	names := DBNamesFromRecord(InstanceRecord{})
	if names != nil {
		t.Errorf("got %v, want nil", names)
	}
}
