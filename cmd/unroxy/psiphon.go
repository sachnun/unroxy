package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
)

//go:embed server_entries.txt
var embeddedServerList string

var errPsiphonNotReady = errors.New("psiphon not ready")

const psiphonRetryCount = 3

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

func NewPsiphonDialer(region string, poolSize int, logger *log.Logger) (*PsiphonDialer, error) {
	if allServerEntries == nil {
		allServerEntries = parseServerEntries(embeddedServerList)
	}

	dataDir := "/tmp/unroxy-psiphon"
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

	refreshInterval := 30 * time.Minute
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
