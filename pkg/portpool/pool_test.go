package portpool

import (
	"net"
	"strconv"
	"testing"

	"github.com/apollopower/plax/pkg/registry"
)

func TestAllocate_FirstFree(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := New(3000, 3010, reg)
	port, err := p.Allocate("i1", "app")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port != 3000 {
		t.Errorf("expected first port 3000, got %d", port)
	}
}

func TestAllocate_SkipsAllocated(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{3000: {Instance: "i1", Service: "app"}},
	}
	p := New(3000, 3010, reg)
	port, err := p.Allocate("i2", "web")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port != 3001 {
		t.Errorf("expected 3001 (skip allocated 3000), got %d", port)
	}
}

func TestAllocate_Exhausted(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	// Allocate ports 3000-3009 (10 ports)
	for i := 3000; i < 3010; i++ {
		_, err := New(3000, 3009, reg).Allocate("i1", "svc")
		if err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}
	// Should fail now
	_, err := New(3000, 3009, reg).Allocate("i2", "svc")
	if err == nil {
		t.Fatal("expected error for exhausted pool")
	}
}

func TestAllocate_OSProbe(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:3099")
	if err != nil {
		t.Skipf("cannot bind test port: %v", err)
	}
	defer func() { _ = ln.Close() }()

	p := New(3099, 3109, reg)
	port, err := p.Allocate("i1", "app")
	if err != nil {
		t.Fatalf("Allocate should skip OS-bound 3099: %v", err)
	}
	if port != 3100 {
		t.Errorf("expected 3100 (skip OS-bound 3099), got %d", port)
	}
}

func TestRelease_ReturnsPort(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{3000: {Instance: "i1", Service: "app"}},
	}
	p := New(3000, 3010, reg)

	p.Release(3000)
	if _, exists := reg.PortAllocations[3000]; exists {
		t.Error("port should be released")
	}

	port, err := p.Allocate("i2", "web")
	if err != nil {
		t.Fatalf("should re-allocate released port: %v", err)
	}
	if port != 3000 {
		t.Errorf("expected 3000 (released), got %d", port)
	}
}

func TestReserve_Success(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := New(3000, 4000, reg)
	if err := p.Reserve(3050, "i1", "app"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, exists := reg.PortAllocations[3050]; !exists {
		t.Error("reserved port should be in allocations")
	}
}

func TestReserve_Taken(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		PortAllocations: map[int]registry.PortAllocation{3050: {Instance: "i1", Service: "app"}},
	}
	p := New(3000, 4000, reg)
	if err := p.Reserve(3050, "i2", "web"); err == nil {
		t.Fatal("expected error for reserved port")
	}
}

func TestReserve_OSBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:3098")
	if err != nil {
		t.Skipf("cannot bind test port: %v", err)
	}
	defer func() { _ = ln.Close() }()

	reg := &registry.Registry{
		Version:         1,
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := New(3000, 4000, reg)
	if err := p.Reserve(3098, "i1", "app"); err == nil {
		t.Fatal("expected error for OS-bound port")
	}
}

func TestAllocate_DefaultRange(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	// start=0,end=0 should use defaults (3000-4000)
	p := New(0, 0, reg)
	port, err := p.Allocate("i1", "app")
	if err != nil {
		t.Fatalf("Allocate with defaults: %v", err)
	}
	if port != 3000 {
		t.Errorf("expected 3000 (default start), got %d", port)
	}
}

func TestAllocate_CachesPort(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := New(5000, 5010, reg)
	port, err := p.Allocate("i1", "svc")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	alloc, exists := reg.PortAllocations[port]
	if !exists {
		t.Fatal("port should be in registry allocations")
	}
	if alloc.Instance != "i1" || alloc.Service != "svc" {
		t.Errorf("wrong allocation: %+v", alloc)
	}
}

func TestPortFree_RealPort(t *testing.T) {
	if !portFree(getFreePort(t)) {
		t.Error("portFree should return true for free port")
	}
}

func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot get free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return port
}
