package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

type serverEntryInfo struct {
	ip     string
	region string
}

type tunnelInfo struct {
	ip       string
	region   string
	protocol string
}

var (
	allServerEntries  map[string]serverEntryInfo
	protocolByIP      sync.Map
	regionDialers     = make(map[string]*PsiphonDialer)
	regionDialersMu   sync.Mutex
	globalHostTunnels sync.Map
)

func parseServerEntries(raw string) map[string]serverEntryInfo {
	entries := make(map[string]serverEntryInfo)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		decoded, err := hex.DecodeString(line)
		if err != nil {
			continue
		}
		decodedLine := string(decoded)
		jsonStart := strings.Index(decodedLine, "{")
		if jsonStart < 0 {
			continue
		}
		var entry struct {
			IpAddress       string `json:"ipAddress"`
			WebServerSecret string `json:"webServerSecret"`
			Region          string `json:"region"`
		}
		if json.Unmarshal([]byte(decodedLine[jsonStart:]), &entry) != nil {
			continue
		}
		if entry.IpAddress == "" {
			continue
		}
		tag := protocol.GenerateServerEntryTag(entry.IpAddress, entry.WebServerSecret)
		diagID := protocol.TagToDiagnosticID(tag)
		entries[diagID] = serverEntryInfo{ip: entry.IpAddress, region: entry.Region}
	}
	return entries
}

func serversByRegion() map[string]int {
	counts := make(map[string]int)
	for _, e := range allServerEntries {
		if e.region != "" {
			counts[e.region]++
		}
	}
	return counts
}

// PsiphonDialers returns a snapshot of all registered region dialers.
func PsiphonDialers() map[string]*PsiphonDialer {
	regionDialersMu.Lock()
	defer regionDialersMu.Unlock()
	snapshot := make(map[string]*PsiphonDialer, len(regionDialers))
	for region, d := range regionDialers {
		snapshot[region] = d
	}
	return snapshot
}

// EnsureServerEntries loads the latest server list once, if needed.
func EnsureServerEntries(ctx context.Context, logger *log.Logger) {
	if allServerEntries != nil {
		return
	}
	serverEntryList = loadServerEntries(ctx, logger)
	allServerEntries = parseServerEntries(serverEntryList)
	if len(allServerEntries) == 0 {
		logger.Printf("Psiphon: no server entries available, provider will be idle")
	}
}

// ServersByRegion counts server entries per region.
func ServersByRegion() map[string]int {
	if allServerEntries == nil {
		return map[string]int{}
	}
	return serversByRegion()
}
