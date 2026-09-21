package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func resetGeoCache() {
	ispCacheMu.Lock()
	ispCache = make(map[string]string)
	countryCache = make(map[string]string)
	ispCacheMu.Unlock()
}

func swapIPWhoisBase(t *testing.T, url string) {
	t.Helper()
	old := ipWhoisBase
	ipWhoisBase = url
	t.Cleanup(func() { ipWhoisBase = old })
}

func TestCountryForIPFetchesAndCaches(t *testing.T) {
	resetGeoCache()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"country_code":"sg","connection":{"isp":"ExampleNet","org":"ExampleOrg"}}`))
	}))
	defer srv.Close()
	swapIPWhoisBase(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got := countryForIP(ctx, "203.0.113.9"); got != "SG" {
		t.Fatalf("countryForIP = %q, want SG", got)
	}
	if got := countryForIP(ctx, "203.0.113.9"); got != "SG" {
		t.Fatalf("cached countryForIP = %q, want SG", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
}

func TestWhoisForIPUsesOrgFallback(t *testing.T) {
	resetGeoCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"country_code":"US","connection":{"isp":"","org":"FallbackOrg"}}`))
	}))
	defer srv.Close()
	swapIPWhoisBase(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got := ispForIP(ctx, "198.51.100.4"); got != "FallbackOrg" {
		t.Fatalf("ispForIP = %q, want FallbackOrg", got)
	}
}

func TestWhoisForIPRejectsInvalidResponses(t *testing.T) {
	resetGeoCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/failed":
			w.Write([]byte(`{"success":false,"country_code":"US"}`))
		case "/short":
			w.Write([]byte(`{"success":true,"country_code":"S1"}`))
		default:
			w.Write([]byte(`not json`))
		}
	}))
	defer srv.Close()
	swapIPWhoisBase(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, ip := range []string{"failed", "short", "garbage"} {
		if got := countryForIP(ctx, ip); got != "" {
			t.Fatalf("countryForIP(%q) = %q, want empty", ip, got)
		}
	}
}

func TestWhoisForIPSkipsEmptyAddress(t *testing.T) {
	resetGeoCache()

	if got := countryForIP(context.Background(), ""); got != "" {
		t.Fatalf("countryForIP('') = %q, want empty", got)
	}
	if got := ispForIP(context.Background(), ""); got != "" {
		t.Fatalf("ispForIP('') = %q, want empty", got)
	}
}
