package specdispatch

import "testing"

// The consecutive assignee-lock-timeout count is read from metadata, not from
// the notes, and follows the bound-kill count's trust rule: no record is 0,
// and a record this package cannot read is the retries spent, so a bead whose
// history cannot be trusted is held for a person (gt-a4cdcd).
func TestLockTimeoutAttemptsReadsOnlyItsOwnMetadataKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		meta []byte
		want int
	}{
		{"its own key as a string", []byte(`{"spec_dispatch_lock_timeout_attempts":"2"}`), 2},
		{"its own key as a number", []byte(`{"spec_dispatch_lock_timeout_attempts":1}`), 1},
		{"the bound-kill key is another count", []byte(`{"spec_dispatch_timeout_attempts":2}`), 0},
		{"no metadata", nil, 0},
		{"a count that is not a number", []byte(`{"spec_dispatch_lock_timeout_attempts":"many"}`), LockTimeoutRetries},
		{"metadata that is not json", []byte("{not json"), LockTimeoutRetries},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := LockTimeoutAttempts(c.meta); got != c.want {
				t.Errorf("LockTimeoutAttempts(%s) = %d, want %d", c.meta, got, c.want)
			}
		})
	}
}
