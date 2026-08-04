package registry

import (
	"os"
	"path/filepath"
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
	r1.Instances["i1"] = InstanceRecord{ID: "i1", State: "running", CreatedAt: time.Now()}
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
	if rec.State != "running" {
		t.Errorf("expected running, got %s", rec.State)
	}
	if len(r2.PortAllocations) != 1 {
		t.Errorf("expected 1 port allocation, got %d", len(r2.PortAllocations))
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
		_ = r1.AddInstance("i2", InstanceRecord{ID: "i2", Branch: "feat/x", State: "suspended"})
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
		if rec.Branch != "feat/x" || rec.State != "suspended" {
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
