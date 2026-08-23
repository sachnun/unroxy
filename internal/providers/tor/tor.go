// Package tor implements an experimental Tor provider built on gonion, a
// pure-Go Tor client: it bootstraps a consensus through directory
// authorities, maintains a small pool of pre-built 3-hop circuits and
// exposes them as dedicated named pools:
//
//	/tor       (or auth user "tor")      all circuits, random exit
//	/tor/{CC}  (or auth user "tor/DE")   circuits whose exit is in CC
//
// The implementation is experimental: gonion is unaudited and its API is
// unstable.
package tor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	gonion "github.com/robogg133/gonion"
	"github.com/robogg133/gonion/pkg/common"
	"github.com/robogg133/gonion/pkg/path"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

const (
	circuitCount = 8
	circuitHops  = 3

	dialTimeout    = 15 * time.Second
	buildTimeout   = 90 * time.Second
	probeTimeout   = 45 * time.Second
	rebuildTimeout = 90 * time.Second
	// identityTimeout bounds the full HTTPS egress lookup through the 3-hop
	// tunnel; core's fixed 8s egressTimeout is too tight for Tor.
	identityTimeout = 60 * time.Second

	// exitPort is the port used for exit-relay selection. Most exits allow
	// both 80 and 443; 443 matches the bulk of real traffic.
	exitPort uint16 = 443

	// circuitPriority deprioritizes Tor circuits below list-based providers
	// so the slower multi-hop tunnels only carry traffic when others fail.
	circuitPriority = 10
)

// dirAuthorities are Tor v3 directory authorities (IP:ORPort), used only
// for the initial consensus bootstrap before any relay is known.
var dirAuthorities = []string{
	"86.59.21.38:443",    // tor26
	"45.66.33.45:443",    // dizum
	"131.188.40.189:443", // gabelmoo
	"193.23.244.244:443", // dannenberg
	"171.25.193.9:80",    // maatuska
	"199.58.81.140:443",  // longclaw
	"204.13.164.118:443", // bastet
	"128.31.0.39:9101",   // moria1
}

// Provider starts the Tor circuit pools.
type Provider struct {
	mu       sync.Mutex
	started  bool
	logger   *log.Logger
	host     *providers.Host
	all      *core.ProxyPool
	byCC     map[string]*core.ProxyPool
	circuits []*managedCircuit
}

type managedCircuit struct {
	cp *circuitProxy
	ps *core.ProxyState
}

func init() {
	providers.Register(&Provider{})
}

func (p *Provider) Name() string { return "Tor" }

func (p *Provider) Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	go p.start(context.WithoutCancel(ctx), host, logger)
	return nil
}

func (p *Provider) start(ctx context.Context, host *providers.Host, logger *log.Logger) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.logger = logger
	p.host = host
	p.byCC = make(map[string]*core.ProxyPool)
	p.mu.Unlock()

	if common.GetGlobalConsensus() == nil {
		if err := bootstrapConsensus(logger); err != nil {
			logger.Printf("Tor: bootstrap failed: %v", err)
			return
		}
	}
	logger.Printf("Tor: consensus loaded (%d relays)", len(common.GetGlobalConsensus().RelayInformation))

	// Build and validate circuits in parallel.
	ch := make(chan *managedCircuit, circuitCount)
	var wg sync.WaitGroup
	for i := range circuitCount {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			ch <- p.buildCircuit(ctx, key, logger)
		}(fmt.Sprintf("circuit-%d", i))
	}
	wg.Wait()
	close(ch)

	circuits := make([]*managedCircuit, 0, circuitCount)
	for m := range ch {
		if m != nil {
			circuits = append(circuits, m)
		}
	}
	if len(circuits) == 0 {
		logger.Printf("Tor: no circuits could be established")
		return
	}

	states := make([]*core.ProxyState, 0, len(circuits))
	for _, m := range circuits {
		states = append(states, m.ps)
	}

	p.mu.Lock()
	p.circuits = circuits
	p.registerLocked(states)
	p.mu.Unlock()

	logger.Printf("Tor: %d circuits ready, path /tor or auth user \"tor\"%s",
		len(circuits), regionSummary(circuits))
}

