package core

import (
	"encoding/json"
	"log"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
)

func InitNotices(logger *log.Logger) {
	psiphon.SetNoticeWriter(psiphon.NewNoticeReceiver(func(notice []byte) {
		var msg struct {
			Type string `json:"noticeType"`
			Data struct {
				DiagnosticID string `json:"diagnosticID"`
				Protocol     string `json:"protocol"`
			} `json:"data"`
		}
		if json.Unmarshal(notice, &msg) != nil {
			return
		}

		if msg.Type == "ConnectedServer" {
			if entry, ok := allServerEntries[msg.Data.DiagnosticID]; ok {
				if msg.Data.Protocol != "" {
					protocolByIP.Store(entry.ip, msg.Data.Protocol)
				}
			}
		}

		if msg.Type == "ActiveTunnel" {
			regionDialersMu.Lock()
			d := dialerByServerID[msg.Data.DiagnosticID]
			regionDialersMu.Unlock()
			if d != nil {
				n := d.tunnelReady.Add(1)
				if n == 1 || int(n) == d.targetPool {
					logger.Printf("Psiphon [%s]: %d/%d tunnels ready", d.id, n, d.targetPool)
				}
			}
		}
	}))
}
