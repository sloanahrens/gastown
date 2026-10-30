package specdispatch

import (
	"fmt"
	"strings"
	"testing"
)

// A bead the spec-dispatch-failed label holds out is slung again only while its
// notes say the last failure was a sling killed at its bound and a retry is
// left: the label is the dispatcher's record of the kill, and clearing it after
// a failure of another kind would retry a bead whose reason is not the host's
// load (gt-vaivp).
func TestRetryAfterTimeout(t *testing.T) {
	t.Parallel()
	mark := TimeoutNoteMarker + " sling killed at its 5m0s bound (attempt %d of 3)"
	timeouts := func(n int) string {
		lines := make([]string, 0, n)
		for i := 1; i <= n; i++ {
			lines = append(lines, fmt.Sprintf(mark, i))
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct {
		name    string
		notes   string
		attempt int
		retry   bool
	}{
		{"no notes", "", 0, false},
		{"an unrelated note", "findings: the retry loop never resets", 0, false},
		{"one bound kill", timeouts(1), 1, true},
		{"the last retry left", timeouts(TimeoutRetries - 1), TimeoutRetries - 1, true},
		{"retries spent", timeouts(TimeoutRetries), TimeoutRetries, false},
		{"kills and then a failure of its own", timeouts(1) + "\nspec-dispatch: the bead has something wrong; excluded", 1, false},
		{"the marker mid-line is no kill", "see " + TimeoutNoteMarker + " in the log", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			attempt, retry := RetryAfterTimeout(c.notes)
			if attempt != c.attempt || retry != c.retry {
				t.Errorf("RetryAfterTimeout(%q) = attempt %d retry %v, want attempt %d retry %v",
					c.notes, attempt, retry, c.attempt, c.retry)
			}
		})
	}
}
