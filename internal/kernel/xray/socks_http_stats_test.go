package xray

import (
	"context"
	"testing"

	"github.com/cedar2025/xboard-node/internal/model"
)

// Xray's SOCKS5 and HTTP authentication paths put the login username in
// inbound.User.Email. Using user@ID here silently returns zero bytes.
func TestSOCKSAndHTTPStatsUseLoginIdentity(t *testing.T) {
	for _, protocol := range []string{"socks", "http"} {
		t.Run(protocol, func(t *testing.T) {
			user := model.UserSpec{ID: 21, UUID: "test-login-uuid"}
			x, manager := newStatsBackedXray(t, []model.UserSpec{user}, nil)
			x.protocol = protocol

			loginUp := mustRegisterCounter(t, manager, "user>>>"+user.UUID+">>>traffic>>>uplink")
			loginDown := mustRegisterCounter(t, manager, "user>>>"+user.UUID+">>>traffic>>>downlink")
			legacyDown := mustRegisterCounter(t, manager, counterName(user.ID, "downlink"))
			loginUp.Add(321)
			loginDown.Add(65536)
			legacyDown.Add(999)

			traffic, _, _, err := x.GetUserTraffic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := traffic[user.ID]; got != [2]int64{321, 65536} {
				t.Fatalf("traffic keyed by authenticated username: got %v; want [321 65536]", got)
			}

			loginDown.Add(1024)
			traffic, _, _, err = x.GetUserTraffic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := traffic[user.ID]; got != [2]int64{321, 66560} {
				t.Fatalf("cumulative stats after second poll: got %v", got)
			}
		})
	}
}

func TestOtherXrayProtocolsKeepConfiguredEmailStats(t *testing.T) {
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks"} {
		t.Run(protocol, func(t *testing.T) {
			user := model.UserSpec{ID: 7, UUID: "test-login"}
			x, manager := newStatsBackedXray(t, []model.UserSpec{user}, nil)
			x.protocol = protocol
			c := mustRegisterCounter(t, manager, counterName(user.ID, "downlink"))
			c.Add(500)
			traffic, _, _, err := x.GetUserTraffic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := traffic[user.ID]; got != [2]int64{0, 500} {
				t.Fatalf("configured email accounting broken: got %v", got)
			}
		})
	}
}
