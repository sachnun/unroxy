package main

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

type proxyState struct {
	key         string
	url         *url.URL
	country     string
	latency     time.Duration
	healthy     bool
	lastChecked time.Time
	priority    int
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	psiphon     *PsiphonDialer
}

type proxyCandidate struct {
	key         string
	url         *url.URL
	country     string
	latency     time.Duration
	priority    int
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	psiphon     *PsiphonDialer
}

type ProxyPool struct {
	logger *log.Logger

	mu           sync.RWMutex
	proxies      []*proxyState
	failedByHost map[string]map[string]time.Time
}

func NewProxyPool(logger *log.Logger, proxies []*proxyState) *ProxyPool {
	if logger == nil {
		logger = log.Default()
	}

	return &ProxyPool{
		logger:  logger,
		proxies: cloneProxyStates(proxies),
	}
}

func groupProxiesByCountry(proxies []*proxyState) map[string][]*proxyState {
	groups := make(map[string][]*proxyState)
	for _, p := range proxies {
		code := p.country
		if code == "" {
			code = "XX"
		}
		groups[code] = append(groups[code], p)
	}
	return groups
}

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []proxyCandidate {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.proxies) == 0 {
		return nil
	}

	rotationKey := strings.ToLower(strings.TrimSpace(targetHost))
	failedKeys := p.failedByHost[rotationKey]

	ready := make([]proxyCandidate, 0, len(p.proxies))
	failed := make([]proxyCandidate, 0, len(p.proxies))

	for _, state := range p.proxies {
		if state == nil || state.url == nil {
			continue
		}

		candidate := proxyCandidate{
			key:         state.key,
			url:         cloneURL(state.url),
			country:     state.country,
			latency:     state.latency,
			priority:    state.priority,
			dialContext: state.dialContext,
			psiphon:     state.psiphon,
		}

		if failedAt, isFailed := failedKeys[state.key]; isFailed && time.Since(failedAt) < failureTTL {
			failed = append(failed, candidate)
		} else {
			delete(failedKeys, state.key)
			ready = append(ready, candidate)
		}
	}

	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].priority != ready[j].priority {
			return ready[i].priority < ready[j].priority
		}
		return ready[i].latency < ready[j].latency
	})
	sort.SliceStable(failed, func(i, j int) bool {
		if failed[i].priority != failed[j].priority {
			return failed[i].priority < failed[j].priority
		}
		return failed[i].latency < failed[j].latency
	})

	ordered := make([]proxyCandidate, 0, len(p.proxies))
	ordered = append(ordered, ready...)
	ordered = append(ordered, failed...)
	return ordered
}

func (p *ProxyPool) MarkSuccess(key, targetHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.key != key {
			continue
		}

		state.healthy = true
		state.lastChecked = time.Now()
		delete(p.failedByHost[strings.ToLower(strings.TrimSpace(targetHost))], key)
		return
	}
}

func (p *ProxyPool) MarkFailure(key, targetHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.key != key {
			continue
		}

		state.healthy = false
		state.lastChecked = time.Now()
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

func (p *ProxyPool) Replace(proxies []*proxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.proxies = cloneProxyStates(proxies)
	p.failedByHost = nil
}

func (p *ProxyPool) SetPrimary(primary *proxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cp := *primary
	cp.priority = 0
	p.proxies = append([]*proxyState{&cp}, p.proxies...)
}

func cloneProxyStates(proxies []*proxyState) []*proxyState {
	if len(proxies) == 0 {
		return nil
	}

	cloned := make([]*proxyState, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || proxy.url == nil {
			continue
		}

		state := *proxy
		state.url = cloneURL(proxy.url)
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
