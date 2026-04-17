package main

import "testing"

func TestProxyEnabled(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		enabled bool
	}{
		{name: "empty is disabled", value: "", enabled: false},
		{name: "zero is disabled", value: "0", enabled: false},
		{name: "false is disabled", value: "false", enabled: false},
		{name: "one is enabled", value: "1", enabled: true},
		{name: "true is enabled", value: "true", enabled: true},
		{name: "all is enabled", value: "all", enabled: true},
		{name: "sock is enabled", value: "sock", enabled: true},
		{name: "http is enabled", value: "http", enabled: true},
		{name: "csv is enabled", value: "sock,http", enabled: true},
		{name: "trimmed true is enabled", value: " True ", enabled: true},
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
		{name: "all expands all protocols", value: "all", expected: []string{"http", "https", "socks", "socks5"}},
		{name: "sock expands socks protocols", value: "sock", expected: []string{"socks", "socks5"}},
		{name: "http expands http protocols", value: "http", expected: []string{"http", "https"}},
		{name: "csv merges protocols", value: "sock,http", expected: []string{"http", "https", "socks", "socks5"}},
		{name: "boolean true expands all protocols", value: "true", expected: []string{"http", "https", "socks", "socks5"}},
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
