package store

import "testing"

func TestAppendNote(t *testing.T) {
	t.Parallel() // unit tier: pure
	for _, tc := range []struct{ have, add, want string }{
		{"", "first", "first"},
		{"first", "second", "first\nsecond"},
		{"first\n", "second", "first\nsecond"},
		{"first", "", "first"},
	} {
		if got := appendNote(tc.have, tc.add); got != tc.want {
			t.Errorf("appendNote(%q, %q) = %q, want %q", tc.have, tc.add, got, tc.want)
		}
	}
}

func TestBuiltinStatuses(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"open", "in_progress", "closed", "deferred", "hooked"} {
		if !isBuiltinStatus(s) {
			t.Errorf("%q should be a built-in status", s)
		}
	}
	for _, s := range []string{"", "Open", "done", "wip", "staged_ready"} {
		if isBuiltinStatus(s) {
			t.Errorf("%q is not a built-in status (custom ones live in tracker.custom_statuses)", s)
		}
	}
}
