package core

import (
	"context"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProxyState struct {
	Key         string
	URL         *url.URL
	Country     string
	Latency     time.Duration
	Healthy     bool
	LastChecked time.Time
	Priority    int
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Psiphon     *PsiphonDialer
}

type ProxyCandidate struct {
	Key         string
	URL         *url.URL
	Country     string
	Latency     time.Duration
	Priority    int
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Psiphon     *PsiphonDialer
}

type ProxyPool struct {
	logger *log.Logger

	mu           sync.RWMutex
	proxies      []*ProxyState
	failedByHost map[string]map[string]time.Time
}

func NewProxyPool(logger *log.Logger, proxies []*ProxyState) *ProxyPool {
	if logger == nil {
		logger = log.Default()
	}

	return &ProxyPool{
		logger:  logger,
		proxies: cloneProxyStates(proxies),
	}
}

func GroupProxiesByCountry(proxies []*ProxyState) map[string][]*ProxyState {
	groups := make(map[string][]*ProxyState)
	for _, p := range proxies {
		code := p.Country
		if code == "" {
			code = "XX"
		}
		groups[code] = append(groups[code], p)
	}
	return groups
}

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []ProxyCandidate {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.proxies) == 0 {
		return nil
	}

	rotationKey := strings.ToLower(strings.TrimSpace(targetHost))

	// Snapshot the failed set: the shared failedByHost map is never
	// mutated while holding only the read lock (concurrent deletes would
	// race and can panic "concurrent map writes").
	failedSet := make(map[string]bool, len(p.failedByHost[rotationKey]))
	for key, failedAt := range p.failedByHost[rotationKey] {
		if time.Since(failedAt) < FailureTTL {
			failedSet[key] = true
		}
	}

	ready := make([]ProxyCandidate, 0, len(p.proxies))
	failed := make([]ProxyCandidate, 0, len(p.proxies))

	for _, state := range p.proxies {
		if state == nil || state.URL == nil {
			continue
		}

		candidate := ProxyCandidate{
			Key:         state.Key,
			URL:         cloneURL(state.URL),
			Country:     state.Country,
			Latency:     state.Latency,
			Priority:    state.Priority,
			DialContext: state.DialContext,
			Psiphon:     state.Psiphon,
		}

		if failedSet[state.Key] {
			failed = append(failed, candidate)
		} else {
			ready = append(ready, candidate)
		}
	}

	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority < ready[j].Priority
		}
		return ready[i].Latency < ready[j].Latency
	})
	sort.SliceStable(failed, func(i, j int) bool {
		if failed[i].Priority != failed[j].Priority {
			return failed[i].Priority < failed[j].Priority
		}
		return failed[i].Latency < failed[j].Latency
	})

	ordered := make([]ProxyCandidate, 0, len(p.proxies))
	ordered = append(ordered, ready...)
	ordered = append(ordered, failed...)
	return ordered
}

func (p *ProxyPool) MarkSuccess(key, targetHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.Key != key {
			continue
		}

		state.Healthy = true
		state.LastChecked = time.Now()
		delete(p.failedByHost[strings.ToLower(strings.TrimSpace(targetHost))], key)
		return
	}
}

func (p *ProxyPool) MarkFailure(key, targetHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.Key != key {
			continue
		}

		state.Healthy = false
		state.LastChecked = time.Now()
		rotationKey := strings.ToLower(strings.TrimSpace(targetHost))
		if rotationKey != "" {
			if p.failedByHost == nil {
				p.failedByHost = make(map[string]map[string]time.Time)
			}
			if p.failedByHost[rotationKey] == nil {
				p.failedByHost[rotationKey] = make(map[string]time.Time)
			}
			p.failedByHost[rotationKey][key] = time.Now()
		}
		return
	}
}

func (p *ProxyPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

func (p *ProxyPool) Replace(proxies []*ProxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.proxies = cloneProxyStates(proxies)
	p.failedByHost = nil
}

func (p *ProxyPool) SetPrimary(primary *ProxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cp := *primary
	cp.Priority = 0
	p.proxies = append([]*ProxyState{&cp}, p.proxies...)
}

func cloneProxyStates(proxies []*ProxyState) []*ProxyState {
	if len(proxies) == 0 {
		return nil
	}

	cloned := make([]*ProxyState, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || proxy.URL == nil {
			continue
		}

		state := *proxy
		state.URL = cloneURL(proxy.URL)
		cloned = append(cloned, &state)
	}

	return cloned
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}

	cloned := *u
	return &cloned
}
