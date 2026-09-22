package core

func buildPsiphonConfig(dataDir string, poolSize int, egressRegion string) map[string]interface{} {
	sshWindowSize := 32
	pc := map[string]interface{}{
		"LocalSocksProxyPort":             0,
		"LocalHttpProxyPort":              0,
		"PropagationChannelId":            "FFFFFFFFFFFFFFFF",
		"SponsorId":                       "FFFFFFFFFFFFFFFF",
		"EstablishTunnelTimeoutSeconds":   300,
		"TunnelPoolSize":                  poolSize,
		"DisableDSLFetcher":               true,
		"DataRootDirectory":               dataDir,
		"NetworkID":                       "WIFI",
		"EmitDiagnosticNotices":           true,
		"DisableTactics":                  true,
		"LimitMeekBufferSizes":            false,
		"LimitRelayBufferSizes":           true,
		"LimitCPUThreads":                 true,
		"ConnectionWorkerPoolSize":        2,
		"ConnectionWorkerPoolMaxSize":     2,
		"LimitIntensiveConnectionWorkers": 1,
		"SSHChannelWindowSize":            &sshWindowSize,
		"DisableServerEntriesReporter":    true,
		"DisableReplay":                   true,
		"IgnoreHandshakeStatsRegexps":     true,
		"ServerEntrySignaturePublicKey":   psiphonServerEntrySignaturePublicKey,
	}

	if egressRegion != "" {
		pc["EgressRegion"] = egressRegion
	}

	return pc
}
