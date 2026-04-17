package utils

import (
	"testing"
)

func TestRandomUserAgent(t *testing.T) {
	t.Run("returns non-empty string", func(t *testing.T) {
		ua := RandomUserAgent()
		if ua == "" {
			t.Error("Expected non-empty user agent")
		}
	})

	t.Run("returns valid user agent", func(t *testing.T) {
		ua := RandomUserAgent()
		// All our user agents contain "Mozilla"
		if len(ua) < 10 {
			t.Error("User agent too short")
		}
	})

	t.Run("returns different user agents", func(t *testing.T) {
		seen := make(map[string]bool)
		for i := 0; i < 100; i++ {
			ua := RandomUserAgent()
			seen[ua] = true
		}
		// Should have at least 2 different user agents
		if len(seen) < 2 {
			t.Errorf("Expected multiple different user agents, got %d", len(seen))
		}
	})
}
