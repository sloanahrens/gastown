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

func TestWithNice(t *testing.T) {
	t.Parallel()
	base := []string{"make", "test"}
	if got := withNice(base, 0); strings.Join(got, " ") != "make test" {
		t.Errorf("nice 0 must leave the command alone: %v", got)
	}
	got := withNice(base, 10)
	if len(got) != 5 || !strings.HasSuffix(got[0], "nice") || got[1] != "-n" || got[2] != "10" || got[3] != "make" || got[4] != "test" {
		t.Errorf("withNice(10) = %v, want <nice> -n 10 make test", got)
	}
	if got := withNice(nil, 10); got != nil {
		t.Errorf("empty command must stay empty: %v", got)
	}
}
