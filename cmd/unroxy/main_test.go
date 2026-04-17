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
