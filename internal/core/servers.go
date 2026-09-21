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

type serverInfo struct {
	ip     string
	region string
}

type exitInfo struct {
	ip       string
	region   string
	protocol string
}

type ServerEntry struct {
	ID     string
	IP     string
	Region string
	Raw    string
}

var (
	allServerEntries  map[string]serverInfo
	entriesByRegion   map[string][]ServerEntry
	protocolByIP      sync.Map
	regionDialers     = make(map[string]*Dialer)
	regionDialersMu   sync.Mutex
	dialerByServerID  = make(map[string]*Dialer)
	globalHostTunnels sync.Map
)

func decodeEntry(line string) (id, ip, region string, ok bool) {
	decoded, err := hex.DecodeString(line)
	if err != nil {
		return "", "", "", false
	}
	decodedLine := string(decoded)
	jsonStart := strings.Index(decodedLine, "{")
	if jsonStart < 0 {
		return "", "", "", false
	}
	var entry struct {
		IpAddress       string `json:"ipAddress"`
		WebServerSecret string `json:"webServerSecret"`
		Region          string `json:"region"`
	}
	if json.Unmarshal([]byte(decodedLine[jsonStart:]), &entry) != nil {
		return "", "", "", false
	}
	if entry.IpAddress == "" {
		return "", "", "", false
	}
	tag := protocol.GenerateServerEntryTag(entry.IpAddress, entry.WebServerSecret)
	return protocol.TagToDiagnosticID(tag), entry.IpAddress, entry.Region, true
}

func parseServerEntries(raw string) map[string]serverInfo {
	entries := make(map[string]serverInfo)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, ip, region, ok := decodeEntry(line)
		if !ok {
			continue
		}
		entries[id] = serverInfo{ip: ip, region: region}
	}
	return entries
}

func parseEntriesByRegion(raw string) map[string][]ServerEntry {
	byRegion := make(map[string][]ServerEntry)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, ip, region, ok := decodeEntry(line)
		if !ok || region == "" {
			continue
		}
		byRegion[region] = append(byRegion[region], ServerEntry{
			ID: id, IP: ip, Region: region, Raw: line,
		})
	}
	return byRegion
}

func EntriesByRegion() map[string][]ServerEntry { return entriesByRegion }

func serversByRegion() map[string]int {
	counts := make(map[string]int)
	for _, e := range allServerEntries {
		if e.region != "" {
			counts[e.region]++
		}
	}
	return counts
}

func Dialers() map[string]*Dialer {
	regionDialersMu.Lock()
	defer regionDialersMu.Unlock()
	snapshot := make(map[string]*Dialer, len(regionDialers))
	for id, d := range regionDialers {
		snapshot[id] = d
	}
	return snapshot
}

func registerDialer(d *Dialer, entries []ServerEntry) {
	regionDialersMu.Lock()
	defer regionDialersMu.Unlock()
	regionDialers[d.id] = d
	for _, e := range entries {
		dialerByServerID[e.ID] = d
	}
}

func LoadServers(ctx context.Context, logger *log.Logger) {
	if allServerEntries != nil {
		return
	}
	serverEntryList = loadServerEntries(ctx, logger)
	allServerEntries = parseServerEntries(serverEntryList)
	entriesByRegion = parseEntriesByRegion(serverEntryList)
	if len(allServerEntries) == 0 {
		logger.Printf("Psiphon: no server entries available, provider will be idle")
	}
}

func ServerRegions() map[string]int {
	if allServerEntries == nil {
		return map[string]int{}
	}
	return serversByRegion()
}
