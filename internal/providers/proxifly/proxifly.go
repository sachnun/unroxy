// Package proxifly implements the Proxifly free-proxy-list provider.
// The list is fetched as CSV, health-checked, grouped by country, and
// contributed to the default and per-country pools.
package proxifly

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
	"h12.io/socks"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

// CSVURL is the Proxifly dataset URL. Overridable in tests.
var CSVURL = "https://raw.githubusercontent.com/proxifly/free-proxy-list/refs/heads/main/proxies/all/data.csv"

// Provider fetches, health-checks and maintains Proxifly proxies.
type Provider struct {
	host     *providers.Host
	logger   *log.Logger
	lastETag string
}

func init() {
	providers.Register(&Provider{})
}

func (p *Provider) Name() string { return "Proxifly" }

func (p *Provider) Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	p.host = host
	p.logger = logger

	if etag, err := fetchETag(); err == nil {
		p.lastETag = etag
	}

	proxies, err := fetch()
	if err != nil {
		logger.Printf("Proxifly proxy not ready: %v", err)
		return err
	}
	p.distribute(proxies)
	return nil
}

// Refresh re-fetches only when the remote ETag changes.
func (p *Provider) Refresh(ctx context.Context) error {
	etag, err := fetchETag()
	if err != nil {
		return err
	}
	if etag == p.lastETag {
		p.logger.Printf("Proxifly: no change")
		return nil
	}

	proxies, err := fetch()
	if err != nil {
		return err
	}
	p.distribute(proxies)
	p.lastETag = etag
	return nil
}

func (p *Provider) distribute(proxies []*core.ProxyState) {
	p.logger.Printf("Proxifly: %d proxies fetched", len(proxies))
	for _, ps := range proxies {
		ps.Priority = 1
	}
	// The background validator probes every proxy; only the healthy ones
	// are graduated into the pools.
	p.host.Submit(proxies)
	p.logger.Printf("Proxifly: %d proxies queued for validation", len(proxies))
}

func fetchETag() (string, error) {
	client := core.NewProviderHTTPClient()
	req, err := http.NewRequest(http.MethodHead, CSVURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("no ETag for proxifly CSV")
	}
	return etag, nil
}

// fetch downloads and parses the Proxifly CSV into proxy states. Only
// entries with a usable dial context are returned.
func fetch() ([]*core.ProxyState, error) {
	client := core.NewProviderHTTPClient()

	resp, err := client.Get(CSVURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("proxifly CSV returned status %d", resp.StatusCode)
	}

	reader := csv.NewReader(resp.Body)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to read proxifly CSV: %w", err)
	}

	states := make([]*core.ProxyState, 0, len(records))
	for _, row := range records {
		if len(row) < 2 {
			continue
		}
		rawURL := strings.TrimSpace(row[0])
		country := strings.ToUpper(strings.TrimSpace(row[1]))
		if country == "" {
			country = "XX"
		}

		parsedURL, err := url.Parse(rawURL)
		if err != nil {
			continue
		}

		state := &core.ProxyState{
			Key:     rawURL,
			URL:     parsedURL,
			Country: country,
		}

		switch parsedURL.Scheme {
		case "socks5", "socks5h":
			d, err := proxy.FromURL(parsedURL, proxy.Direct)
			if err == nil {
				state.DialContext = d.(proxy.ContextDialer).DialContext
			}
		case "socks4", "socks4a":
			d := socks.Dial(rawURL)
			state.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				type dialResult struct {
					conn net.Conn
					err  error
				}
				ch := make(chan dialResult, 1)
				go func() {
					conn, err := d(network, addr)
					ch <- dialResult{conn, err}
				}()
				select {
				case r := <-ch:
					return r.conn, r.err
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		if state.DialContext != nil {
			states = append(states, state)
		}
	}

	if len(states) == 0 {
		return nil, errors.New("no proxifly proxies fetched")
	}

	return states, nil
}
