package bdgateway

import (
	"net/http/httptest"
	"testing"
)

// TestWorkspaceDriftServerAbsentRefusal drives the gateway's real handler with a
// runner that refuses the call for want of a workspace, and shows
// workspaceAbsent recognises the answer the server actually writes. A refusal
// the server maps to the same status for another reason is not an absent
// workspace. If the sentinel's text or the status mapping changes, this test
// goes red instead of the daemon silently never re-pushing.
func TestWorkspaceDriftServerAbsentRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		runner error
		want   bool
	}{
		{name: "no workspace", runner: ErrNoWorkspace, want: true},
		{name: "bad rig is another bad request", runner: ErrBadRig, want: false},
		{name: "cwd outside is another bad request", runner: ErrCwdOutsideWorkspace, want: false},
		{name: "env not allowed is another bad request", runner: ErrEnvNotAllowed, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t, mustStore(t), newTestCallLog(t), &stubRunner{err: tc.runner})
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))
			if got := workspaceAbsent(rec.Code, rec.Body.Bytes()); got != tc.want {
				t.Fatalf("workspaceAbsent(%d, %s) = %v, want %v", rec.Code, rec.Body.String(), got, tc.want)
			}
		})
	}
}
