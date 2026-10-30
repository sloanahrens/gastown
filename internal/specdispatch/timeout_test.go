package specdispatch

import (
	"fmt"
	"testing"
)

// A bead a failure left is slung again only while it wears both the failure
// label and the timeout label — the second says the failure was a sling killed
// at its bound, which every other failure clears — and while the metadata
// count of those kills leaves a retry (gt-vaivp).
func TestRetryAfterTimeout(t *testing.T) {
	t.Parallel()
	failed := []string{DispatchFailedLabel}
	timedOut := []string{DispatchFailedLabel, TimeoutLabel}
	metadata := func(n int) []byte {
		return []byte(fmt.Sprintf(`{"%s":"%d"}`, TimeoutAttemptsKey, n))
	}
	cases := []struct {
		name    string
		labels  []string
		meta    []byte
		attempt int
		retry   bool
	}{
		{"no labels", nil, metadata(1), 0, false},
		{"the failure label alone", failed, metadata(1), 0, false},
		{"both labels, no count", timedOut, nil, 0, false},
		{"both labels, one kill", timedOut, metadata(1), 1, true},
		{"both labels, the last retry left", timedOut, metadata(TimeoutRetries - 1), TimeoutRetries - 1, true},
		{"both labels, the retries spent", timedOut, metadata(TimeoutRetries), TimeoutRetries, false},
		{"a count written as a number", timedOut,
			[]byte(fmt.Sprintf(`{"%s":%d}`, TimeoutAttemptsKey, TimeoutRetries)), TimeoutRetries, false},
		{"metadata this package cannot read", timedOut, []byte("{not json"), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			attempt, retry := RetryAfterTimeout(c.labels, c.meta)
			if attempt != c.attempt || retry != c.retry {
				t.Errorf("RetryAfterTimeout(%v, %s) = attempt %d retry %v, want attempt %d retry %v",
					c.labels, c.meta, attempt, retry, c.attempt, c.retry)
			}
		})
	}
}

// The count is read from metadata, not from the notes: a note that mentions a
// count is prose, and another writer's metadata key is not this one's.
func TestTimeoutAttemptsReadsOnlyItsOwnMetadataKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		meta []byte
		want int
	}{
		{"its own key as a string", []byte(`{"spec_dispatch_timeout_attempts":"2"}`), 2},
		{"its own key as a number", []byte(`{"spec_dispatch_timeout_attempts":2}`), 2},
		{"another writer's key", []byte(`{"attempts":"5"}`), 0},
		{"no metadata", nil, 0},
		{"a count that is not a number", []byte(`{"spec_dispatch_timeout_attempts":"many"}`), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := TimeoutAttempts(c.meta); got != c.want {
				t.Errorf("TimeoutAttempts(%s) = %d, want %d", c.meta, got, c.want)
			}
		})
	}
}
