package portpool

import (
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/apollopower/plax/pkg/registry"
)

func newPool(t *testing.T, start, end int, reg *registry.Registry) *PortPool {
	t.Helper()
	p, err := New(start, end, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestPortPool_AllocateFirstFree(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := newPool(t, 3000, 3010, reg)
	port, err := p.Allocate("i1", "app")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port != 3000 {
		t.Errorf("expected first port 3000, got %d", port)
	}
}

func TestPortPool_AllocateSkipsAllocated(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{3000: {Instance: "i1", Service: "app"}},
	}
	p := newPool(t, 3000, 3010, reg)
	port, err := p.Allocate("i2", "web")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port != 3001 {
		t.Errorf("expected 3001 (skip allocated 3000), got %d", port)
	}
}

func TestPortPool_AllocateExhausted(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	// Allocate ports 3000-3009 (10 ports)
	for i := 3000; i < 3010; i++ {
		p := newPool(t, 3000, 3009, reg)
		_, err := p.Allocate("i1", "svc")
		if err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}
	// Should fail now
	p := newPool(t, 3000, 3009, reg)
	_, err := p.Allocate("i2", "svc")
	if err == nil {
		t.Fatal("expected error for exhausted pool")
	}
}

func TestPortPool_AllocateOSProbe(t *testing.T) {
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

	p := newPool(t, 3099, 3109, reg)
	port, err := p.Allocate("i1", "app")
	if err != nil {
		t.Fatalf("Allocate should skip OS-bound 3099: %v", err)
	}
	if port != 3100 {
		t.Errorf("expected 3100 (skip OS-bound 3099), got %d", port)
	}
}

func TestPortPool_ReleaseReturnsPort(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{3000: {Instance: "i1", Service: "app"}},
	}
	p := newPool(t, 3000, 3010, reg)

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

func TestPortPool_AllocateDefaultRange(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	// start=0,end=0 should use defaults (3000-4000)
	p := newPool(t, 0, 0, reg)
	port, err := p.Allocate("i1", "app")
	if err != nil {
		t.Fatalf("Allocate with defaults: %v", err)
	}
	if port != 3000 {
		t.Errorf("expected 3000 (default start), got %d", port)
	}
}

func TestPortPool_AllocateCachesPort(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := newPool(t, 5000, 5010, reg)
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

func TestPortPool_NewInvalidRange(t *testing.T) {
	reg := &registry.Registry{
		PortAllocations: map[int]registry.PortAllocation{},
	}
	_, err := New(5000, 4000, reg)
	if err == nil {
		t.Fatal("expected error for start >= end")
	}
}

func TestPortPool_NewBelow1024(t *testing.T) {
	reg := &registry.Registry{
		PortAllocations: map[int]registry.PortAllocation{},
	}
	_, err := New(100, 200, reg)
	if err == nil {
		t.Fatal("expected error for start < 1024")
	}
}

func TestPortPool_NewAbove65535(t *testing.T) {
	reg := &registry.Registry{
		PortAllocations: map[int]registry.PortAllocation{},
	}
	_, err := New(60000, 70000, reg)
	if err == nil {
		t.Fatal("expected error for end > 65535")
	}
}

func TestPortPool_NewDefaultRange(t *testing.T) {
	reg := &registry.Registry{
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p, err := New(0, 0, reg)
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if p.start != defaultStart || p.end != defaultEnd {
		t.Errorf("got %d-%d, want %d-%d", p.start, p.end, defaultStart, defaultEnd)
	}
}

func TestPortPool_ConcurrentAllocate(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := newPool(t, 3000, 3019, reg)

	type result struct {
		port int
		err  error
	}
	results := make(chan result, 20)
	for i := 0; i < 20; i++ {
		go func() {
			port, err := p.Allocate("conc", "svc")
			results <- result{port, err}
		}()
	}

	ports := map[int]bool{}
	for i := 0; i < 20; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("unexpected error: %v", r.err)
			continue
		}
		if ports[r.port] {
			t.Errorf("duplicate port %d", r.port)
		}
		ports[r.port] = true
	}
	if len(ports) != 20 {
		t.Errorf("expected 20 unique ports, got %d", len(ports))
	}
}

func TestPortPool_ConcurrentAllocateRelease(t *testing.T) {
	reg := &registry.Registry{
		Version:         1,
		Instances:       map[string]registry.InstanceRecord{},
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := newPool(t, 3000, 3049, reg)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := p.Allocate("conc", "svc")
			if err != nil {
				errs <- err
				return
			}
			p.Release(port)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("concurrent alloc/release: %v", err)
		}
	}
}

func TestPortPool_Close(t *testing.T) {
	reg := &registry.Registry{
		PortAllocations: map[int]registry.PortAllocation{},
	}
	p := newPool(t, 3000, 4000, reg)
	p.Close()
	// Closing again should not panic.
	p.Close()
}

func TestPortPool_ProbeFreeRealPort(t *testing.T) {
	if !ProbeFree(getFreePort(t)) {
		t.Error("ProbeFree should return true for free port")
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
