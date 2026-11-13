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

// A bead's comments record the timeout run when its count is gone: a deferral
// comment is a run still in progress, the bound's own comment ends the run so a
// person's label clear gets fresh retries, and the newest such comment decides
// (gt-a4cdcd).
func TestLockTimeoutRecordedReadsTheNewestRunComment(t *testing.T) {
	t.Parallel()
	deferred := "spec-dispatch: gt-a: sling deferred, sbx-flash's hook write lock stayed held for its whole wait (1 of 3): timed out"
	stuck := "spec-dispatch: gt-a: dispatch failed, sbx-flash's hook write lock stayed held on 3 ticks in a row, so it is stuck: timed out"
	cases := []struct {
		name     string
		comments []string
		want     bool
	}{
		{"no comments", nil, false},
		{"a comment about something else", []string{"spec-dispatch: gt-a: shaped once"}, false},
		{"a deferral", []string{deferred}, true},
		{"the bound's comment", []string{stuck}, false},
		{"a run that ended after one that did not", []string{deferred, stuck}, false},
		{"a fresh run after an ended one", []string{stuck, deferred}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := LockTimeoutRecorded(c.comments); got != c.want {
				t.Errorf("LockTimeoutRecorded(%v) = %v, want %v", c.comments, got, c.want)
			}
		})
	}
}
