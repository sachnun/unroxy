// Package warp implements the WARP provider: it contributes named pools
// whose transport tunnels traffic through the local usque MASQUE binary,
// reusing Psiphon tunnels (via core dialers) or Proxifly pools as uplink.
package warp

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

// Provider starts the WARP named pools.
type Provider struct{}

func init() {
	providers.Register(&Provider{})
}

func (p *Provider) Name() string { return "WARP" }

func (p *Provider) Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	configPath, err := findUsqueConfig()
	if err != nil {
		logger.Printf("WARP: config not found (%v)", err)
		return nil
	}

	go p.startDefault(host, configPath, logger)
	go p.startRegional(ctx, host, configPath, logger)
	return nil
}

// waitForDialers blocks until Psiphon registered at least one dialer.
func waitForDialers(ctx context.Context) {
	for len(core.PsiphonDialers()) == 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (p *Provider) startDefault(host *providers.Host, configPath string, logger *log.Logger) {
	psiphonDial := pickPsiphonDialer()
	u, dialer, err := startWarpUsque("40000", "6443", configPath, psiphonDial, logger)
	if err != nil {
		logger.Printf("WARP: start failed (%v)", err)
		return
	}
	_ = u

	wt := core.NewUTLSTransport(dialer.DialContext)
	wt.MaxIdleConns = 100
	wt.MaxIdleConnsPerHost = 10
	wt.ResponseHeaderTimeout = 20 * time.Second

	warpTransport := core.NewRotatingProxyTransport(nil)
	warpTransport.SetWarpTransport(wt)

	host.AddNamed("WARP", core.NewProxyPool(logger, nil), warpTransport)
	logger.Printf("WARP: active, path /warp or auth user \"warp\"")
}

func (p *Provider) startRegional(ctx context.Context, host *providers.Host, configPath string, logger *log.Logger) {
	waitForDialers(ctx)

	type warpInstance struct {
		region  string
		dialer  *core.PsiphonDialer
		port    int
		fwdPort int
	}
	instances := make([]warpInstance, 0)
	port := 40001
	fwdPort := 6444
	for region, dialer := range core.PsiphonDialers() {
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

			rWt := core.NewUTLSTransport(rDialer.DialContext)
			rWt.MaxIdleConns = 50
			rWt.MaxIdleConnsPerHost = 5
			rWt.ResponseHeaderTimeout = 20 * time.Second

			rTransport := core.NewRotatingProxyTransport(nil)
			rTransport.SetWarpTransport(rWt)

			host.AddNamed("WARP/"+inst.region, core.NewProxyPool(logger, nil), rTransport)
			logger.Printf("WARP/%s: active, path /warp/%s or auth user \"warp/%s\"", inst.region, inst.region, inst.region)
		}(inst)
	}
	wg.Wait()

	hasPsiphon := make(map[string]bool, len(core.PsiphonDialers()))
	for region := range core.PsiphonDialers() {
		hasPsiphon[region] = true
	}

	type proxiflyWarp struct {
		name      string
		upper     string
		transport *core.RotatingProxyTransport
	}
	candidates := make([]proxiflyWarp, 0)
	for _, name := range host.Router().Names() {
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "WARP") || strings.Contains(upper, "/") {
			continue
		}
		if hasPsiphon[upper] {
			continue
		}
		np := host.Router().Get(upper)
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

			rWt := core.NewUTLSTransport(rDialer.DialContext)
			rWt.MaxIdleConns = 50
			rWt.MaxIdleConnsPerHost = 5
			rWt.ResponseHeaderTimeout = 20 * time.Second

			rTransport := core.NewRotatingProxyTransport(nil)
			rTransport.SetWarpTransport(rWt)

			host.AddNamed("WARP/"+c.upper, core.NewProxyPool(logger, nil), rTransport)
			logger.Printf("WARP/%s: active (proxifly), path /warp/%s or auth user \"warp/%s\"", c.upper, c.upper, c.upper)
		}(c, port+i, fwdPort+i)
	}
	wg2.Wait()
}

func pickPsiphonDialer() func(context.Context, string, string) (net.Conn, error) {
	for {
		for _, d := range core.PsiphonDialers() {
			if d.IsReady() {
				return d.DialContext
			}
		}
		time.Sleep(time.Second)
	}
}
