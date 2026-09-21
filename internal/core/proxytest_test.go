package core

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type upstreamProxy struct {
	*httptest.Server

	mu       sync.Mutex
	requests []string
	bodies   []string
	connects []string

	addHeader http.Header
	onRequest func(r *http.Request) (status int, handled bool)

	client *http.Client
}

func newUpstreamProxy() *upstreamProxy {
	p := &upstreamProxy{
		client: &http.Client{Transport: &http.Transport{}},
	}
	p.Server = httptest.NewServer(http.HandlerFunc(p.handle))
	return p
}

func (p *upstreamProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.mu.Lock()
		p.connects = append(p.connects, r.Host)
		p.mu.Unlock()
		p.tunnel(w, r)
		return
	}

	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+r.URL.String())
	p.bodies = append(p.bodies, string(body))
	respond := p.onRequest
	extra := p.addHeader.Clone()
	p.mu.Unlock()

	if respond != nil {
		if status, handled := respond(r); handled {
			w.WriteHeader(status)
			return
		}
	}

	out, err := http.NewRequest(r.Method, r.URL.String(), strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	for k, values := range extra {
		out.Header[k] = values
	}

	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *upstreamProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	target, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer target.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go io.Copy(target, client)
	io.Copy(client, target)
}

func (p *upstreamProxy) proxyState(t *testing.T) *ProxyState {
	t.Helper()
	return proxyStateURL(t, p.URL, p.URL)
}

func (p *upstreamProxy) sawBody(body string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.bodies {
		if b == body {
			return true
		}
	}
	return false
}

func proxyStateURL(t *testing.T, key, rawURL string) *ProxyState {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	return &ProxyState{Key: key, URL: parsed}
}

func netDial(rawURL string) (net.Conn, error) {
	return net.Dial("tcp", strings.TrimPrefix(rawURL, "http://"))
}

func newEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	return listener.Addr().String()
}

func testTransport(states ...*ProxyState) *RotatingProxyTransport {
	return NewRotatingProxyTransport(NewProxyPool(log.New(io.Discard, "", 0), states))
}

func testHandler(t *testing.T, transport http.RoundTripper) *httptest.Server {
	t.Helper()
	router := NewPoolRouter(nil, transport)
	srv := httptest.NewServer(NewProxyHandler(log.New(io.Discard, "", 0), router))
	t.Cleanup(srv.Close)
	return srv
}

func proxyClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", srv.URL, err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
}
