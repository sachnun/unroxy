package utils

import (
	"net"
	"testing"
)

func TestGenerateRandomIP(t *testing.T) {
	t.Run("generates valid IP", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			ip := GenerateRandomIP()
			parsedIP := net.ParseIP(ip)
			if parsedIP == nil {
				t.Errorf("Generated invalid IP: %s", ip)
			}
		}
	})

	t.Run("does not generate private IPs", func(t *testing.T) {
		privateRanges := []struct {
			start net.IP
			end   net.IP
		}{
			{net.ParseIP("10.0.0.0"), net.ParseIP("10.255.255.255")},
			{net.ParseIP("172.16.0.0"), net.ParseIP("172.31.255.255")},
			{net.ParseIP("192.168.0.0"), net.ParseIP("192.168.255.255")},
			{net.ParseIP("127.0.0.0"), net.ParseIP("127.255.255.255")},
		}

		for i := 0; i < 1000; i++ {
			ip := GenerateRandomIP()
			parsedIP := net.ParseIP(ip).To4()

			for _, r := range privateRanges {
				if bytesInRange(parsedIP, r.start.To4(), r.end.To4()) {
					t.Errorf("Generated private/reserved IP: %s", ip)
				}
			}
		}
	})

	t.Run("generates different IPs", func(t *testing.T) {
		seen := make(map[string]bool)
		for i := 0; i < 100; i++ {
			ip := GenerateRandomIP()
			seen[ip] = true
		}
		// Should have at least 90 unique IPs out of 100
		if len(seen) < 90 {
			t.Errorf("Expected at least 90 unique IPs, got %d", len(seen))
		}
	})
}

func bytesInRange(ip, start, end net.IP) bool {
	for i := 0; i < 4; i++ {
		if ip[i] < start[i] {
			return false
		}
		if ip[i] > end[i] {
			return false
		}
	}
	return true
}
