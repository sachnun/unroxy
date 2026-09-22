package psiphon

import (
	"context"
	"log"
	"sync"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

func Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	core.InitNotices(logger)
	core.LoadServers(ctx, logger)

	byRegion := core.EntriesByRegion()

	type result struct {
		id     string
		dialer *core.Dialer
		err    error
	}
	ch := make(chan result)
	var wg sync.WaitGroup

	for region, entries := range byRegion {
		id := region
		wg.Add(1)
		go func(id, region string, entries []core.ServerEntry) {
			defer wg.Done()
			dialer, err := core.NewDialer(id, region, entries, logger)
			ch <- result{id: id, dialer: dialer, err: err}
		}(id, region, entries)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for r := range ch {
		if r.err != nil {
			logger.Printf("Psiphon [%s] init failed: %v", r.id, r.err)
			continue
		}
		host.AddPrimary(core.NewState(r.dialer))
	}

	totalTarget := 0
	for _, d := range core.Dialers() {
		totalTarget += d.TargetPool()
	}
	if totalTarget > 0 {
		logger.Printf("Psiphon: %d regions, %d tunnels target", len(byRegion), totalTarget)
	}
	return nil
}
