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

	handler := NewProxyHandlerWithLoggerAndRouter(logger, router)

	logger.Printf("Unroxy running on :%s", port)
	logger.Printf("Rewrite proxy:  http://localhost:%s/{domain}/{path}", port)
	logger.Printf("Forward proxy:  curl -x http://localhost:%s http://example.com", port)
	logger.Printf("CONNECT tunnel: curl -x http://localhost:%s https://example.com", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func newCountryPoolRouter(logger *log.Logger) *PoolRouter {
	apiKeyValue := os.Getenv(webshareAPIKeyEnv)
	if apiKeyValue == "" {
		logger.Printf("Webshare proxy not ready: %s must be set", webshareAPIKeyEnv)
		return NewPoolRouter(nil, nil)
	}

	countryPools, allProxies, apiKeys, err := NewWebshareCountryPools(logger, apiKeyValue)
	if err != nil {
		logger.Printf("Webshare proxy not ready")
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
	startCountryPoolsRefresh(countryPools, defaultPool, apiKeys, logger)

	return NewPoolRouter(named, defaultTransport)
}
