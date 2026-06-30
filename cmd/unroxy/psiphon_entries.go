package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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

func formatRegionSummary(regionCount map[string]int) string {
	type kv struct {
		region string
		count  int
	}
	sorted := make([]kv, 0, len(regionCount))
	for r, c := range regionCount {
		sorted = append(sorted, kv{r, c})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	parts := make([]string, len(sorted))
	for i, kv := range sorted {
		parts[i] = fmt.Sprintf("%s(%d)", kv.region, kv.count)
	}
	return strings.Join(parts, ", ")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
