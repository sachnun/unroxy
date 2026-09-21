package core

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

func serverEntryLine(ip, secret, region string) string {
	json := fmt.Sprintf(`{"ipAddress":%q,"webServerSecret":%q,"region":%q}`, ip, secret, region)
	return hex.EncodeToString([]byte(json))
}

func TestDecodeEntryParsesRealLine(t *testing.T) {
	line := serverEntryLine("203.0.113.9", "c2VjcmV0", "US")

	id, ip, region, ok := decodeEntry(line)
	if !ok {
		t.Fatalf("decodeEntry(%q) failed", line)
	}
	if ip != "203.0.113.9" {
		t.Fatalf("ip = %q, want 203.0.113.9", ip)
	}
	if region != "US" {
		t.Fatalf("region = %q, want US", region)
	}

	tag := protocol.GenerateServerEntryTag("203.0.113.9", "c2VjcmV0")
	if want := protocol.TagToDiagnosticID(tag); id != want {
		t.Fatalf("id = %q, want %q", id, want)
	}
}

func TestDecodeEntryRejectsMalformedLines(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "not hex", line: "zzzz"},
		{name: "no json", line: hex.EncodeToString([]byte("plain text"))},
		{name: "empty ip", line: serverEntryLine("", "c2VjcmV0", "US")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, ok := decodeEntry(tt.line); ok {
				t.Fatalf("decodeEntry(%q) succeeded, want failure", tt.line)
			}
		})
	}
}

func TestParseServerEntriesIndexesByID(t *testing.T) {
	raw := serverEntryLine("203.0.113.9", "c2VjcmV0", "US") + "\n\n" + serverEntryLine("198.51.100.4", "b3RoZXI=", "DE") + "\nnot-a-line\n"

	entries := parseServerEntries(raw)
	if len(entries) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(entries))
	}
	_, ip, region, _ := decodeEntry(serverEntryLine("203.0.113.9", "c2VjcmV0", "US"))
	for id, info := range entries {
		if info.ip == ip && info.region == region {
			if id == "" {
				t.Fatal("entry indexed by empty id")
			}
			return
		}
	}
	t.Fatalf("entry %s/%s missing from %v", ip, region, entries)
}

func TestParseEntriesByRegionGroupsAndSkipsEmptyRegion(t *testing.T) {
	raw := serverEntryLine("203.0.113.9", "c2VjcmV0", "US") + "\n" +
		serverEntryLine("198.51.100.4", "b3RoZXI=", "US") + "\n" +
		serverEntryLine("192.0.2.1", "dGhpcmQ=", "") + "\n"

	byRegion := parseEntriesByRegion(raw)
	if len(byRegion["US"]) != 2 {
		t.Fatalf("US entries = %d, want 2", len(byRegion["US"]))
	}
	if _, ok := byRegion[""]; ok {
		t.Fatal("entries without region should be skipped")
	}
}
