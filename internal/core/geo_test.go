package core

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestCountryForIP(t *testing.T) {
	resetGeoCache()

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/203.0.113.9" {
			http.NotFound(w, r)
			return
		}
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
	// Cached: no additional server hit.
	if got := countryForIP(ctx, "203.0.113.9"); got != "SG" {
		t.Fatalf("cached countryForIP = %q, want SG", got)
	}
	if hits != 1 {
		t.Fatalf("expected 1 server hit, got %d", hits)
	}
}

func TestCountryForIPRejectsInvalid(t *testing.T) {
	resetGeoCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bad":
			w.Write([]byte(`{"success":false,"country_code":"US"}`))
		case "/notalpha":
			w.Write([]byte(`{"success":true,"country_code":"S1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	swapIPWhoisBase(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got := countryForIP(ctx, "bad"); got != "" {
		t.Fatalf("countryForIP(success:false) = %q, want empty", got)
	}
	if got := countryForIP(ctx, "notalpha"); got != "" {
		t.Fatalf("countryForIP(non-alpha) = %q, want empty", got)
	}
}

func TestISPForIPUsesOrgFallback(t *testing.T) {
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
