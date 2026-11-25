package bdgateway

import "testing"

// TestDefaultMaxConcurrent pins the gateway's own default cap to the literal 8:
// the existing default-cap test compares with the constant itself and passes
// whatever it is. About 12 simultaneous new connections pass the VM-to-host
// path to Dolt (gt-w0iqhu) and each bd process opens its own, so the cap sits
// below that.
func TestDefaultMaxConcurrent(t *testing.T) {
	t.Parallel()
	if DefaultMaxConcurrent != 8 {
		t.Errorf("DefaultMaxConcurrent = %d, want the literal 8", DefaultMaxConcurrent)
	}
}
