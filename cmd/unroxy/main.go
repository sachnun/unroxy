package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	// Register providers (blank imports activate init() registration).
	_ "unroxy/internal/providers/proxifly"
	_ "unroxy/internal/providers/psiphon"
	_ "unroxy/internal/providers/tor"
	_ "unroxy/internal/providers/turbo"
	_ "unroxy/internal/providers/urban"
	_ "unroxy/internal/providers/warp"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

const refreshEvery = 5 * time.Minute

func main() {
	logger := log.Default()
	host := providers.NewHost(logger)

	ctx := context.Background()
	for _, p := range providers.All() {
		go func(p providers.Provider) {
			if err := p.Start(ctx, host, logger); err != nil {
				logger.Printf("%s: start failed: %v", p.Name(), err)
			}
		}(p)
	}
	go refreshLoop(ctx, logger)

	handler := core.NewProxyHandler(logger, host.Router(), os.Getenv("TCP"))
	logger.Printf("Unroxy running on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func refreshLoop(ctx context.Context, logger *log.Logger) {
	ticker := time.NewTicker(refreshEvery)
	defer ticker.Stop()
	for range ticker.C {
		for _, p := range providers.All() {
			r, ok := p.(providers.Refresher)
			if !ok {
				continue
			}
			if err := r.Refresh(ctx); err != nil {
				logger.Printf("%s refresh failed: %v", p.Name(), err)
			}
		}
	}
}
