// Package main embeds the Psiphon tunnel core for the Rust proxy.
//
// The tunnel stays in Go. Rust reaches it through a SOCKS5 listener per
// tunnel, which keeps Rust free to own its own TLS and HTTP stacks while Go
// still knows which server carried each connection.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

type exitInfo struct {
	IP       string `json:"ip"`
	Region   string `json:"region"`
	Protocol string `json:"protocol"`
}

type tunnel struct {
	controller *psiphon.Controller
	cancel     context.CancelFunc
	listener   net.Listener
	socksPort  int
	region     string
	target     int

	mu        sync.Mutex
	exits     map[string]exitInfo
	terminate int
	active    int
}

var (
	tunnelCountInterval = 5 * time.Second

	mu             sync.Mutex
	tunnels        = map[C.int]*tunnel{}
	nextID         C.int
	lastErr        string
	diagnosticByIP = map[string]string{}
	protocolByID   = map[string]string{}
	noticeCounts   = map[string]int{}
)

func setErr(err error) {
	mu.Lock()
	defer mu.Unlock()
	if err == nil {
		lastErr = ""
		return
	}
	lastErr = err.Error()
}

//export PsiLastError
func PsiLastError() *C.char {
	mu.Lock()
	defer mu.Unlock()
	return C.CString(lastErr)
}

//export PsiFree
func PsiFree(ptr *C.char) {
	C.free(unsafePointer(ptr))
}

