package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	"golang.org/x/net/proxy"
)

type usqueInstance struct {
	cmd       *exec.Cmd
	socksPort string
}

func startWarpUsque(port, fwdPort, configPath string, psiphonDial func(ctx context.Context, network, addr string) (net.Conn, error), logger *log.Logger) (*usqueInstance, proxy.ContextDialer, error) {
	startMasqueProxy(psiphonDial, fwdPort, logger)

	cfg := "/tmp/usque-" + port + ".json"
	exec.Command("cp", configPath, cfg).Run()
	patchConfigEndpoint(cfg, "127.0.0.1")

	cmd := exec.Command("./usque", "-c", cfg,
		"socks", "-p", port, "-b", "127.0.0.1",
		"--http2", "-P", fwdPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("usque start: %w", err)
	}

	u := &usqueInstance{cmd: cmd, socksPort: port}
	dialer, err := u.waitReady(logger)
	if err != nil {
		u.close()
		return nil, nil, err
	}
	return u, dialer, nil
}

func (u *usqueInstance) waitReady(logger *log.Logger) (proxy.ContextDialer, error) {
	addr := "127.0.0.1:" + u.socksPort

	for range 60 {
		d, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		ctxDialer, ok := d.(proxy.ContextDialer)
		if !ok {
			time.Sleep(time.Second)
			continue
		}
		conn, err := ctxDialer.DialContext(context.Background(), "tcp", "1.1.1.1:80")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		conn.Close()
		logger.Printf("WARP: usque ready on :%s", u.socksPort)
		return ctxDialer, nil
	}
	return nil, fmt.Errorf("usque socks timeout")
}

func findUsqueConfig() (string, error) {
	for _, p := range []string{"/root/config.json", "/tmp/config.json", "config.json"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no usque config.json found")
}

func patchConfigEndpoint(path, endpoint string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := bytes.ReplaceAll(data,
		[]byte(`"endpoint_v4": "162.159.198.2"`),
		[]byte(`"endpoint_v4": "`+endpoint+`"`))
	content = bytes.ReplaceAll(content,
		[]byte(`"endpoint_h2_v4": "162.159.198.2"`),
		[]byte(`"endpoint_h2_v4": "`+endpoint+`"`))
	os.WriteFile(path, content, 0644)
}

func (u *usqueInstance) close() {
	if u.cmd != nil && u.cmd.Process != nil {
		u.cmd.Process.Signal(os.Interrupt)
	}
}
