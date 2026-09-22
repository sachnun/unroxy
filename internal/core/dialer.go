package core

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
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

const MaxTunnelsPerRegion = 3

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
		targetPool:    min(len(entries), MaxTunnelsPerRegion),
		region:        region,
		serverEntries: byID,
	}

	pc := buildPsiphonConfig(dataDir, d.targetPool, region)
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

	refreshInterval := 10 * time.Minute
	refreshCount := 1
	d.startTunnelRefresh(ctx, refreshInterval, refreshCount, logger)

	return d, nil
}

func (d *Dialer) Region() string  { return d.region }
func (d *Dialer) TargetPool() int { return d.targetPool }
func (d *Dialer) IsReady() bool   { return d.tunnelReady.Load() > 0 }

func (d *Dialer) ActiveTunnels() int {
	if d == nil || d.controller == nil {
		return 0
	}
	v := reflect.ValueOf(d.controller)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return 0
	}
	tunnels := v.Elem().FieldByName("tunnels")
	if !tunnels.IsValid() || tunnels.Kind() != reflect.Slice {
		return 0
	}
	return tunnels.Len()
}

func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.tunnelReady.Load() == 0 && d.targetPool > 0 {
		return nil, errNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	conn, err := d.controller.Dial(addr, nil)
	if err != nil {
		return nil, err
	}

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

func (d *Dialer) startTunnelRefresh(ctx context.Context, interval time.Duration, count int, logger *log.Logger) {
	go func() {
		initial := time.NewTimer(time.Duration(rand.Int63n(int64(interval))))
		defer initial.Stop()
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if d.ActiveTunnels() < d.targetPool {
					continue
				}
				for i := 0; i < count; i++ {
					d.controller.TerminateNextActiveTunnel()
				}
			}
		}
	}()
}
