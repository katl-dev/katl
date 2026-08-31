package apiproxy

import (
	"errors"
	"slices"
	"sync"
	"time"
)

var ErrNoBackend = errors.New("no eligible API backend")

type BackendStatus struct {
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Local       bool      `json:"local,omitempty"`
	Eligible    bool      `json:"eligible"`
	Reason      string    `json:"reason,omitempty"`
	LastChecked time.Time `json:"lastChecked,omitempty"`
}

type pool struct {
	mu       sync.RWMutex
	backends []BackendStatus
	next     int
}

func newPool(backends []Backend) *pool {
	statuses := make([]BackendStatus, len(backends))
	for i, backend := range backends {
		statuses[i] = BackendStatus{
			Name: backend.Name, Address: backend.Address, Local: backend.Local,
			Reason: "not checked",
		}
	}
	return &pool{backends: statuses}
}

func (p *pool) update(name string, eligible bool, reason string, checked time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.backends {
		if p.backends[i].Name != name {
			continue
		}
		p.backends[i].Eligible = eligible
		p.backends[i].Reason = reason
		p.backends[i].LastChecked = checked
		return
	}
}

func (p *pool) selectBackend(excluded map[string]struct{}) (BackendStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, backend := range p.backends {
		if backend.Local && backend.Eligible {
			if _, skip := excluded[backend.Name]; !skip {
				return backend, nil
			}
		}
	}
	if len(p.backends) == 0 {
		return BackendStatus{}, ErrNoBackend
	}
	for offset := 0; offset < len(p.backends); offset++ {
		index := (p.next + offset) % len(p.backends)
		backend := p.backends[index]
		if backend.Local || !backend.Eligible {
			continue
		}
		if _, skip := excluded[backend.Name]; skip {
			continue
		}
		p.next = (index + 1) % len(p.backends)
		return backend, nil
	}
	return BackendStatus{}, ErrNoBackend
}

func (p *pool) snapshot() []BackendStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.backends)
}
