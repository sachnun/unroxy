// Package psiphon implements the Psiphon tunnel provider: one dialer per
// embedded server region, contributed as primaries to the default pool and
// registered in core so other providers (WARP) can reuse the tunnels.
package psiphon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

const maxPerRegion = 3

// Provider boots the per-region Psiphon dialers.
type Provider struct{}

func init() {
	providers.Register(&Provider{})
}

func (p *Provider) Name() string { return "Psiphon" }

func (p *Provider) Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	core.InitPsiphonNoticeHandler(logger)
	core.EnsureServerEntries()

	serverCounts := core.ServersByRegion()

	type psiphonResult struct {
		region string
		dialer *core.PsiphonDialer
		err    error
	}
	ch := make(chan psiphonResult)
	var wg sync.WaitGroup

	for region, serverCount := range serverCounts {
		poolSize := min(maxPerRegion, serverCount)
		if poolSize == 0 {
			continue
		}
		wg.Add(1)
		go func(region string, poolSize int) {
			defer wg.Done()
			dialer, err := core.NewPsiphonDialer(region, poolSize, logger)
			ch <- psiphonResult{region: region, dialer: dialer, err: err}
		}(region, poolSize)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for r := range ch {
		if r.err != nil {
			logger.Printf("Psiphon [%s] init failed: %v", r.region, r.err)
			continue
		}
		host.AddPrimary(core.NewPsiphonState(r.dialer))
	}

	totalTunnels := 0
	regionSummary := make([]string, 0, len(core.PsiphonDialers()))
	for _, d := range core.PsiphonDialers() {
		regionSummary = append(regionSummary, fmt.Sprintf("%s(%d)", d.Region(), d.TargetPool()))
		totalTunnels += d.TargetPool()
	}
	if len(regionSummary) > 0 {
		logger.Printf("Psiphon: %s ready (%d tunnels)", strings.Join(regionSummary, " "), totalTunnels)
	}
	return nil
}
