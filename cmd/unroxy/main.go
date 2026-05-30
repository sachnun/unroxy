package main

import (
	"log"
	"net/http"
	"os"
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
	countryPools, allProxies, err := NewProxiflyCountryPools(logger)
	if err != nil {
		logger.Printf("Proxifly proxy not ready: %v", err)
		return NewPoolRouter(nil, nil)
	}

	// Default pool with ALL proxies (backward compat, no auth)
	defaultPool := NewProxyPool(logger, allProxies)
	defaultTransport := NewRotatingProxyTransport(defaultPool)

	// Named pools per country
	named := make([]*NamedPool, 0, len(countryPools))
	for country, pool := range countryPools {
		transport := NewRotatingProxyTransport(pool)
		named = append(named, &NamedPool{
			Name:      country,
			Username:  country,
			Pool:      pool,
			Transport: transport,
		})
		logger.Printf("Pool %q ready: %d proxies", country, pool.Count())
	}

	logger.Printf("Total: %d proxies across %d countries", len(allProxies), len(countryPools))

	// Start refresh for all pools
	startProxiflyRefresh(countryPools, defaultPool, logger)

	return NewPoolRouter(named, defaultTransport)
}
