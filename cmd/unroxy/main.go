package main

import (
	"context"
	"log"
	"net/http"

	"unroxy/internal/core"
	"unroxy/internal/providers"
	"unroxy/internal/providers/psiphon"
)

func main() {
	logger := log.Default()
	host := providers.NewHost(logger)

	go psiphon.Start(context.Background(), host, logger)

	handler := core.NewProxyHandler(logger, host.Router())
	logger.Printf("Unroxy running on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}
