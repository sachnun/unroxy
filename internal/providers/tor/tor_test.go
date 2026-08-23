package tor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robogg133/gonion/pkg/common"
	"github.com/robogg133/gonion/pkg/path"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

func requireConsensus(t *testing.T) {
	t.Helper()
	if common.GetGlobalConsensus() != nil && len(common.GetGlobalConsensus().RelayInformation) > 100 {
		return
	}
	if err := bootstrapConsensus(testLogger(t)); err != nil {
		t.Fatalf("bootstrap consensus: %v", err)
	}
	cns := common.GetGlobalConsensus()
	if cns == nil {
		t.Fatal("no global consensus after bootstrap")
	}
	if len(cns.RelayInformation) <= 100 {
		t.Fatalf("consensus has too few relays: %d", len(cns.RelayInformation))
	}
}

func testLogger(t *testing.T) *log.Logger {
	return log.New(testWriter{t}, "", 0)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestBootstrapConsensus(t *testing.T) {
	requireConsensus(t)
}

// TestCircuitEnsure builds a 3-hop circuit end-to-end.
func TestCircuitEnsure(t *testing.T) {
	requireConsensus(t)

	cp := &circuitProxy{}
	defer cp.close()

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	if err := cp.ensure(ctx); err != nil {
		t.Fatalf("ensure circuit: %v", err)
	}
	if cp.circ == nil || cp.conn == nil {
		t.Fatal("circuit or conn not set after ensure")
	}
}

// TestDialContextHTTP performs real HTTP requests through the exit relay.
func TestDialContextHTTP(t *testing.T) {
	requireConsensus(t)

	cp := &circuitProxy{}
	defer cp.close()

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext:           cp.DialContext,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 45 * time.Second,
		},
	}

	for _, target := range []string{"http://check.torproject.org/", "http://example.com/"} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", target, resp.StatusCode)
		}
		t.Logf("GET %s -> %d, body head: %.80s", target, resp.StatusCode, body[:n])
	}
}

// TestProbeProxy verifies integration with unroxy's validator probe.
func TestProbeProxy(t *testing.T) {
	requireConsensus(t)

	cp := &circuitProxy{}
	defer cp.close()

	ps := &core.ProxyState{
		Key:         "tor://test",
		DialContext: cp.DialContext,
		ProbeFunc:   cp.probe,
	}
	var lat time.Duration
	var ok bool
	var lastErr error
	for attempt := range 3 {
		lat, ok = core.ProbeProxy(context.Background(), ps)
		if ok {
			break
		}
		// Surface the underlying dial/relay error.
		pctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		_, lastErr = cp.DialContext(pctx, "tcp", core.ProbeTarget)
		cancel()
		t.Logf("attempt %d failed: %v", attempt+1, lastErr)
	}
	if !ok {
		t.Fatal("ProbeProxy failed")
	}
	t.Logf("probe ok, latency=%s country=%s ip=%s isp=%s", lat, ps.Country, ps.IP, ps.ISP)
}

// TestRegisterNamedPools verifies the TOR and TOR/{CC} pool layout without
// touching the network.
func TestRegisterNamedPools(t *testing.T) {
	var p Provider
	p.logger = log.New(io.Discard, "", 0)
	p.host = providers.NewHost(nil)
	p.byCC = make(map[string]*core.ProxyPool)

	states := []*core.ProxyState{
		{Key: "tor://a", Country: "de", URL: &url.URL{Scheme: "tor", Host: "circuit-a"}}, // lower case must normalize
		{Key: "tor://b", Country: "AT", URL: &url.URL{Scheme: "tor", Host: "circuit-b"}},
		{Key: "tor://c", URL: &url.URL{Scheme: "tor", Host: "circuit-c"}}, // no country -> only in TOR
	}
	p.mu.Lock()
	p.registerLocked(states)
	p.mu.Unlock()

	names := map[string]bool{}
	for _, n := range p.host.Router().Names() {
		names[n] = true
	}
	for _, want := range []string{"TOR", "TOR/DE", "TOR/AT"} {
		if !names[want] {
			t.Fatalf("named pool %q not registered; got %v", want, names)
		}
	}

	if got := p.all.Count(); got != len(states) {
		t.Fatalf("TOR pool has %d proxies, want %d", got, len(states))
	}
	if got := p.byCC["DE"].Count(); got != 1 {
		t.Fatalf("TOR/DE pool has %d proxies, want 1", got)
	}
}

