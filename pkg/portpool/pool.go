// Package portpool allocates ports for services from a configurable range.
//
// All port state is owned by a single allocator goroutine. Requests arrive
// over a channel; replies return over per-request channels. This ensures
// concurrent Allocate, Release, and Reserve calls never race on the shared
// PortAllocations map.
package portpool

import (
	"fmt"
	"sync"

	"github.com/apollopower/plax/pkg/registry"
)

const (
	defaultStart = 3000
	defaultEnd   = 4000
)

type portOp int

const (
	opAllocate portOp = iota
	opRelease
)

type portRequest struct {
	op       portOp
	instance string
	service  string
	port     int
	reply    chan portResult
}

type portResult struct {
	port int
	err  error
}

type PortPool struct {
	reqCh    chan portRequest
	done     chan struct{}
	once     sync.Once
	start    int
	end      int
	registry *registry.Registry
}

func New(start, end int, reg *registry.Registry) (*PortPool, error) {
	if start == 0 && end == 0 {
		start = defaultStart
		end = defaultEnd
	}
	if start < 1024 {
		return nil, fmt.Errorf("portpool: start %d must be >= 1024", start)
	}
	if end > 65535 {
		return nil, fmt.Errorf("portpool: end %d must be <= 65535", end)
	}
	if start >= end {
		return nil, fmt.Errorf("portpool: start %d must be < end %d", start, end)
	}

	p := &PortPool{
		reqCh:    make(chan portRequest),
		done:     make(chan struct{}),
		start:    start,
		end:      end,
		registry: reg,
	}
	go p.run()
	return p, nil
}

func (p *PortPool) run() {
	for {
		select {
		case req := <-p.reqCh:
			switch req.op {
			case opAllocate:
				port, err := p.allocate(req.instance, req.service)
				req.reply <- portResult{port, err}
			case opRelease:
				p.registry.ReleasePort(req.port)
				req.reply <- portResult{}
			}
		case <-p.done:
			return
		}
	}
}

func (p *PortPool) allocate(instance, service string) (int, error) {
	for port := p.start; port <= p.end; port++ {
		if _, exists := p.registry.PortAllocations[port]; exists {
			continue
		}
		if !ProbeFree(port) {
			continue
		}
		if err := p.registry.AllocPort(port, instance, service); err != nil {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("portpool: no free port in range %d-%d", p.start, p.end)
}

func (p *PortPool) Allocate(instance, service string) (int, error) {
	reply := make(chan portResult, 1)
	p.reqCh <- portRequest{op: opAllocate, instance: instance, service: service, reply: reply}
	r := <-reply
	return r.port, r.err
}

func (p *PortPool) Release(port int) {
	reply := make(chan portResult, 1)
	p.reqCh <- portRequest{op: opRelease, port: port, reply: reply}
	<-reply
}

func (p *PortPool) Close() {
	p.once.Do(func() {
		close(p.done)
	})
}
