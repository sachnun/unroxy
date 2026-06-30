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
	"sync"
	"time"
)

func main() {
	port := "8080"

	logger := log.Default()

	router := newCountryPoolRouter(logger)

	if warpEnabled() {
		initWarpAsync(router, logger)
	}

	handler := NewProxyHandler(logger, router, os.Getenv("TCP"))

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

	type psiphonResult struct {
		region string
		dialer *PsiphonDialer
		err    error
	}
	psiphonCh := make(chan psiphonResult)
	var psiphonWg sync.WaitGroup

	for region, serverCount := range serverCounts {
		poolSize := min(maxPerRegion, serverCount)
		if poolSize == 0 {
			continue
		}
		psiphonWg.Add(1)
		go func(region string, poolSize int) {
			defer psiphonWg.Done()
			dialer, err := NewPsiphonDialer(region, poolSize, logger)
			psiphonCh <- psiphonResult{region: region, dialer: dialer, err: err}
		}(region, poolSize)
	}

	go func() {
		psiphonWg.Wait()
		close(psiphonCh)
	}()

	var (
		countryPools map[string]*ProxyPool
		allProxies   []*proxyState
		proxiflyErr  error
	)
	var proxiflyWg sync.WaitGroup
	proxiflyWg.Add(1)
	go func() {
		defer proxiflyWg.Done()
		countryPools, allProxies, proxiflyErr = NewProxiflyCountryPools(logger)
	}()

	for r := range psiphonCh {
		if r.err != nil {
			logger.Printf("Psiphon [%s] init failed: %v", r.region, r.err)
			continue
		}
		ps := &proxyState{
			key:         "psiphon://" + r.region,
			url:         &url.URL{Scheme: "psiphon", Host: r.region},
			dialContext: r.dialer.DialContext,
			country:     r.region,
			psiphon:     r.dialer,
		}
		defaultPool.SetPrimary(ps)
	}

	proxiflyWg.Wait()

	if proxiflyErr != nil {
		logger.Printf("Proxifly proxy not ready: %v", proxiflyErr)
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
	go func() {
		warpPools := initWarpUsque(logger)
		for _, p := range warpPools {
			router.Add(p)
		}
	}()
	go initWarpRegional(router, logger)
}

func initWarpRegional(router *PoolRouter, logger *log.Logger) {
	configPath, err := findUsqueConfig()
	if err != nil {
		return
	}

	type warpInstance struct {
		region  string
		dialer  *PsiphonDialer
		port    int
		fwdPort int
	}
	instances := make([]warpInstance, 0)
	port := 40001
	fwdPort := 6444
	for region, dialer := range regionDialers {
		instances = append(instances, warpInstance{region, dialer, port, fwdPort})
		port++
		fwdPort++
	}

	var wg sync.WaitGroup
	for _, inst := range instances {
		wg.Add(1)
		go func(inst warpInstance) {
			defer wg.Done()
			rU, rDialer, err := startWarpUsque(fmt.Sprintf("%d", inst.port), fmt.Sprintf("%d", inst.fwdPort), configPath, inst.dialer.DialContext, logger)
			if err != nil {
				logger.Printf("WARP/%s: start failed (%v)", inst.region, err)
				return
			}
			_ = rU

			rWt := newUTLSTransport(rDialer.DialContext)
			rWt.MaxIdleConns = 50
			rWt.MaxIdleConnsPerHost = 5
			rWt.ResponseHeaderTimeout = 20 * time.Second

			rTransport := NewRotatingProxyTransport(nil)
			rTransport.SetWarpTransport(rWt)

			router.Add(&NamedPool{
				Name:      "WARP/" + inst.region,
				Username:  "WARP/" + inst.region,
				Pool:      NewProxyPool(logger, nil),
				Transport: rTransport,
			})
			logger.Printf("WARP/%s: active, path /warp/%s or auth user \"warp/%s\"", inst.region, inst.region, inst.region)
		}(inst)
	}
	wg.Wait()

	regionDialersMu.Lock()
	hasPsiphon := make(map[string]bool, len(regionDialers))
	for region := range regionDialers {
		hasPsiphon[region] = true
	}
	regionDialersMu.Unlock()

	type proxiflyWarp struct {
		name      string
		upper     string
		transport *RotatingProxyTransport
	}
	candidates := make([]proxiflyWarp, 0)
	for _, name := range router.Names() {
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "WARP") || strings.Contains(upper, "/") {
			continue
		}
		if hasPsiphon[upper] {
			continue
		}
		np := router.Get(upper)
		if np == nil || np.Transport == nil || np.Pool == nil || np.Pool.Count() == 0 {
			continue
		}
		candidates = append(candidates, proxiflyWarp{name, upper, np.Transport})
	}

	var wg2 sync.WaitGroup
	for i, c := range candidates {
		wg2.Add(1)
		go func(c proxiflyWarp, port, fwdPort int) {
			defer wg2.Done()
			rU, rDialer, err := startWarpUsque(fmt.Sprintf("%d", port), fmt.Sprintf("%d", fwdPort), configPath, c.transport.DialContext, logger)
			if err != nil {
				logger.Printf("WARP/%s: start failed (%v)", c.upper, err)
				return
			}
			_ = rU

			rWt := newUTLSTransport(rDialer.DialContext)
			rWt.MaxIdleConns = 50
			rWt.MaxIdleConnsPerHost = 5
			rWt.ResponseHeaderTimeout = 20 * time.Second

			rTransport := NewRotatingProxyTransport(nil)
			rTransport.SetWarpTransport(rWt)

			router.Add(&NamedPool{
				Name:      "WARP/" + c.upper,
				Username:  "WARP/" + c.upper,
				Pool:      NewProxyPool(logger, nil),
				Transport: rTransport,
			})
			logger.Printf("WARP/%s: active (proxifly), path /warp/%s or auth user \"warp/%s\"", c.upper, c.upper, c.upper)
		}(c, port+i, fwdPort+i)
	}
	wg2.Wait()
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

	wt := newUTLSTransport(dialer.DialContext)
	wt.MaxIdleConns = 100
	wt.MaxIdleConnsPerHost = 10
	wt.ResponseHeaderTimeout = 20 * time.Second

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