// TestCircuitRebuild kills the live circuit and verifies the next dial
// rebuilds it transparently.
func TestCircuitRebuild(t *testing.T) {
	requireConsensus(t)

	cp := &circuitProxy{}
	defer cp.close()

	// Build the initial circuit with retries: relay selection on the live
	// Tor network is inherently flaky, and the provider tolerates that via
	// lazy rebuilds and Refresh.
	var ensureErr error
	for attempt := range 3 {
		bctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		ensureErr = cp.ensure(bctx)
		cancel()
		if ensureErr == nil {
			break
		}
		t.Logf("ensure attempt %d failed: %v", attempt+1, ensureErr)
	}
	if ensureErr != nil {
		t.Fatalf("initial ensure: %v", ensureErr)
	}

	// Simulate circuit death.
	if err := cp.circ.Close(); err != nil {
		t.Fatalf("close circuit: %v", err)
	}

	conn, err := cp.DialContext(context.Background(), "tcp", "1.1.1.1:80")
	if err != nil {
		t.Fatalf("rebuild dial: %v", err)
	}
	conn.Close()

	cp.mu.Lock()
	alive := cp.circ != nil && cp.conn != nil && cp.conn.Context().Err() == nil
	cp.mu.Unlock()
	if !alive {
		t.Fatal("circuit not rebuilt")
	}
}

// TestExitCountryCoverage samples random 3-hop paths from the live
// consensus, resolves each exit relay's country via ipwho.is and reports
// the distribution. This shows which /tor/{CC} pools are realistically
// reachable.
func TestExitCountryCoverage(t *testing.T) {
	requireConsensus(t)

	const samples = 40

	cns := common.GetGlobalConsensus()
	t.Logf("consensus relays: %d", len(cns.RelayInformation))

	exits := make(map[string]bool) // exit IP -> seen
	for range samples {
		sel := path.New(cns, false)
		if err := sel.SelectRandomCircuit(circuitHops, exitPort); err != nil {
			continue
		}
		relays := sel.Circuit()
		if len(relays) == 0 || relays[len(relays)-1].Ipv4Addr == "" {
			continue
		}
		exits[relays[len(relays)-1].Ipv4Addr] = true
	}
	if len(exits) == 0 {
		t.Fatal("no exit relays sampled")
	}
	t.Logf("sampled %d paths -> %d unique exit IPs", samples, len(exits))

	type result struct{ cc string }
	ch := make(chan result, len(exits))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 15 * time.Second}
	for ip := range exits {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := client.Get("https://ipwho.is/" + ip)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var doc struct {
				CountryCode string `json:"country_code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || len(doc.CountryCode) != 2 {
				return
			}
			ch <- result{cc: strings.ToUpper(doc.CountryCode)}
		}(ip)
	}
	wg.Wait()
	close(ch)

	counts := map[string]int{}
	for r := range ch {
		counts[r.cc]++
	}
	parts := make([]string, 0, len(counts))
	for cc, n := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", cc, n))
	}
	sort.Strings(parts)
	t.Logf("exit countries (%d distinct): %v", len(counts), parts)

	if len(counts) < 3 {
		t.Fatalf("expected coverage of at least 3 countries, got %d", len(counts))
	}
}

// TestExitIPInventory enumerates the live consensus directly: total relays,
// unique exit IPs and how many allow port 443. It then uniformly samples
// exit IPs and geo-resolves them to show the true country spread (unlike
// bandwidth-weighted path selection, which concentrates on big relays).
func TestExitIPInventory(t *testing.T) {
	requireConsensus(t)

	cns := common.GetGlobalConsensus()

	total := len(cns.RelayInformation)
	ips := make(map[string]bool)
	exits := make(map[string]bool)
	for i := range cns.RelayInformation {
		rs := &cns.RelayInformation[i]
		if rs.Ipv4Addr == "" {
			continue
		}
		ips[rs.Ipv4Addr] = true
		if rs.Ports.IsAllowed(443) || rs.Ports.IsAllowed(80) {
			exits[rs.Ipv4Addr] = true
		}
	}
	t.Logf("total relays=%d, unique IPv4=%d, exit-capable IPs (80/443)=%d",
		total, len(ips), len(exits))

	if len(exits) < 500 {
		t.Fatalf("expected >=500 exit IPs in consensus, got %d", len(exits))
	}

	// Uniform sample of exit IPs -> geo lookup.
	list := make([]string, 0, len(exits))
	for ip := range exits {
		list = append(list, ip)
	}
	const sampleN = 120
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	if len(list) > sampleN {
		list = list[:sampleN]
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	counts := map[string]int{}
	ch := make(chan struct{})
	sem := make(chan struct{}, 10)
	client := &http.Client{Timeout: 15 * time.Second}
	for _, ip := range list {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := client.Get("https://ipwho.is/" + ip)
			if err != nil {
				ch <- struct{}{}
				return
			}
			defer resp.Body.Close()
			var doc struct {
				CountryCode string `json:"country_code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&doc); err == nil && len(doc.CountryCode) == 2 {
				mu.Lock()
				counts[strings.ToUpper(doc.CountryCode)]++
				mu.Unlock()
			}
			ch <- struct{}{}
		}(ip)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	for range ch {
	}

	parts := make([]string, 0, len(counts))
	for cc, n := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", cc, n))
	}
	sort.Strings(parts)
	t.Logf("uniform sample %d exit IPs -> %d distinct countries: %v",
		len(list), len(counts), parts)

	if len(counts) < 20 {
		t.Fatalf("expected >=20 distinct countries across exit IPs, got %d", len(counts))
	}
}
