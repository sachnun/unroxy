package main

import (
	"encoding/json"
	"log"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
)

func initPsiphonNoticeHandler(logger *log.Logger) {
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
			if entry, ok := allServerEntries[msg.Data.DiagnosticID]; ok {
				if d, ok := regionDialers[entry.region]; ok {
					n := d.tunnelReady.Add(1)
					if int(n) == d.targetPool {
						logger.Printf("Psiphon [%s]: %d/%d tunnels ready", entry.region, n, d.targetPool)
					}
				}
			}
		}
	}))
}
