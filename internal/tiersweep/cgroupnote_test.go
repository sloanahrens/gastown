package tiersweep

import (
	"bytes"
	"errors"
	"testing"
)

// TestSweepCgroupNote pins the once-per-sweep line: only a print spends it,
// and a nil error or writer stays silent (gt-3wb27k).
func TestSweepCgroupNote(t *testing.T) {
	t.Parallel()
	first := errors.New("first failure")
	want := "tier-sweep: timings: cgroup: first failure (the sweep goes on)\n"
	cases := []struct {
		name string
		run  func(*cgroupNote, *bytes.Buffer)
	}{
		{"first error prints", func(n *cgroupNote, b *bytes.Buffer) { n.report(b, first) }},
		{"later error stays silent", func(n *cgroupNote, b *bytes.Buffer) { n.report(b, first); n.report(b, errors.New("second failure")) }},
		{"nil error silent and unspent", func(n *cgroupNote, b *bytes.Buffer) { n.report(b, nil); n.report(b, first) }},
		{"nil writer silent and unspent", func(n *cgroupNote, b *bytes.Buffer) { n.report(nil, first); n.report(b, first) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var n cgroupNote
			var b bytes.Buffer
			tc.run(&n, &b)
			if got := b.String(); got != want {
				t.Errorf("report wrote %q, want %q", got, want)
			}
		})
	}
}
