package core

import (
	"context"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ProxyState struct {
	Key         string
	URL         *url.URL
	Country     string
	Priority    int
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Tunnel      *Dialer
	IP          string
	ISP         string
}

type ProxyCandidate struct {
	Key         string
	URL         *url.URL
	Country     string
	Priority    int
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Tunnel      *Dialer
	IP          string
	ISP         string
}

type ProxyPool struct {
	logger *log.Logger

	mu           sync.RWMutex
	proxies      []*ProxyState
	failedByHost map[string]map[string]time.Time

	failCounts map[string]int

	rotation atomic.Uint64
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

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []ProxyCandidate {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.proxies) == 0 {
		return nil
	}

	rotationKey := strings.ToLower(strings.TrimSpace(targetHost))

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
			Priority:    state.Priority,
			DialContext: state.DialContext,
			Tunnel:      state.Tunnel,
			IP:          state.IP,
			ISP:         state.ISP,
		}

		if failedSet[state.Key] {
			failed = append(failed, candidate)
		} else {
			ready = append(ready, candidate)
		}
	}

	sort.SliceStable(ready, func(i, j int) bool {
		return ready[i].Priority < ready[j].Priority
	})
	sort.SliceStable(failed, func(i, j int) bool {
		return failed[i].Priority < failed[j].Priority
	})

	if len(ready) > 1 {
		off := int(p.rotation.Add(1)) % len(ready)
		rotated := make([]ProxyCandidate, 0, len(ready))
		rotated = append(rotated, ready[off:]...)
		rotated = append(rotated, ready[:off]...)
		ready = rotated
	}

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

		delete(p.failedByHost[strings.ToLower(strings.TrimSpace(targetHost))], key)
		delete(p.failCounts, key)
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

		if p.failCounts == nil {
			p.failCounts = make(map[string]int)
		}
		p.failCounts[key]++
		break
	}
}

func (p *ProxyPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

func (p *ProxyPool) TunnelCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	tunnels := 0
	for _, state := range p.proxies {
		if state != nil && state.Tunnel != nil {
			tunnels += state.Tunnel.TargetPool()
		}
	}
	return tunnels
}

func (p *ProxyPool) Replace(proxies []*ProxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.proxies = cloneProxyStates(proxies)
	p.failedByHost = nil
	p.failCounts = nil
}

func (p *ProxyPool) SetPrimary(primary *ProxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, existing := range p.proxies {
		if existing != nil && existing.Key == primary.Key {
			return
		}
	}

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
