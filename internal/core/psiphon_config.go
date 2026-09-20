package core

import "encoding/base64"

func buildPsiphonConfig(dataDir string, poolSize, minIdle, maxTunnels int, egressRegion string) map[string]interface{} {
	if minIdle > poolSize {
		minIdle = poolSize
	}
	if maxTunnels < poolSize {
		maxTunnels = poolSize
	}

	sshWindowSize := 32
	pc := map[string]interface{}{
		"LocalSocksProxyPort":                0,
		"LocalHttpProxyPort":                 0,
		"PropagationChannelId":               "FFFFFFFFFFFFFFFF",
		"SponsorId":                          "FFFFFFFFFFFFFFFF",
		"EstablishTunnelTimeoutSeconds":      60,
		"TunnelPoolSize":                     poolSize,
		"MaxTunnelPoolSize":                  maxTunnels,
		"MinIdleTunnels":                     minIdle,
		"DisableDSLFetcher":                  true,
		"DataRootDirectory":                  dataDir,
		"NetworkID":                          "WIFI",
		"EmitDiagnosticNotices":              true,
		"DisableTactics":                     true,
		"LimitMeekBufferSizes":               false,
		"LimitRelayBufferSizes":              false,
		"LimitCPUThreads":                    true,
		"ConnectionWorkerPoolMaxSize":        4,
		"SSHChannelWindowSize":               &sshWindowSize,
		"DisableServerEntriesReporter":       true,
		"DisableReplay":                      true,
		"IgnoreHandshakeStatsRegexps":        true,
		"RemoteServerListURLs":               remoteServerListTransferURLs(),
		"RemoteServerListSignaturePublicKey": psiphonRemoteServerListSignaturePublicKey,
		"ServerEntrySignaturePublicKey":      psiphonServerEntrySignaturePublicKey,
	}

	if egressRegion != "" {
		pc["EgressRegion"] = egressRegion
	}

	return pc
}

func remoteServerListTransferURLs() []map[string]interface{} {
	urls := make([]map[string]interface{}, 0, len(psiphonRemoteServerListURLs))
	for _, u := range psiphonRemoteServerListURLs {
		urls = append(urls, map[string]interface{}{
			"URL": base64.StdEncoding.EncodeToString([]byte(u)),
		})
	}
	return urls
}
