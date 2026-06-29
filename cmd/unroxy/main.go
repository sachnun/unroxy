package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func main() {
	port := "8080"

	logger := log.Default()

	router := newCountryPoolRouter(logger)

	if warpEnabled() {
		initWarpAsync(router, logger)
	}

	handler := NewProxyHandler(logger, router)

	logger.Printf("Unroxy running on :%s", port)

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

	maxPerRegion := 3

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

	startProxyRefresh([]ProxyProvider{&proxiflyProvider{}}, countryPools, defaultPool, readdPsiphon, logger)

	return NewPoolRouter(named, defaultTransport)
}

func initWarpAsync(router *PoolRouter, logger *log.Logger) {
	warpPools := initWarpUsque(logger)
	for _, p := range warpPools {
		router.Add(p)
	}
	go initWarpRegional(router, logger)
}

func initWarpRegional(router *PoolRouter, logger *log.Logger) {
	configPath, err := findUsqueConfig()
	if err != nil {
		return
	}

	port := 40001
	fwdPort := 6444
	for region, dialer := range regionDialers {
		rU, rDialer, err := startWarpUsque(fmt.Sprintf("%d", port), fmt.Sprintf("%d", fwdPort), configPath, dialer.DialContext, logger)
		if err != nil {
			logger.Printf("WARP/%s: start failed (%v)", region, err)
			continue
		}
		_ = rU

		rWt := &http.Transport{
			DialContext:           rDialer.DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   5,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		}

		rTransport := NewRotatingProxyTransport(nil)
		rTransport.SetWarpTransport(rWt)

		router.Add(&NamedPool{
			Name:      "WARP/" + region,
			Username:  "WARP/" + region,
			Pool:      NewProxyPool(logger, nil),
			Transport: rTransport,
		})
		logger.Printf("WARP/%s: active, path /warp/%s or auth user \"warp/%s\"", region, region, region)

		port++
		fwdPort++
	}

	for _, name := range router.Names() {
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "WARP") || strings.Contains(upper, "/") {
			continue
		}
		if _, hasPsiphon := regionDialers[upper]; hasPsiphon {
			continue
		}
		np := router.Get(upper)
		if np == nil || np.Transport == nil || np.Pool == nil || np.Pool.Count() == 0 {
			continue
		}

		rU, rDialer, err := startWarpUsque(fmt.Sprintf("%d", port), fmt.Sprintf("%d", fwdPort), configPath, np.Transport.DialContext, logger)
		if err != nil {
			logger.Printf("WARP/%s: start failed (%v)", upper, err)
			continue
		}
		_ = rU

		rWt := &http.Transport{
			DialContext:           rDialer.DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   5,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		}

		rTransport := NewRotatingProxyTransport(nil)
		rTransport.SetWarpTransport(rWt)

		router.Add(&NamedPool{
			Name:      "WARP/" + upper,
			Username:  "WARP/" + upper,
			Pool:      NewProxyPool(logger, nil),
			Transport: rTransport,
		})
		logger.Printf("WARP/%s: active (proxifly), path /warp/%s or auth user \"warp/%s\"", upper, upper, upper)

		port++
		fwdPort++
	}
}

func initWarpUsque(logger *log.Logger) []*NamedPool {
	configPath, err := findUsqueConfig()
	if err != nil {
		logger.Printf("WARP: config not found (%v)", err)
		return nil
	}

	psiphonDial := pickPsiphonDialer()
	u, dialer, err := startWarpUsque("40000", "6443", configPath, psiphonDial, logger)
	if err != nil {
		logger.Printf("WARP: start failed (%v)", err)
		return nil
	}
	_ = u

	wt := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}

	warpTransport := NewRotatingProxyTransport(nil)
	warpTransport.SetWarpTransport(wt)

	warpPools := []*NamedPool{{
		Name:      "WARP",
		Username:  "WARP",
		Pool:      NewProxyPool(logger, nil),
		Transport: warpTransport,
	}}
	logger.Printf("WARP: active, path /warp or auth user \"warp\"")

	return warpPools
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
