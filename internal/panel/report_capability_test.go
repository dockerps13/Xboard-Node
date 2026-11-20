package panel

import (
	"encoding/json"
	"testing"
)

func TestHandshakeIdempotentReportsExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    bool
	}{
		{"legacy", `{"websocket":{"enabled":false},"settings":{"push_interval":60}}`, false},
		{"enabled", `{"websocket":{"enabled":false},"settings":{"push_interval":60,"idempotent_reports":true}}`, true},
		{"disabled", `{"websocket":{"enabled":false},"settings":{"push_interval":60,"idempotent_reports":false}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hs HandshakeResponse
			if err := json.Unmarshal([]byte(tc.payload), &hs); err != nil {
				t.Fatal(err)
			}
			if hs.Settings.IdempotentReports != tc.want {
				t.Fatalf("got %v want %v", hs.Settings.IdempotentReports, tc.want)
			}
		})
	}
}
