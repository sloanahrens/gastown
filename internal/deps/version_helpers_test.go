package deps

import (
	"fmt"
	"testing"
)

// belowVersion returns a version just under v, so fixtures track the
// Min*/Recommended* constants instead of hard-coding their neighbours.
func belowVersion(v string) string {
	p := ParseVersion(v)
	switch {
	case p[2] > 0:
		p[2]--
	case p[1] > 0:
		p[1], p[2] = p[1]-1, 99
	default:
		p[0], p[1], p[2] = p[0]-1, 99, 99
	}
	return fmt.Sprintf("%d.%d.%d", p[0], p[1], p[2])
}

func TestBelowVersion(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"2.0.7", "0.57.0", "3.0.0", MinDoltVersion, MinClaudeCodeVersion, RecommendedClaudeCodeVersion} {
		if b := belowVersion(v); CompareVersions(b, v) >= 0 {
			t.Errorf("belowVersion(%q) = %q, not below it", v, b)
		}
	}
}
