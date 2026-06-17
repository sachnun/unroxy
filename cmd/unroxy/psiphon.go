package main

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

//go:embed server_entries.txt
var embeddedServerList string

var errPsiphonNotReady = errors.New("psiphon not ready")

const psiphonRetryCount = 3

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
	globalHostTunnels sync.Map // host -> *tunnelInfo (shared all dialers)
)

type PsiphonDialer struct {
	controller  *psiphon.Controller
	ctx         context.Context
	cancel      context.CancelFunc
	once        sync.Once
	tunnelReady atomic.Int32
	targetPool  int
	region      string

	serverEntries map[string]serverEntryInfo
}

func TunnelInfoForHost(host string) *tunnelInfo {
	v, ok := globalHostTunnels.Load(host)
	if !ok {
		return nil
	}
	return v.(*tunnelInfo)
}

func serverIDFromConn(conn net.Conn) string {
	v := reflect.ValueOf(conn)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	f := v.FieldByName("serverID")
	if f.IsValid() {
		return f.String()
	}
	if v.Kind() == reflect.Struct {
		for i := 0; i < v.NumField(); i++ {
			fv := v.Field(i)
			if fv.Kind() == reflect.Interface || fv.Kind() == reflect.Ptr {
				if inner := fv.Elem(); inner.IsValid() && inner.Kind() == reflect.Ptr {
					inner = inner.Elem()
					if sf := inner.FieldByName("serverID"); sf.IsValid() {
						return sf.String()
					}
				}
			}
		}
	}
	return ""
}

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

func initPsiphonNoticeHandler(logger *log.Logger) {
	psiphon.SetNoticeWriter(psiphon.NewNoticeReceiver(func(notice []byte) {
		var msg struct {
			Type string `json:"noticeType"`
			Data struct {
				DiagnosticID string `json:"diagnosticID"`
				Protocol     string `json:"protocol"`
			} `json:"data"`
		}
		if json.Unmarshal(notice, &msg) != nil {
			return
		}

		if msg.Type == "ConnectedServer" {
			if entry, ok := allServerEntries[msg.Data.DiagnosticID]; ok {
				if msg.Data.Protocol != "" {
					protocolByIP.Store(entry.ip, msg.Data.Protocol)
				}
			}
		}

		if msg.Type == "ActiveTunnel" {
			if entry, ok := allServerEntries[msg.Data.DiagnosticID]; ok {
				if d, ok := regionDialers[entry.region]; ok {
					n := d.tunnelReady.Add(1)
					if int(n) == d.targetPool {
						logger.Printf("Psiphon [%s]: %d/%d tunnels ready", entry.region, n, d.targetPool)
					}
				}
			}
		}
	}))
}

func NewPsiphonDialer(region string, poolSize int, logger *log.Logger) (*PsiphonDialer, error) {
	if allServerEntries == nil {
		allServerEntries = parseServerEntries(embeddedServerList)
	}

	dataDir := envOrDefault("PSIPHON_DATA_DIR", "/tmp/unroxy-psiphon")
	if region != "" {
		dataDir += "-" + region
	}

	dsDir := dataDir + "/ca.psiphon.PsiphonTunnel.tunnel-core/datastore"
	if err := os.MkdirAll(dsDir, 0755); err != nil {
		return nil, err
	}

	minIdle := max(0, poolSize-1)
	maxTunnels := poolSize

	d := &PsiphonDialer{
		targetPool:    poolSize,
		region:        region,
		serverEntries: allServerEntries,
	}

	regionDialers[region] = d

	pc := buildPsiphonConfig(dataDir, poolSize, minIdle, maxTunnels, region)
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
	d.ctx = ctx
	d.cancel = cancel

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
	d.controller = controller

	go controller.Run(ctx)

	refreshInterval := envDuration("PSIPHON_REFRESH_INTERVAL", 30*time.Minute)
	refreshCount := max(1, poolSize/3)
	d.startTunnelRefresh(ctx, refreshInterval, refreshCount, logger)

	return d, nil
}

func (d *PsiphonDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.tunnelReady.Load() == 0 && d.targetPool > 0 {
		return nil, errPsiphonNotReady
	}
	var lastErr error
	for i := 0; i < psiphonRetryCount; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		conn, err := d.controller.Dial(addr, nil)
		if err == nil {
			host, _, _ := net.SplitHostPort(addr)
			serverIP := serverIDFromConn(conn)
			for _, e := range d.serverEntries {
				if e.ip == serverIP {
					proto := ""
					if v, ok := protocolByIP.Load(serverIP); ok {
						proto, _ = v.(string)
					}
					globalHostTunnels.Store(host, &tunnelInfo{ip: e.ip, region: e.region, protocol: proto})
					break
				}
			}
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *PsiphonDialer) Close() {
	d.once.Do(func() { d.cancel() })
}

func (d *PsiphonDialer) startTunnelRefresh(ctx context.Context, interval time.Duration, count int, logger *log.Logger) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i := 0; i < count; i++ {
					d.controller.TerminateNextActiveTunnel()
				}
			}
		}
	}()
}

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
