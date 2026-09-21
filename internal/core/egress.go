package core

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

var (
	ispCacheMu   sync.Mutex
	ispCache     = make(map[string]string)
	countryCache = make(map[string]string)
)

var ipWhoisBase = "https://ipwho.is"

func whoisForIP(ctx context.Context, ip string) (isp, cc string) {
	if ip == "" {
		return "", ""
	}

	ispCacheMu.Lock()
	isp, ispOK := ispCache[ip]
	cc, ccOK := countryCache[ip]
	ispCacheMu.Unlock()
	if ispOK && ccOK {
		return isp, cc
	}

	client := newHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipWhoisBase+"/"+ip, nil)
	if err != nil {
		return "", ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	var out struct {
		Success     bool   `json:"success"`
		CountryCode string `json:"country_code"`
		Connection  struct {
			ISP string `json:"isp"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", ""
	}

	isp = out.Connection.ISP
	if isp == "" {
		isp = out.Connection.Org
	}
	if out.Success {
		cc = strings.ToUpper(strings.TrimSpace(out.CountryCode))
		if len(cc) != 2 {
			cc = ""
		} else {
			for i := 0; i < len(cc); i++ {
				if cc[i] < 'A' || cc[i] > 'Z' {
					cc = ""
					break
				}
			}
		}
	}

	ispCacheMu.Lock()
	if isp != "" {
		ispCache[ip] = isp
	}
	if cc != "" {
		countryCache[ip] = cc
	}
	ispCacheMu.Unlock()
	return isp, cc
}

func ispForIP(ctx context.Context, ip string) string {
	isp, _ := whoisForIP(ctx, ip)
	return isp
}

func countryForIP(ctx context.Context, ip string) string {
	_, cc := whoisForIP(ctx, ip)
	return cc
}
