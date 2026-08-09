package core

import (
	"encoding/hex"
	"encoding/json"
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
		parts := strings.SplitN(decodedLine, " ", 2)
		if len(parts) < 2 {
			continue
		}
		ip := parts[0]
		var entry struct {
			IpAddress       string `json:"ipAddress"`
			WebServerSecret string `json:"webServerSecret"`
			Region          string `json:"region"`
		}
		if json.Unmarshal([]byte(parts[1]), &entry) != nil {
			continue
		}
		if entry.IpAddress == "" || entry.WebServerSecret == "" {
			continue
		}
		tag := protocol.GenerateServerEntryTag(entry.IpAddress, entry.WebServerSecret)
		diagID := protocol.TagToDiagnosticID(tag)
		entries[diagID] = serverEntryInfo{ip: ip, region: entry.Region}
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

// EnsureServerEntries parses the embedded server list once, if needed.
func EnsureServerEntries() {
	if allServerEntries == nil {
		allServerEntries = parseServerEntries(embeddedServerList)
	}
}

// ServersByRegion counts embedded server entries per region.
func ServersByRegion() map[string]int {
	EnsureServerEntries()
	return serversByRegion()
}
