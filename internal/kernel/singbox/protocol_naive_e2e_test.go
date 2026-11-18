//go:build with_naive_outbound

package singbox

import "testing"

func TestNaiveClientRoundTrip(t *testing.T) {
	testProtocolClientRoundTrip(t, []string{"naive"})
}
