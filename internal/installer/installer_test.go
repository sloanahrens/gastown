package installer

import (
	"strings"
	"testing"
)

// The four result lines below are the ones the deleted shell installer's test
// pinned for the same situations, so a field that stops being written shows up
// here.
func TestResultLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Result
		want string
	}{
		{
			name: "every field",
			in:   Result{Event: "installed", Commit: "abc123", Prev: "def456", Reason: "smoke-failed"},
			want: "install-gt: RESULT installed abc123 def456 smoke-failed",
		},
		{
			name: "a field with nothing said about it is a dash",
			in:   Result{Event: "installed", Commit: "abc123", Prev: "def456"},
			want: "install-gt: RESULT installed abc123 def456 -",
		},
		{
			name: "nothing known at all",
			in:   Result{},
			want: "install-gt: RESULT - - - -",
		},
		{
			name: "the trap's line for an unanticipated end",
			in:   Result{Event: "failed", Commit: "origin/main", Reason: "unexpected"},
			want: "install-gt: RESULT failed origin/main - unexpected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.Line(); got != tc.want {
				t.Errorf("Line() = %q, want %q", got, tc.want)
			}
		})
	}
}

// log() joins its arguments with one space, so a caller that passes one string
// and a caller that passes words get the same line.
func TestLog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		parts []string
		want  string
	}{
		{"one argument", []string{"Building abc123 in /tmp/rig (source: manual)"},
			"[install-gt] Building abc123 in /tmp/rig (source: manual)\n"},
		{"words", []string{"No build checkout at", "/tmp/rig"},
			"[install-gt] No build checkout at /tmp/rig\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			c := &Contract{Stdout: &out}
			c.Log(tc.parts...)
			if got := out.String(); got != tc.want {
				t.Errorf("Log(%v) wrote %q, want %q", tc.parts, got, tc.want)
			}
		})
	}
}

// Finish is the script's `result_line ...; exit N`: one call site states both,
// so the status cannot drift from the line.
func TestFinish(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	c := &Contract{Stdout: &out}

	got := c.Finish(Result{Event: "refused", Commit: "abc123", Reason: "dirty"}, ExitRefused)

	if got != ExitRefused {
		t.Errorf("Finish returned %d, want %d", got, ExitRefused)
	}
	if want := "install-gt: RESULT refused abc123 - dirty\n"; out.String() != want {
		t.Errorf("Finish wrote %q, want %q", out.String(), want)
	}
}
