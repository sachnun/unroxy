package utils

import (
	"fmt"
	"math/rand"
)

// GenerateRandomIP generates a random public IP address
// Avoids private, loopback, and reserved ranges
func GenerateRandomIP() string {
	for {
		a := rand.Intn(224)

		// Skip reserved ranges
		if a == 0 || a == 10 || a == 127 {
			continue
		}

		b := rand.Intn(256)

		// Skip 172.16.0.0 - 172.31.255.255 (private)
		if a == 172 && b >= 16 && b <= 31 {
			continue
		}

		// Skip 192.168.0.0 - 192.168.255.255 (private)
		if a == 192 && b == 168 {
			continue
		}

		return fmt.Sprintf("%d.%d.%d.%d", a, b, rand.Intn(256), rand.Intn(256))
	}
}