// PsiStart loads the config, stores the server entries, runs a controller and
// listens for SOCKS5 connections. The returned handle is used by every other
// call; zero means failure.
//
//export PsiStart
func PsiStart(cConfigJSON, cServerEntries *C.char) C.int {
	configJSON := C.GoString(cConfigJSON)
	serverEntries := C.GoString(cServerEntries)

	config, err := psiphon.LoadConfig([]byte(configJSON))
	if err != nil {
		setErr(err)
		return 0
	}
	if err := config.Commit(true); err != nil {
		setErr(err)
		return 0
	}
	if err := psiphon.OpenDataStore(config); err != nil {
		setErr(err)
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	if serverEntries != "" {
		err := psiphon.StreamingStoreServerEntries(
			ctx,
			config,
			protocol.NewStreamingServerEntryDecoder(
				strings.NewReader(serverEntries),
				common.TruncateTimestampToHour(common.GetCurrentTimestamp()),
				protocol.SERVER_ENTRY_SOURCE_REMOTE),
			true)
		if err != nil {
			cancel()
			psiphon.CloseDataStore()
			setErr(err)
			return 0
		}
		indexServerEntries(serverEntries)
	}

	controller, err := psiphon.NewController(config)
	if err != nil {
		cancel()
		psiphon.CloseDataStore()
		setErr(err)
		return 0
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		psiphon.CloseDataStore()
		setErr(err)
		return 0
	}

	active := &tunnel{
		controller: controller,
		cancel:     cancel,
		listener:   listener,
		socksPort:  listener.Addr().(*net.TCPAddr).Port,
		region:     config.EgressRegion,
		target:     config.TunnelPoolSize,
		exits:      map[string]exitInfo{},
	}

	mu.Lock()
	nextID++
	id := nextID
	tunnels[id] = active
	mu.Unlock()

	go controller.Run(ctx)
	go active.serveSocks()
	go active.trackTunnels(ctx)

	setErr(nil)
	return id
}

// indexServerEntries maps server IPs to diagnostic IDs. Notices redact server
// addresses, so the entry list is the only source for that mapping.
func indexServerEntries(serverEntries string) {
	timestamp := common.TruncateTimestampToHour(common.GetCurrentTimestamp())
	decoder := protocol.NewStreamingServerEntryDecoder(
		strings.NewReader(serverEntries), timestamp, protocol.SERVER_ENTRY_SOURCE_REMOTE)

	mu.Lock()
	defer mu.Unlock()
	for {
		fields, err := decoder.Next()
		if err != nil || fields == nil {
			return
		}
		diagnosticByIP[fields.GetIPAddress()] = fields.GetDiagnosticID()
	}
}

// PsiState reports tunnel counters and the SOCKS port as JSON.
//
// The counts come from counters the controller's notices keep up to date, so a
// status request costs a mutex and an encoding rather than a walk over the
// controller's tunnel list.
//
//export PsiState
func PsiState(handle C.int) *C.char {
	mu.Lock()
	active := tunnels[handle]
	mu.Unlock()
	if active == nil {
		return C.CString(`{"active":0,"connected":0,"socks_port":0}`)
	}

	active.mu.Lock()
	state := struct {
		Active    int            `json:"active"`
		Connected int            `json:"connected"`
		SocksPort int            `json:"socks_port"`
		Notices   map[string]int `json:"notices"`
	}{active.active, len(active.exits), active.socksPort, noticeCounts}
	active.mu.Unlock()

	encoded, err := json.Marshal(state)
	if err != nil {
		return C.CString(`{"active":0,"connected":0,"socks_port":0}`)
	}
	return C.CString(string(encoded))
}

// PsiExitFor reports the exit that served the most recent connection to host.
// An empty object means the host has not been dialed yet.
//
//export PsiExitFor
func PsiExitFor(handle C.int, cHost *C.char) *C.char {
	mu.Lock()
	active := tunnels[handle]
	mu.Unlock()
	if active == nil {
		return C.CString("{}")
	}

	active.mu.Lock()
	info, ok := active.exits[strings.ToLower(C.GoString(cHost))]
	active.mu.Unlock()
	if !ok {
		return C.CString("{}")
	}

	encoded, err := json.Marshal(info)
	if err != nil {
		return C.CString("{}")
	}
	return C.CString(string(encoded))
}

// PsiTerminate asks the controller to retire up to count active tunnels so it
// builds fresh ones, which is how exits get rotated. Retirement is applied on
// the next connection because the controller only terminates safely from the
// dial path.
//
//export PsiTerminate
func PsiTerminate(handle C.int, count C.int) {
	mu.Lock()
	active := tunnels[handle]
	mu.Unlock()
	if active == nil {
		return
	}
	active.mu.Lock()
	active.terminate += int(count)
	active.mu.Unlock()
}

//export PsiStop
func PsiStop(handle C.int) {
	mu.Lock()
	active := tunnels[handle]
	delete(tunnels, handle)
	mu.Unlock()
	if active == nil {
		return
	}
	active.listener.Close()
	active.cancel()
	psiphon.CloseDataStore()
}

// currentTunnel returns the single tunnel the process is running. The notice
// receiver is global while the tunnel handle is not, and the server runs one
// region per process, so this is the only tunnel there is.
func currentTunnel() *tunnel {
	for _, active := range tunnels {
		return active
	}
	return nil
}

// trackTunnels refreshes the active count from the controller on a timer. The
// count is what the index page and the tunnel refresh loop read, so it is kept
// here rather than walked per request.
func (t *tunnel) trackTunnels(ctx context.Context) {
	ticker := time.NewTicker(tunnelCountInterval)
	defer ticker.Stop()
	for {
		count := t.controllerTunnelCount()
		t.mu.Lock()
		t.active = count
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// controllerTunnelCount reads the controller's live tunnel slice. The
// controller exposes no accessor, so this is the only source for the count.
func (t *tunnel) controllerTunnelCount() int {
	value := reflect.ValueOf(t.controller)
	if value.Kind() != reflect.Ptr || value.IsNil() {
		return 0
	}
	value = value.Elem()
	if value.Kind() != reflect.Struct {
		return 0
	}
	slice := value.FieldByName("tunnels")
	if !slice.IsValid() || slice.Kind() != reflect.Slice {
		return 0
	}
	return slice.Len()
}

func (t *tunnel) pendingTerminations() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := t.terminate
	t.terminate = 0
	return count
}

func (t *tunnel) recordExit(destination string, conn net.Conn) {
	ip := serverIP(conn)

	mu.Lock()
	diagnosticID := diagnosticByIP[ip]
	protocol := protocolByID[diagnosticID]
	mu.Unlock()

	host, _, err := net.SplitHostPort(destination)
	if err != nil {
		host = destination
	}

	t.mu.Lock()
	t.exits[strings.ToLower(host)] = exitInfo{
		IP:       ip,
		Region:   t.region,
		Protocol: protocol,
	}
	t.mu.Unlock()
}

// serverIP reads the server address off the connection chain. The controller
// wraps every tunneled connection in a stats connection that carries it.
func serverIP(conn net.Conn) string {
	value := reflect.ValueOf(conn)
	for value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	field := value.FieldByName("serverID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return ""
	}
	return field.String()
}

func (t *tunnel) serveSocks() {
	for {
		client, err := t.listener.Accept()
		if err != nil {
			return
		}
		go t.handleSocks(client)
	}
}

func (t *tunnel) handleSocks(client net.Conn) {
	defer client.Close()

	destination, err := readSocksConnect(client)
	if err != nil {
		return
	}

	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	for i := 0; i < t.pendingTerminations(); i++ {
		t.controller.TerminateNextActiveTunnel()
	}

	remote, err := t.controller.Dial(destination, nil)
	if err != nil {
		setErr(err)
		return
	}
	defer remote.Close()

	t.recordExit(destination, remote)
	relay(remote, client)
}

// readSocksConnect performs the SOCKS5 greeting and CONNECT handshake,
// returning the requested destination as "host:port".
func readSocksConnect(client net.Conn) (string, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(client, header); err != nil {
		return "", err
	}
	if header[0] != 0x05 {
		return "", fmt.Errorf("socks5: unsupported version %d", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return "", err
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return "", err
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(client, request); err != nil {
		return "", err
	}
	if request[0] != 0x05 || request[1] != 0x01 {
		return "", fmt.Errorf("socks5: unsupported request %d/%d", request[0], request[1])
	}

	var host string
	switch request[3] {
	case 0x01:
		address := make([]byte, 4)
		if _, err := io.ReadFull(client, address); err != nil {
			return "", err
		}
		host = net.IP(address).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(client, length); err != nil {
			return "", err
		}
		address := make([]byte, int(length[0]))
		if _, err := io.ReadFull(client, address); err != nil {
			return "", err
		}
		host = string(address)
	case 0x04:
		address := make([]byte, 16)
		if _, err := io.ReadFull(client, address); err != nil {
			return "", err
		}
		host = net.IP(address).String()
	default:
		return "", fmt.Errorf("socks5: unsupported address type %d", request[3])
	}

	port := make([]byte, 2)
	if _, err := io.ReadFull(client, port); err != nil {
		return "", err
	}

	return net.JoinHostPort(host, fmt.Sprint(int(port[0])<<8|int(port[1]))), nil
}

func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(a, b)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(b, a)
		done <- struct{}{}
	}()
	<-done
}

// main is never called in c-archive mode; init registers the notice receiver
// so the controller's diagnostics reach handleNotice.
func main() {}

func init() {
	psiphon.SetNoticeWriter(psiphon.NewNoticeReceiver(handleNotice))
}

func handleNotice(notice []byte) {
	var message struct {
		Type string `json:"noticeType"`
		Data struct {
			DiagnosticID string `json:"diagnosticID"`
			Protocol     string `json:"protocol"`
			Count        int    `json:"count"`
		} `json:"data"`
	}
	if json.Unmarshal(notice, &message) != nil {
		return
	}
	mu.Lock()
	noticeCounts[message.Type]++
	active := currentTunnel()
	if message.Type == "ConnectedServer" && message.Data.DiagnosticID != "" && message.Data.Protocol != "" {
		protocolByID[message.Data.DiagnosticID] = message.Data.Protocol
	}
	mu.Unlock()

	// ActiveTunnel fires as soon as a tunnel is usable, so it makes readiness
	// visible immediately. The periodic count is authoritative and corrects
	// any drift, including tunnels retired without a notice of their own.
	if message.Type == "ActiveTunnel" && active != nil {
		active.mu.Lock()
		if active.active < active.target {
			active.active++
		}
		active.mu.Unlock()
	}
}
