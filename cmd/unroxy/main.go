package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
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

	startProxyRefresh([]ProxyProvider{&proxiflyProvider{}}, countryPools, defaultPool, nil, logger)

	return NewPoolRouter(named, defaultTransport)
}
