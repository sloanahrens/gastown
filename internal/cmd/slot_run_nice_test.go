package cmd

import (
	"strings"
	"testing"
)

// TestSlotRunNiceness covers gt-93m1: gate-class holders keep normal CPU
// priority, everyone else is niced, and --nice overrides both.
func TestSlotRunNiceness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role string
		flag int
		want int
	}{
		{"gastown/refinery", -1, 0},
		{"gastown/refinery-batch", -1, 0},
		{"hm/main-branch-test", -1, 0},
		{"gastown/amber", -1, defaultNonGateNice},
		{"pid-1234", -1, defaultNonGateNice},
		{"gastown/amber", 0, 0},
		{"gastown/amber", 5, 5},
		{"gastown/refinery", 7, 7},
	}
	for _, c := range cases {
		if got := slotRunNiceness(c.role, c.flag); got != c.want {
			t.Errorf("slotRunNiceness(%q, %d) = %d, want %d", c.role, c.flag, got, c.want)
		}
	}
}

func TestNiceWrapper(t *testing.T) {
	t.Parallel()
	if got := niceWrapper(0); got != nil {
		t.Errorf("nice 0 must not wrap the command: %v", got)
	}
	if got := niceWrapper(-1); got != nil {
		t.Errorf("a negative niceness must not wrap the command: %v", got)
	}
	got := niceWrapper(10)
	if len(got) != 3 || !strings.HasSuffix(got[0], "nice") || got[1] != "-n" || got[2] != "10" {
		t.Errorf("niceWrapper(10) = %v, want <nice> -n 10", got)
	}
}
