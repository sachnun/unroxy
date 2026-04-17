package main

import (
	"bytes"
	"log"
	"reflect"
	"strings"
	"testing"
)

func TestProxyEnabled(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		enabled bool
	}{
		{name: "empty is disabled", value: "", enabled: false},
		{name: "none is disabled", value: "none", enabled: false},
		{name: "none mixed with http is disabled", value: "none,http", enabled: false},
		{name: "all is enabled", value: "all", enabled: true},
		{name: "sock is enabled", value: "sock", enabled: true},
		{name: "http is enabled", value: "http", enabled: true},
		{name: "csv is enabled", value: "sock,http", enabled: true},
		{name: "trimmed all is enabled", value: " all ", enabled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proxyEnabled(tt.value); got != tt.enabled {
				t.Fatalf("proxyEnabled(%q) = %v, want %v", tt.value, got, tt.enabled)
			}
		})
	}
}

func TestParseProxyProtocols(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected []string
	}{
		{name: "disabled when empty", value: "", expected: nil},
		{name: "disabled when none", value: "none", expected: nil},
		{name: "all expands all protocols", value: "all", expected: []string{"http", "https", "socks", "socks5"}},
		{name: "sock expands socks protocols", value: "sock", expected: []string{"socks", "socks5"}},
		{name: "http expands http protocols", value: "http", expected: []string{"http", "https"}},
		{name: "csv merges protocols", value: "sock,http", expected: []string{"http", "https", "socks", "socks5"}},
		{name: "none mixed with http stays disabled", value: "none,http", expected: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sortedProxyProtocols(parseProxyProtocols(tt.value))
			if len(got) != len(tt.expected) {
				t.Fatalf("parseProxyProtocols(%q) = %v, want %v", tt.value, got, tt.expected)
			}

			for i := range got {
				if got[i] != tt.expected[i] {
					t.Fatalf("parseProxyProtocols(%q) = %v, want %v", tt.value, got, tt.expected)
				}
			}
		})
	}
}

func TestParseProxyProtocolConfigInvalidValues(t *testing.T) {
	allowed, invalid := parseProxyProtocolConfig("http,foo,bar,foo")
	gotAllowed := sortedProxyProtocols(allowed)
	wantAllowed := []string{"http", "https"}
	if !reflect.DeepEqual(gotAllowed, wantAllowed) {
		t.Fatalf("parseProxyProtocolConfig allowed = %v, want %v", gotAllowed, wantAllowed)
	}

	wantInvalid := []string{"foo", "bar"}
	if !reflect.DeepEqual(invalid, wantInvalid) {
		t.Fatalf("parseProxyProtocolConfig invalid = %v, want %v", invalid, wantInvalid)
	}
}

func TestParseProxyProtocolConfigNoneOverridesOtherValues(t *testing.T) {
	allowed, invalid := parseProxyProtocolConfig("none,http,foo")
	if allowed != nil {
		t.Fatalf("parseProxyProtocolConfig allowed = %v, want nil", sortedProxyProtocols(allowed))
	}

	wantInvalid := []string{"foo"}
	if !reflect.DeepEqual(invalid, wantInvalid) {
		t.Fatalf("parseProxyProtocolConfig invalid = %v, want %v", invalid, wantInvalid)
	}
}

func TestNewUpstreamTransportLogsInvalidProxyValues(t *testing.T) {
	t.Setenv("PROXY", "none,foo,bar")

	var logs bytes.Buffer
	transport := newUpstreamTransport(log.New(&logs, "", 0))
	if transport != nil {
		t.Fatalf("newUpstreamTransport() = %v, want nil", transport)
	}

	output := logs.String()
	if !strings.Contains(output, "Ignoring unknown PROXY values: foo,bar") {
		t.Fatalf("expected invalid PROXY warning in logs, got %q", output)
	}

	if !strings.Contains(output, "Upstream proxy mode disabled") {
		t.Fatalf("expected disabled log in logs, got %q", output)
	}
}