// buildCircuit ensures a live 3-hop tunnel and validates it end-to-end.
// Returns nil if the circuit cannot be built or does not relay traffic.
func (p *Provider) buildCircuit(ctx context.Context, key string, logger *log.Logger) *managedCircuit {
	cp := &circuitProxy{}

	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	err := cp.ensure(bctx)
	cancel()
	if err != nil {
		cp.close()
		logger.Printf("Tor [%s] build failed: %v", key, err)
		return nil
	}

	ps := &core.ProxyState{
		Key:         "tor://" + key,
		URL:         &url.URL{Scheme: "tor", Host: key},
		DialContext: cp.DialContext,
		// The default validator probe enforces a 3s deadline, far too
		// tight for a 3-hop Tor tunnel; use a patient custom probe.
		ProbeFunc: cp.probe,
		Priority:  circuitPriority,
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	_, ok := core.ProbeProxy(pctx, ps)
	cancel()
	if !ok {
		cp.close()
		logger.Printf("Tor [%s] probe failed", key)
		return nil
	}
	if ps.Country == "" {
		ictx, cancel := context.WithTimeout(ctx, identityTimeout)
		if ip, cc, err := cp.resolveExitIdentity(ictx); err == nil {
			ps.IP, ps.Country, ps.CountryVerified = ip, cc, true
		}
		cancel()
	}
	return &managedCircuit{cp: cp, ps: ps}
}

// registerLocked wires up the TOR and TOR/{CC} named pools. Called with
// p.mu held.
func (p *Provider) registerLocked(states []*core.ProxyState) {
	p.all = core.NewProxyPool(p.logger, states)
	p.host.AddNamed("TOR", p.all, core.NewRotatingProxyTransport(p.all))

	groups := groupByExitCountry(states)
	for cc, group := range groups {
		pool := core.NewProxyPool(p.logger, group)
		p.byCC[cc] = pool
		p.host.AddNamed("TOR/"+cc, pool, core.NewRotatingProxyTransport(pool))
	}
}

// Refresh periodically rebuilds dead circuits so the pools recover without
// a process restart.
func (p *Provider) Refresh(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || len(p.circuits) == 0 {
		return nil
	}

	healthy := make([]*core.ProxyState, 0, len(p.circuits))
	rebuilt := 0
	resolved := 0
	for _, m := range p.circuits {
		if !m.cp.alive() {
			bctx, cancel := context.WithTimeout(ctx, rebuildTimeout)
			err := m.cp.ensure(bctx)
			cancel()
			if err != nil {
				continue
			}
			rebuilt++
		}
		// Circuits whose exit country is still unknown get another patient
		// identity resolution attempt each refresh cycle.
		if m.ps.Country == "" {
			ictx, cancel := context.WithTimeout(ctx, identityTimeout)
			if ip, cc, err := m.cp.resolveExitIdentity(ictx); err == nil {
				m.ps.IP, m.ps.Country, m.ps.CountryVerified = ip, cc, true
				resolved++
			}
			cancel()
		}
		healthy = append(healthy, m.ps)
	}
	if rebuilt > 0 {
		p.logger.Printf("Tor: rebuilt %d circuit(s)", rebuilt)
	}
	if resolved > 0 {
		p.logger.Printf("Tor: resolved %d exit identit(y/ies)", resolved)
	}
	if len(healthy) == 0 {
		return nil
	}

	p.all.Replace(healthy)
	groups := groupByExitCountry(healthy)
	for cc, pool := range p.byCC {
		pool.Replace(groups[cc])
	}
	// Register pools for newly identified countries.
	for cc := range groups {
		if _, ok := p.byCC[cc]; ok {
			continue
		}
		pool := core.NewProxyPool(p.logger, groups[cc])
		p.byCC[cc] = pool
		p.host.AddNamed("TOR/"+cc, pool, core.NewRotatingProxyTransport(pool))
		p.logger.Printf("Tor: new region pool /tor/%s", cc)
	}
	return nil
}

func groupByExitCountry(states []*core.ProxyState) map[string][]*core.ProxyState {
	groups := make(map[string][]*core.ProxyState)
	for _, ps := range states {
		cc := strings.ToUpper(strings.TrimSpace(ps.Country))
		if len(cc) != 2 {
			continue
		}
		groups[cc] = append(groups[cc], ps)
	}
	return groups
}

func regionSummary(circuits []*managedCircuit) string {
	counts := make(map[string]int)
	for _, m := range circuits {
		cc := strings.ToUpper(strings.TrimSpace(m.ps.Country))
		if len(cc) != 2 {
			cc = "??"
		}
		counts[cc]++
	}
	parts := make([]string, 0, len(counts))
	for cc, n := range counts {
		parts = append(parts, fmt.Sprintf("%s(%d)", cc, n))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return ", exits: " + strings.Join(parts, " ")
}

// bootstrapConsensus dials directory authorities until one serves a
// consensus that gonion can parse and apply globally.
func bootstrapConsensus(logger *log.Logger) error {
	var lastErr error
	for _, addr := range dirAuthorities {
		raw, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		conn, err := gonion.NewConn(raw, io.Discard, false)
		if err != nil {
			raw.Close()
			lastErr = err
			continue
		}
		err = gonion.BootstrapOneConn(conn)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		logger.Printf("Tor: bootstrapped consensus via %s", addr)
		return nil
	}
	return fmt.Errorf("all authorities failed: %w", lastErr)
}

// circuitProxy owns one pre-built 3-hop circuit and rebuilds it lazily when
// the circuit or its guard connection dies.
type circuitProxy struct {
	mu   sync.Mutex
	conn *gonion.Conn
	circ *gonion.Circuit
}

// resolveExitIdentity determines the observed egress IP and its country by
// requesting an IP echo endpoint through the tunnel, then resolving the
// country directly (non-proxied). Uses Tor-friendly timeouts.
func (c *circuitProxy) resolveExitIdentity(ctx context.Context) (ip, cc string, err error) {
	client := &http.Client{
		Timeout: identityTimeout,
		Transport: &http.Transport{
			DialContext:           c.DialContext,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	var doc struct {
		IP string `json:"ip"`
	}
	err = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if err != nil {
		return "", "", err
	}
	if doc.IP == "" {
		return "", "", fmt.Errorf("empty egress IP")
	}

	geoClient := core.NewProviderHTTPClient()
	gresp, err := geoClient.Get("https://ipwho.is/" + doc.IP)
	if err != nil {
		return doc.IP, "", err
	}
	defer gresp.Body.Close()
	var gdoc struct {
		CountryCode string `json:"country_code"`
	}
	if err := json.NewDecoder(gresp.Body).Decode(&gdoc); err != nil {
		return doc.IP, "", err
	}
	cc = strings.ToUpper(strings.TrimSpace(gdoc.CountryCode))
	if len(cc) != 2 {
		return doc.IP, "", fmt.Errorf("invalid country code %q", gdoc.CountryCode)
	}
	return doc.IP, cc, nil
}

// probe exercises the circuit with a real HTTP request against the shared
// control target, mirroring core.ProbeProxy but with Tor-friendly timings.
func (c *circuitProxy) probe(ctx context.Context) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	start := time.Now()
	conn, err := c.DialContext(ctx, "tcp", core.ProbeTarget)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	deadline := start.Add(probeTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	host, _, err := net.SplitHostPort(core.ProbeTarget)
	if err != nil {
		host = core.ProbeTarget
	}
	req := fmt.Sprintf("GET /cdn-cgi/trace HTTP/1.1\r\nHost: %s\r\nUser-Agent: unroxy-probe\r\nConnection: close\r\n\r\n", host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, err
	}
	buf := make([]byte, 512)
	n := 0
	for !bytes.Contains(buf[:n], []byte("HTTP/")) {
		var k int
		k, err = conn.Read(buf[n:])
		if k > 0 {
			n += k
		}
		if err != nil || n >= len(buf) {
			break
		}
	}
	if n == 0 {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if !bytes.Contains(buf[:n], []byte("HTTP/")) {
		return 0, fmt.Errorf("tor probe: non-HTTP reply")
	}
	return time.Since(start), nil
}

// DialContext dials addr through the circuit, rebuilding it once on failure.
func (c *circuitProxy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	stream, err := c.dialLocked(ctx, addr)
	if err == nil {
		return stream, nil
	}
	// The circuit or guard conn is likely dead; rebuild once and retry.
	c.resetLocked()
	stream, err = c.dialLocked(ctx, addr)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// alive reports whether the current circuit still has a live guard conn.
func (c *circuitProxy) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.circ != nil && c.conn != nil && c.conn.Context().Err() == nil
}

// ensure builds or rebuilds the circuit under the lock.
func (c *circuitProxy) ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureLocked(ctx)
}

func (c *circuitProxy) dialLocked(ctx context.Context, addr string) (net.Conn, error) {
	if err := c.ensureLocked(ctx); err != nil {
		return nil, err
	}
	return c.circ.Dial(addr)
}

// ensureLocked reconnects the guard connection and builds a fresh random
// 3-hop path if no live circuit exists.
func (c *circuitProxy) ensureLocked(ctx context.Context) error {
	if c.circ != nil && c.conn != nil && c.conn.Context().Err() == nil {
		return nil
	}
	c.resetLocked()

	cns := common.GetGlobalConsensus()
	if cns == nil {
		return fmt.Errorf("no consensus")
	}

	sel := path.New(cns, false)
	if err := sel.SelectRandomCircuit(circuitHops, exitPort); err != nil {
		return fmt.Errorf("select path: %w", err)
	}
	relays := sel.Circuit()
	guard := relays[0]

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(guard.Ipv4Addr, fmt.Sprintf("%d", guard.ORPort)))
	if err != nil {
		return fmt.Errorf("dial guard %s (%s): %w", guard.Nickname, guard.Ipv4Addr, err)
	}
	conn, err := gonion.NewConn(raw, io.Discard, false)
	if err != nil {
		raw.Close()
		return fmt.Errorf("link handshake with %s: %w", guard.Nickname, err)
	}
	c.conn = conn

	circ, err := conn.BuildPath(1, relays)
	if err != nil {
		conn.Close()
		c.conn = nil
		return fmt.Errorf("build path via %s: %w", guard.Nickname, err)
	}
	c.circ = circ
	return nil
}

func (c *circuitProxy) resetLocked() {
	if c.circ != nil {
		c.circ.Close()
		c.circ = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *circuitProxy) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetLocked()
}
