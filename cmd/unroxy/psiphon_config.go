package main

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

func buildPsiphonConfig(dataDir string, poolSize, minIdle, maxTunnels int, egressRegion string) map[string]interface{} {
	if minIdle > poolSize {
		minIdle = poolSize
	}
	if maxTunnels < poolSize {
		maxTunnels = poolSize
	}

	sshWindowSize := 32
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
		"LimitMeekBufferSizes":           false,
		"LimitRelayBufferSizes":          false,
		"LimitCPUThreads":                true,
		"ConnectionWorkerPoolMaxSize":    4,
		"SSHChannelWindowSize":           &sshWindowSize,
		"DisableServerEntriesReporter":   true,
		"DisableReplay":                  true,
		"IgnoreHandshakeStatsRegexps":    true,
	}

	if egressRegion != "" {
		pc["EgressRegion"] = egressRegion
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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
