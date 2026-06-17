package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
	"h12.io/socks"
)

const (
	proxyDialTimeout       = 5 * time.Second
	proxyHeaderTimeout     = 20 * time.Second
	proxyHealthTimeout     = 3 * time.Second
	providerFetchTimeout   = 30 * time.Second
	providerRefreshEvery   = 5 * time.Minute
	healthCheckConcurrency = 300
	failureTTL             = 10 * time.Minute
)

type ProxyProvider interface {
	Name() string
	Fetch() ([]*proxyState, error)
	ETag() (string, error)
}

var (
	proxiflyCSVURL     = "https://raw.githubusercontent.com/proxifly/free-proxy-list/refs/heads/main/proxies/all/data.csv"
	errNoUpstreamProxy = errors.New("no upstream proxies available")
)

type proxiflyProvider struct{}

func (p *proxiflyProvider) Name() string { return "Proxifly" }

func (p *proxiflyProvider) ETag() (string, error) {
	client := &http.Client{Timeout: providerFetchTimeout}
	req, err := http.NewRequest(http.MethodHead, proxiflyCSVURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	et := resp.Header.Get("ETag")
	if et == "" {
		return "", fmt.Errorf("no ETag for proxifly CSV")
	}
	return et, nil
}

func (p *proxiflyProvider) Fetch() ([]*proxyState, error) {
	return fetchProxiflyProxies()
}

func fetchProxiflyProxies() ([]*proxyState, error) {
	client := &http.Client{Timeout: providerFetchTimeout}

	resp, err := client.Get(proxiflyCSVURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("proxifly CSV returned status %d", resp.StatusCode)
	}

	reader := csv.NewReader(resp.Body)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to read proxifly CSV: %w", err)
	}

	states := make([]*proxyState, 0, len(records))
	for _, row := range records {
		if len(row) < 2 {
			continue
		}
		rawURL := strings.TrimSpace(row[0])
		country := strings.ToUpper(strings.TrimSpace(row[1]))
		if country == "" {
			country = "XX"
		}

		parsedURL, err := url.Parse(rawURL)
		if err != nil {
			continue
		}

		state := &proxyState{
			key:     rawURL,
			url:     parsedURL,
			country: country,
		}

		switch parsedURL.Scheme {
		case "socks5", "socks5h":
			d, err := proxy.FromURL(parsedURL, proxy.Direct)
			if err == nil {
				state.dialContext = d.(proxy.ContextDialer).DialContext
			}
		case "socks4", "socks4a":
			d := socks.Dial(rawURL)
			state.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				type dialResult struct {
					conn net.Conn
					err  error
				}
				ch := make(chan dialResult, 1)
				go func() {
					conn, err := d(network, addr)
					ch <- dialResult{conn, err}
				}()
				select {
				case r := <-ch:
					return r.conn, r.err
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		if state.dialContext != nil {
			states = append(states, state)
		}
	}

	if len(states) == 0 {
		return nil, errors.New("no proxifly proxies fetched")
	}

	return states, nil
}

func NewProxiflyCountryPools(logger *log.Logger) (map[string]*ProxyPool, []*proxyState, error) {
	proxies, err := fetchProxiflyProxies()
	if err != nil {
		return nil, nil, fmt.Errorf("proxifly=%w", err)
	}

	logger.Printf("Proxifly: %d proxies", len(proxies))
	proxies = testProxiesConcurrently(proxies, healthCheckConcurrency, logger)
	groups := groupProxiesByCountry(proxies)

	pools := make(map[string]*ProxyPool, len(groups))
	for country, states := range groups {
		pools[country] = NewProxyPool(logger, states)
	}

	return pools, proxies, nil
}

func startProxyRefresh(providers []ProxyProvider, countryPools map[string]*ProxyPool, defaultPool *ProxyPool, onRefresh func(), logger *log.Logger) {
	go func() {
		ticker := time.NewTicker(providerRefreshEvery)
		defer ticker.Stop()

		lastETags := make(map[string]string)
		for _, provider := range providers {
			etag, err := provider.ETag()
			if err != nil {
				logger.Printf("%s initial ETag failed: %v", provider.Name(), err)
				continue
			}
			lastETags[provider.Name()] = etag
		}

		for range ticker.C {
			for _, provider := range providers {
				name := provider.Name()
				etag, err := provider.ETag()
				if err != nil {
					logger.Printf("%s ETag check failed: %v", name, err)
					continue
				}
				if etag == lastETags[name] {
					logger.Printf("%s: no change", name)
					continue
				}

				proxies, err := provider.Fetch()
				if err != nil {
					logger.Printf("%s refresh failed: %v", name, err)
					continue
				}

				logger.Printf("%s: %d proxies", name, len(proxies))
				proxies = testProxiesConcurrently(proxies, healthCheckConcurrency, logger)
				groups := groupProxiesByCountry(proxies)

				defaultPool.Replace(proxies)
				if onRefresh != nil {
					onRefresh()
				}

				for country, states := range groups {
					if pool, ok := countryPools[country]; ok {
						pool.Replace(states)
						if countryDialer, ok := regionDialers[country]; ok {
							pool.SetPrimary(&proxyState{
								key:         "psiphon://" + country,
								url:         &url.URL{Scheme: "psiphon", Host: country},
								dialContext: countryDialer.DialContext,
								country:     country,
								psiphon:     countryDialer,
							})
						}
					}
				}

				lastETags[name] = etag
				logger.Printf("%s refreshed: %d healthy proxies", name, len(proxies))
			}
		}
	}()
}

func testProxiesConcurrently(proxies []*proxyState, concurrency int, logger *log.Logger) []*proxyState {
	if len(proxies) == 0 {
		return nil
	}

	sem := make(chan struct{}, concurrency)
	healthy := make([]*proxyState, 0, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var tested int32
	total := len(proxies)

	for _, p := range proxies {
		sem <- struct{}{}
		wg.Add(1)
		go func(ps *proxyState) {
			defer wg.Done()
			defer func() { <-sem }()

			if testProxyReachable(ps) {
				mu.Lock()
				healthy = append(healthy, ps)
				mu.Unlock()
			}

			if n := atomic.AddInt32(&tested, 1); n%500 == 0 || n == int32(total) {
				logger.Printf("[CHECK] %d/%d, %d healthy", n, total, len(healthy))
			}
		}(p)
	}

	wg.Wait()
	logger.Printf("[CHECK] %d proxies, %d healthy", total, len(healthy))
	return healthy
}

func testProxyReachable(p *proxyState) bool {
	if p.dialContext == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyHealthTimeout)
	defer cancel()

	start := time.Now()
	conn, err := p.dialContext(ctx, "tcp", "1.1.1.1:80")
	p.latency = time.Since(start)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
