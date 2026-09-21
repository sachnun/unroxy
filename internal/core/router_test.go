package core

import (
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestAuthUsernameReadsAuthorizationHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "authorization", headers: map[string]string{"Authorization": basicHeader("user", "pass")}, want: "user"},
		{name: "proxy authorization", headers: map[string]string{"Proxy-Authorization": basicHeader("user", "pass")}, want: "user"},
		{name: "username only", headers: map[string]string{"Proxy-Authorization": basicHeader("us", "")}, want: "us"},
		{name: "invalid base64", headers: map[string]string{"Proxy-Authorization": "Basic !!!"}, want: ""},
		{name: "wrong scheme", headers: map[string]string{"Proxy-Authorization": "Bearer token"}, want: ""},
		{name: "absent", headers: map[string]string{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			if got := authUsername(req); got != tt.want {
				t.Fatalf("authUsername = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRouterSelectIsCaseInsensitive(t *testing.T) {
	usTransport := testTransport()
	router := NewPoolRouter([]*NamedPool{
		{Name: "US", Username: "us", Transport: usTransport},
	}, testTransport())

	if got := router.Select("US"); got != usTransport {
		t.Fatalf("Select(US) = %v, want us transport", got)
	}
	if got := router.Select("missing"); got != nil {
		t.Fatalf("Select(missing) = %v, want nil", got)
	}
	if got := router.Select(""); got != nil {
		t.Fatalf("Select('') = %v, want nil", got)
	}
}

func TestRouterStatsCountProxies(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "US", Pool: NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{
			proxyStateURL(t, "a", "http://1.1.1.1:80"),
			proxyStateURL(t, "b", "http://2.2.2.2:80"),
		})},
		{Name: "DE", Pool: NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{
			proxyStateURL(t, "c", "http://3.3.3.3:80"),
		})},
	}, nil)

	stats := router.Stats()
	if stats.TotalProxies != 3 {
		t.Fatalf("TotalProxies = %d, want 3", stats.TotalProxies)
	}
	if len(stats.Pools) != 2 || stats.Pools[0].Name != "US" || stats.Pools[0].ProxyCount != 2 {
		t.Fatalf("Pools = %+v, want US(2) and DE(1)", stats.Pools)
	}
}
