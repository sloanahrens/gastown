package deadseats

import "testing"

// A death is confirmed at the threshold and not before it: the patrol waits
// one sample short, and every reader has to agree (gt-4rxdi).
func TestConfirmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		samples, threshold int
		want               bool
	}{
		{0, 1, false},
		{1, 1, true},
		{2, 1, true},
		{0, 2, false},
		{1, 2, false},
		{2, 2, true},
		{3, 2, true},
		{1, 0, false}, // no configured threshold reads as the default
		{2, 0, true},
	} {
		if got := Confirmed(c.samples, c.threshold); got != c.want {
			t.Errorf("Confirmed(%d, %d) = %v, want %v", c.samples, c.threshold, got, c.want)
		}
	}
}
