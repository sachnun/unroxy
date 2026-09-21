package core

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
)

var serverEntryList string

var errNotReady = errors.New("psiphon not ready")

const dialAttempts = 3

type Dialer struct {
	id          string
	controller  *psiphon.Controller
	cancel      context.CancelFunc
	tunnelReady atomic.Int32
	targetPool  int
	region      string

	serverEntries map[string]serverInfo
}

func exitFor(host string) *exitInfo {
	v, ok := globalHostTunnels.Load(host)
	if !ok {
		return nil
	}
	return v.(*exitInfo)
}

func NewState(d *Dialer) *ProxyState {
	if d == nil {
		return nil
	}
	return &ProxyState{
		Key:         "psiphon://" + d.id,
		URL:         &url.URL{Scheme: "psiphon", Host: d.id},
		DialContext: d.DialContext,
		Country:     d.region,
		Tunnel:      d,
	}
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

func NewDialer(id, region string, entries []ServerEntry, logger *log.Logger) (*Dialer, error) {
	dataDir := "/tmp/unroxy-psiphon-" + id

	dsDir := dataDir + "/ca.Tunnel.PsiphonTunnel.tunnel-core/datastore"
	if err := os.MkdirAll(dsDir, 0755); err != nil {
		return nil, err
	}

	byID := make(map[string]serverInfo, len(entries))
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		byID[e.ID] = serverInfo{ip: e.IP, region: e.Region}
		lines = append(lines, e.Raw)
	}

	d := &Dialer{
		id:            id,
		targetPool:    len(entries),
		region:        region,
		serverEntries: byID,
	}

	pc := buildPsiphonConfig(dataDir, len(entries), region)
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
	d.cancel = cancel

	if err := storeRemoteServerEntries(ctx, config, strings.Join(lines, "\n")); err != nil {
		logger.Printf("Psiphon store server entries warning: %v", err)
	}

	controller, err := psiphon.NewController(config)
	if err != nil {
		cancel()
		return nil, err
	}
	d.controller = controller

	registerDialer(d, entries)

	go controller.Run(ctx)

	refreshInterval := 30 * time.Minute
	refreshCount := max(1, len(entries)/3)
	d.startTunnelRefresh(ctx, refreshInterval, refreshCount, logger)

	return d, nil
}

func (d *Dialer) Region() string  { return d.region }
func (d *Dialer) TargetPool() int { return d.targetPool }
func (d *Dialer) IsReady() bool   { return d.tunnelReady.Load() > 0 }

func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.tunnelReady.Load() == 0 && d.targetPool > 0 {
		return nil, errNotReady
	}
	var lastErr error
	for i := 0; i < dialAttempts; i++ {
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
					globalHostTunnels.Store(host, &exitInfo{ip: e.ip, region: e.region, protocol: proto})
					break
				}
			}
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *Dialer) startTunnelRefresh(ctx context.Context, interval time.Duration, count int, logger *log.Logger) {
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
