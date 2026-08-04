package portpool

import (
	"fmt"
	"net"
	"strconv"

	"github.com/apollopower/plax/pkg/registry"
)

type PortPool struct {
	start    int
	end      int
	registry *registry.Registry
}

func New(start, end int, reg *registry.Registry) *PortPool {
	return &PortPool{start: start, end: end, registry: reg}
}

func (p *PortPool) Allocate(instance, service string) (int, error) {
	// Sensible defaults for web/API services when the blueprint omits the pool range.
	if p.start == 0 {
		p.start = 3000
	}
	if p.end == 0 {
		p.end = 4000
	}

	for port := p.start; port <= p.end; port++ {
		if _, exists := p.registry.PortAllocations[port]; exists {
			continue
		}
		if !portFree(port) {
			continue
		}
		if err := p.registry.AllocPort(port, instance, service); err != nil {
			continue
		}
		return port, nil
	}

	return 0, fmt.Errorf("portpool: no free port in range %d-%d", p.start, p.end)
}

func (p *PortPool) Release(port int) {
	p.registry.ReleasePort(port)
}

func (p *PortPool) Reserve(port int, instance, service string) error {
	if _, exists := p.registry.PortAllocations[port]; exists {
		return fmt.Errorf("portpool: port %d already allocated", port)
	}
	if !portFree(port) {
		return fmt.Errorf("portpool: port %d is in use on the host", port)
	}
	return p.registry.AllocPort(port, instance, service)
}

func portFree(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
