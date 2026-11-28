package hostload

import (
	"strings"
	"testing"
)

const topSample = `CPU usage: 50.00% user, 10.00% sys, 40.00% idle
PhysMem: 8G used (1G wired, 2G compressor), 8G unused.

CPU usage: 81.91% user, 11.40% sys, 6.68% idle
PhysMem: 126G used (13G wired, 40G compressor), 533M unused.
`

// TestHostCPUParseTop reads the last of two top samples: the first one's 40%
// idle and 8G must not win.
func TestHostCPUParseTop(t *testing.T) {
	t.Parallel()
	idle, used, compressor, err := ParseTop(topSample)
	if err != nil {
		t.Fatalf("ParseTop: %v", err)
	}
	if idle != 6.68 || used != 126<<30 || compressor != 40<<30 {
		t.Errorf("ParseTop = %v, %d, %d: want 6.68, %d, %d",
			idle, used, compressor, uint64(126)<<30, uint64(40)<<30)
	}
}

// TestHostCPUParseTopRefuses holds one row for each refusal ParseTop makes.
func TestHostCPUParseTopRefuses(t *testing.T) {
	t.Parallel()
	const mem = "PhysMem: 1G used (0B wired, 0B compressor)\n"
	for _, c := range []struct{ text, want string }{
		{"PhysMem: 1G used (0B wired, 0B compressor), 1G unused.\n", "CPU usage"},
		{"CPU usage: 98% user\n" + mem, "CPU usage"},
		{"CPU usage: 98% idle\n", "PhysMem"},
		{"CPU usage: many% idle\n" + mem, "not a finite number"},
		{"CPU usage: NaN% idle\n" + mem, "not a finite number"},
		{"CPU usage: +Inf% idle\n" + mem, "not a finite number"},
		{"CPU usage: 98% idle\nPhysMem: 126T used (13G wired, 40G compressor)\n", "is not a count"},
		{"CPU usage: 98% idle\nPhysMem: xG used (0B wired, 0B compressor)\n", "is not a count"},
		{"CPU usage: 98% idle\nPhysMem: 1G sold (0B wired, 0B compressor)\n", "no used amount"},
		{"CPU usage: 98% idle\nPhysMem: 1G used (0B wired)\n", "no compressor amount"},
		{"CPU usage: 98% idle\nPhysMem: 99999999999999999999B used (0B wired, 0B compressor)\n", "does not fit"},
		{"CPU usage: 98% idle\nPhysMem: 17179869184G used (0B wired, 0B compressor)\n", "does not fit"},
		{"CPU usage: 98% idle\nPhysMem: 1G used (0B wired, 17179869184G compressor)\n", "does not fit"},
	} {
		if _, _, _, err := ParseTop(c.text); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseTop(%q) = %v: want an error naming %q", c.text, err, c.want)
		}
	}
}
