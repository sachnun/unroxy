package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logger := log.Default()

	router := newCountryPoolRouter(logger)

	handler := NewProxyHandler(logger, router)

	logger.Printf("Unroxy running on :%s", port)
	logger.Printf("Rewrite proxy:  http://localhost:%s/{domain}/{path}", port)
	logger.Printf("Forward proxy:  curl -x http://localhost:%s http://example.com", port)
	logger.Printf("CONNECT tunnel: curl -x http://localhost:%s https://example.com", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func newCountryPoolRouter(logger *log.Logger) *PoolRouter {
	if allServerEntries == nil {
		allServerEntries = parseServerEntries(embeddedServerList)
	}

	initPsiphonNoticeHandler(logger)

	serverCounts := serversByRegion()

	maxPerRegion := envInt("PSIPHON_MAX_PER_REGION", 3)

	named := make([]*NamedPool, 0)
	defaultPool := NewProxyPool(logger, nil)

	for region, serverCount := range serverCounts {
		poolSize := min(maxPerRegion, serverCount)
		if poolSize == 0 {
			continue
		}
		dialer, err := NewPsiphonDialer(region, poolSize, logger)
		if err != nil {
			logger.Printf("Psiphon [%s] init failed: %v", region, err)
			continue
		}
		ps := &proxyState{
			key:         "psiphon://" + region,
			url:         &url.URL{Scheme: "psiphon", Host: region},
			dialContext: dialer.DialContext,
			country:     region,
			psiphon:     dialer,
		}
		defaultPool.SetPrimary(ps)
	}

	countryPools, allProxies, err := NewProxiflyCountryPools(logger)
	if err != nil {
		logger.Printf("Proxifly proxy not ready: %v", err)
	}

	if allProxies != nil {
		for _, p := range allProxies {
			p.priority = 1
		}
		logger.Printf("Proxifly: %d proxies", len(allProxies))
		defaultPool.Replace(allProxies)
		for _, dialer := range regionDialers {
			ps := &proxyState{
				key:         "psiphon://" + dialer.region,
				url:         &url.URL{Scheme: "psiphon", Host: dialer.region},
				dialContext: dialer.DialContext,
				country:     dialer.region,
				psiphon:     dialer,
			}
			defaultPool.SetPrimary(ps)
		}
	}

	if countryPools != nil {
		for country, pool := range countryPools {
			dialer, ok := regionDialers[country]
			if ok {
				ps := &proxyState{
					key:         "psiphon://" + country,
					url:         &url.URL{Scheme: "psiphon", Host: country},
					dialContext: dialer.DialContext,
					country:     country,
					psiphon:     dialer,
				}
				pool.SetPrimary(ps)
			}
			transport := NewRotatingProxyTransport(pool)
			named = append(named, &NamedPool{
				Name:      country,
				Username:  country,
				Pool:      pool,
				Transport: transport,
			})
		}
	}

	defaultTransport := NewRotatingProxyTransport(defaultPool)

	totalTunnels := 0
	regionSummary := make([]string, 0, len(regionDialers))
	for _, d := range regionDialers {
		regionSummary = append(regionSummary, fmt.Sprintf("%s(%d)", d.region, d.targetPool))
		totalTunnels += d.targetPool
	}
	if len(regionSummary) > 0 {
		logger.Printf("Psiphon: %s ready (%d tunnels)", strings.Join(regionSummary, " "), totalTunnels)
	}

	readdPsiphon := func() {
		for _, dialer := range regionDialers {
			ps := &proxyState{
				key:         "psiphon://" + dialer.region,
				url:         &url.URL{Scheme: "psiphon", Host: dialer.region},
				dialContext: dialer.DialContext,
				country:     dialer.region,
				psiphon:     dialer,
			}
			defaultPool.SetPrimary(ps)
		}
	}

	if warpEnabled() {
		initWarpUsque(defaultTransport, named, logger)
	}

	startProxyRefresh([]ProxyProvider{&proxiflyProvider{}}, countryPools, defaultPool, readdPsiphon, logger)

	return NewPoolRouter(named, defaultTransport)
}

func initWarpUsque(defaultTransport *RotatingProxyTransport, named []*NamedPool, logger *log.Logger) {
	configPath, err := findUsqueConfig()
	if err != nil {
		logger.Printf("WARP register failed (%v)", err)
		return
	}

	psiphonDial := pickPsiphonDialer()
	u, dialer, err := startWarpUsque("40000", "6443", configPath, psiphonDial, logger)
	if err != nil {
		logger.Printf("WARP usque failed (%v)", err)
		return
	}

	wt := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}

	defaultTransport.SetWarpTransport(wt)
	_ = u
	logger.Printf("WARP: active, default via Psiphon")

	warpCount := 0
	for _, np := range named {
		if np.Transport == nil || warpCount >= 1 {
			continue
		}
		poolDialer := pickPoolPsiphonDialer(np.Pool)
		if poolDialer == nil {
			continue
		}
		port := fmt.Sprintf("%d", 40001+warpCount)
		fwdPort := fmt.Sprintf("%d", 6443+warpCount)
		pu, pdialer, err := startWarpUsque(port, fwdPort, configPath, poolDialer, logger)
		if err != nil {
			logger.Printf("WARP [%s] failed: %v", np.Name, err)
			continue
		}
		pwt := &http.Transport{
			DialContext:           pdialer.DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   5,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		}
		np.Transport.SetWarpTransport(pwt)
		_ = pu
		warpCount++
		logger.Printf("WARP: %s via Psiphon", np.Name)
	}
}

func pickPoolPsiphonDialer(pool *ProxyPool) func(context.Context, string, string) (net.Conn, error) {
	if pool == nil {
		return nil
	}
	candidates := pool.Candidates(time.Now(), "")
	for _, c := range candidates {
		if c.psiphon != nil && c.psiphon.tunnelReady.Load() > 0 {
			return c.psiphon.DialContext
		}
	}
	return nil
}

func pickPsiphonDialer() func(context.Context, string, string) (net.Conn, error) {
	for {
		for _, d := range regionDialers {
			if d.tunnelReady.Load() > 0 {
				return d.DialContext
			}
		}
		time.Sleep(time.Second)
	}
}
