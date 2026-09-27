package globalswap

import "testing"

func TestA(t *testing.T) {
	t.Parallel()
	runCmd = func() error { return nil }
}
