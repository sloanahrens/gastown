//go:build !darwin

package buildflags

import "testing"

// TestICUCgoEnvAddsNothingOffDarwin: the entries are for a darwin compiler, so
// another host adds none, whatever brew would say about icu4c.
func TestICUCgoEnvAddsNothingOffDarwin(t *testing.T) {
	t.Parallel()
	if got := ICUCgoEnv([]string{"PATH=/bin"}); got != nil {
		t.Errorf("ICUCgoEnv on this host = %v, want nil", got)
	}
}
