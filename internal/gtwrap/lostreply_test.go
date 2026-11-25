package gtwrap

import "testing"

// TestLostReplyNote: a bd write call whose exec stream broke gets the note, every other call gets none.
func TestLostReplyNote(t *testing.T) {
	t.Parallel()
	const want = "the bd call may have run: the exec stream broke before its reply arrived, so check with bd show before repeating anything that is not idempotent (a comment, a create); a repeated dep remove is safe, since it answers No dependency found"
	cases := []struct {
		name    string
		argv    []string
		exit    int
		errText string
		want    string
	}{
		{"protocol error on a create", []string{"bd", "create", "--title=note"}, 1, "error stream protocol error: unknown error", want},
		{"executing-command error on a comment", []string{"bd", "comment", "gt-abc", "text"}, 1, "error executing command in container: container not found", want},
		{"unexpected EOF on a dep remove", []string{"bd", "dep", "remove", "gt-abc", "gt-def"}, 1, "rpc error: unexpected EOF", want},
		{"stream reset on a dep remove", []string{"bd", "dep", "remove", "gt-abc", "gt-def"}, 1, "stream reset: the connection ended", want},
		{"protocol error on a read", []string{"bd", "show", "gt-abc"}, 1, "error stream protocol error: unknown error", ""},
		{"unexpected EOF on a list", []string{"bd", "list"}, 1, "unexpected EOF", ""},
		{"a write call that succeeded", []string{"bd", "create", "--title=note"}, 0, "error stream protocol error: unknown error", ""},
		{"an unrelated error on a write", []string{"bd", "create", "--title=note"}, 1, "Error: bead not found", ""},
		{"a stream break on another tool", []string{"gt", "done"}, 1, "error stream protocol error: unknown error", ""},
		{"a stream break with no argv", nil, 1, "error stream protocol error: unknown error", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lostReplyNote(tc.argv, tc.exit, tc.errText); got != tc.want {
				t.Errorf("lostReplyNote(%q, %d, %q) = %q, want %q", tc.argv, tc.exit, tc.errText, got, tc.want)
			}
		})
	}
}
