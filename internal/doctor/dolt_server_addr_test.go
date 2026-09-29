package doctor

import "testing"

// TestDoltServerAddrBracketsIPv6: an IPv6 host must be bracketed, or the
// port reads as part of the address and the reachability dial always fails.
func TestDoltServerAddrBracketsIPv6(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{
		"127.0.0.1": "127.0.0.1:3307",
		"localhost": "localhost:3307",
		"::1":       "[::1]:3307",
	} {
		if got := doltServerAddr(host, 3307); got != want {
			t.Errorf("doltServerAddr(%q) = %q, want %q", host, got, want)
		}
	}
}
