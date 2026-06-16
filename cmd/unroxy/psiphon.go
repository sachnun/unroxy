package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
)

//go:embed server_entries.txt
var embeddedServerList string

type PsiphonDialer struct {
	controller *psiphon.Controller
	ctx        context.Context
	cancel     context.CancelFunc
	once       sync.Once
	ready      chan struct{}
}

func NewPsiphonDialer(logger *log.Logger) (*PsiphonDialer, error) {
	dataDir := envOrDefault("PSIPHON_DATA_DIR", "/tmp/unroxy-psiphon")

	dsDir := dataDir + "/ca.psiphon.PsiphonTunnel.tunnel-core/datastore"
	if err := os.MkdirAll(dsDir, 0755); err != nil {
		return nil, err
	}

	poolSize := envInt("PSIPHON_POOL_SIZE", 32)
	minIdle := envInt("PSIPHON_MIN_IDLE", 28)
	maxTunnels := envInt("PSIPHON_MAX_TUNNELS", 64)

	pc := buildPsiphonConfig(dataDir, poolSize, minIdle, maxTunnels)
	configJSON, _ := json.Marshal(pc)

	config, err := psiphon.LoadConfig(configJSON)
	if err != nil {
		return nil, err
	}

	if err := config.Commit(true); err != nil {
		return nil, err
	}

	if err := psiphon.OpenDataStore(config); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	if embeddedServerList != "" {
		if err := psiphon.ImportEmbeddedServerEntries(ctx, config, "", embeddedServerList); err != nil {
			logger.Printf("Psiphon import server entries warning: %v", err)
		}
	}

	controller, err := psiphon.NewController(config)
	if err != nil {
		cancel()
		return nil, err
	}

	d := &PsiphonDialer{
		controller: controller,
		ctx:        ctx,
		cancel:     cancel,
		ready:      make(chan struct{}),
	}

	go func() {
		controller.Run(ctx)
	}()
	close(d.ready)

	logger.Printf("Psiphon tunnel starting (pool=%d, min_idle=%d, max=%d)", poolSize, minIdle, maxTunnels)
	return d, nil
}

func (d *PsiphonDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	<-d.ready
	return d.controller.Dial(addr, nil)
}

func (d *PsiphonDialer) Close() {
	d.once.Do(func() { d.cancel() })
}

func buildPsiphonConfig(dataDir string, poolSize, minIdle, maxTunnels int) map[string]interface{} {
	if minIdle > poolSize {
		minIdle = poolSize
	}
	if maxTunnels < poolSize {
		maxTunnels = poolSize
	}

	sshWindowSize := 1
	pc := map[string]interface{}{
		"LocalSocksProxyPort":            0,
		"LocalHttpProxyPort":             0,
		"PropagationChannelId":           "FFFFFFFFFFFFFFFF",
		"SponsorId":                      "FFFFFFFFFFFFFFFF",
		"EstablishTunnelTimeoutSeconds":  60,
		"TunnelPoolSize":                 poolSize,
		"MaxTunnelPoolSize":              maxTunnels,
		"MinIdleTunnels":                 minIdle,
		"DisableRemoteServerListFetcher": true,
		"DisableDSLFetcher":              true,
		"DataRootDirectory":              dataDir,
		"NetworkID":                      envOrDefault("PSIPHON_NETWORK_ID", "WIFI"),
		"EmitDiagnosticNotices":          true,
		"DisableTactics":                 true,
		"LimitMeekBufferSizes":           true,
		"LimitRelayBufferSizes":          true,
		"LimitCPUThreads":                true,
		"ConnectionWorkerPoolMaxSize":    4,
		"SSHChannelWindowSize":           &sshWindowSize,
		"DisableServerEntriesReporter":   true,
		"DisableReplay":                  true,
		"IgnoreHandshakeStatsRegexps":    true,
	}

	if overlay := os.Getenv("PSIPHON_JSON"); overlay != "" {
		var m map[string]interface{}
		if json.Unmarshal([]byte(overlay), &m) == nil {
			for k, v := range m {
				pc[k] = v
			}
		}
	}

	return pc
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
