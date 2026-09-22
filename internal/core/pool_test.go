package core

import (
	"io"
	"log"
	"testing"
	"time"
)

func newTestPool(states ...*ProxyState) *ProxyPool {
	return NewProxyPool(log.New(io.Discard, "", 0), states)
}

func candidateKeys(candidates []ProxyCandidate) []string {
	keys := make([]string, 0, len(candidates))
	for _, c := range candidates {
		keys = append(keys, c.Key)
	}
	return keys
}

func TestPoolCandidatesReturnEveryProxy(t *testing.T) {
	pool := newTestPool(
		proxyStateURL(t, "a", "http://1.1.1.1:80"),
		proxyStateURL(t, "b", "http://2.2.2.2:80"),
		proxyStateURL(t, "c", "http://3.3.3.3:80"),
	)

	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		candidates := pool.Candidates(time.Now(), "example.com")
		if len(candidates) != 3 {
			t.Fatalf("got %d candidates, want 3", len(candidates))
		}
		for _, key := range candidateKeys(candidates) {
			seen[key] = true
		}
	}

	for _, key := range []string{"a", "b", "c"} {
		if !seen[key] {
			t.Fatalf("proxy %q never returned", key)
		}
	}
}

func TestPoolCandidatesPutFailedHostLast(t *testing.T) {
	pool := newTestPool(
		proxyStateURL(t, "a", "http://1.1.1.1:80"),
		proxyStateURL(t, "b", "http://2.2.2.2:80"),
		proxyStateURL(t, "c", "http://3.3.3.3:80"),
	)
	pool.failedByHost = map[string]map[string]time.Time{
		"example.com": {"a": time.Now()},
	}

	candidates := pool.Candidates(time.Now(), "example.com")
	if len(candidates) != 3 {
		t.Fatalf("got %d candidates, want 3", len(candidates))
	}
	if candidates[2].Key != "a" {
		t.Fatalf("candidates = %v, want failed proxy last", candidateKeys(candidates))
	}
}

func TestPoolFailureExpiresAfterTTL(t *testing.T) {
	pool := newTestPool(
		proxyStateURL(t, "a", "http://1.1.1.1:80"),
		proxyStateURL(t, "b", "http://2.2.2.2:80"),
	)
	pool.failedByHost = map[string]map[string]time.Time{
		"example.com": {"a": time.Now().Add(-FailureTTL - time.Minute)},
	}

	for i := 0; i < 20; i++ {
		if pool.Candidates(time.Now(), "example.com")[0].Key == "a" {
			return
		}
	}
	t.Fatal("expired failure still forced proxy to the back")
}

func TestPoolMarkSuccessClearsFailure(t *testing.T) {
	pool := newTestPool(proxyStateURL(t, "a", "http://1.1.1.1:80"))
	pool.MarkFailure("a", "example.com")
	if _, ok := pool.failedByHost["example.com"]["a"]; !ok {
		t.Fatal("MarkFailure did not record failure")
	}

	pool.MarkSuccess("a", "example.com")
	if _, ok := pool.failedByHost["example.com"]["a"]; ok {
		t.Fatal("MarkSuccess did not clear failure")
	}
	if pool.failCounts["a"] != 0 {
		t.Fatalf("failCounts = %v, want cleared", pool.failCounts)
	}
}

func TestPoolReplaceResetsFailures(t *testing.T) {
	pool := newTestPool(proxyStateURL(t, "a", "http://1.1.1.1:80"))
	pool.MarkFailure("a", "example.com")

	pool.Replace([]*ProxyState{
		proxyStateURL(t, "b", "http://2.2.2.2:80"),
		proxyStateURL(t, "c", "http://3.3.3.3:80"),
	})

	if pool.Count() != 2 {
		t.Fatalf("Count = %d, want 2", pool.Count())
	}
	if len(pool.failedByHost) != 0 {
		t.Fatalf("failedByHost = %v, want empty", pool.failedByHost)
	}
}

func TestPoolSetPrimaryPrepends(t *testing.T) {
	pool := newTestPool(proxyStateURL(t, "b", "http://2.2.2.2:80"))
	pool.SetPrimary(proxyStateURL(t, "a", "http://1.1.1.1:80"))

	if pool.Count() != 2 {
		t.Fatalf("Count = %d, want 2", pool.Count())
	}
	if pool.proxies[0].Key != "a" {
		t.Fatalf("first proxy = %q, want a", pool.proxies[0].Key)
	}
}

func TestPoolTunnelCount(t *testing.T) {
	withTunnel := proxyStateURL(t, "a", "http://1.1.1.1:80")
	withTunnel.Tunnel = &Dialer{targetPool: 4}

	pool := newTestPool(withTunnel, proxyStateURL(t, "b", "http://2.2.2.2:80"))

	if got := pool.TunnelCount(); got != 4 {
		t.Fatalf("TunnelCount = %d, want 4", got)
	}
	if got := pool.UsableCount(); got != 1 {
		t.Fatalf("UsableCount = %d, want 1", got)
	}
}
