package installer

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Smoke runs the installed binary and refuses to accept an install whose
// binary cannot answer 'gt stale --json' or answers another commit: it is the
// one failure worth rolling a good install back for, so every case here ends
// failed/rolled_back with a high escalation (gt-tpg0p.11).
func TestSmoke(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stdout string
		status int
		// reported is the commit the installed binary names, and resolved is
		// what the checkout makes of it ("" is a commit it does not have).
		reported string
		resolved string
		ended    bool
		exit     Exit
		// message is the escalation's text, empty when the smoke check passed.
		message string
	}{
		{
			name:     "a binary that answers its commit passes",
			stdout:   `{"stale": false, "binary_commit": "c2222222"}`,
			reported: "c2222222",
			resolved: "c2222222",
		},
		{
			name:    "junk instead of JSON fails the smoke",
			stdout:  "not json",
			ended:   true,
			exit:    ExitFailed,
			message: "the new binary cannot answer 'gt stale --json'",
		},
		{
			name:    "a nonzero status fails even with valid JSON on stdout",
			stdout:  `{"stale": false, "binary_commit": "c2222222"}`,
			status:  1,
			ended:   true,
			exit:    ExitFailed,
			message: "the new binary cannot answer 'gt stale --json'",
		},
		{
			name:     "a commit the checkout cannot resolve fails the smoke",
			stdout:   `{"stale": false}`,
			reported: "d3333333",
			ended:    true,
			exit:     ExitFailed,
			message:  "installed a build but cannot verify what came into force (read 'd3333333')",
		},
		{
			name:    "a binary that names nothing fails the smoke",
			stdout:  `{"stale": false}`,
			ended:   true,
			exit:    ExitFailed,
			message: "installed a build but cannot verify what came into force (read 'nothing')",
		},
		{
			name:     "another commit than the one built fails the smoke",
			stdout:   `{"stale": false}`,
			reported: "c1111111",
			resolved: "c1111111",
			ended:    true,
			exit:     ExitFailed,
			message:  "the install did not take — GT reports c1111111, built c2222222",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.resolved("c1111111")
			tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
			tr.respond = func(string, string, []string) ([]byte, []byte, int) {
				return []byte(tc.stdout), nil, tc.status
			}
			tr.Bin.Commit = func(string, string) string { return tc.reported }
			tr.Repo.Resolve = func(ref string) string {
				if ref == tc.resolved {
					return ref
				}
				return ""
			}

			exit, ended := tr.Smoke()

			if exit != tc.exit || ended != tc.ended {
				t.Errorf("Smoke() = (%d, %v), want (%d, %v)", exit, ended, tc.exit, tc.ended)
			}
			want := []string{tr.Paths.GT(), "stale", "--json"}
			if got := tr.command(0); !reflect.DeepEqual(got, want) {
				t.Errorf("command = %q, want %q", got, want)
			}
			if !tc.ended {
				if got := tr.escalation(); got != nil {
					t.Errorf("escalated with %q, want nothing", got)
				}
				return
			}
			message := strings.ReplaceAll(tc.message, "GT", tr.Paths.GT())
			wantEscalation := []string{tr.C.GT, "escalate", "install-gt: " + message,
				"-s", "high", "--source", "script:install-gt", "--fingerprint", "install-gt:smoke-failed"}
			if got := tr.escalation(); !reflect.DeepEqual(got, wantEscalation) {
				t.Errorf("escalation = %q, want %q", got, wantEscalation)
			}
			// The script's fail_install names the event in the result line and
			// the reason in the receipt: rolled_back, and smoke-failed.
			if want := "install-gt: RESULT rolled_back c2222222 c1111111 smoke-failed\n"; !strings.HasSuffix(tr.out.String(), want) {
				t.Errorf("stdout = %q, want it to end %q", tr.out.String(), want)
			}
			if got := read(t, tr.C.Receipts); !strings.Contains(got, `"reason": "smoke-failed"`) {
				t.Errorf("receipts = %q, want reason smoke-failed", got)
			}
		})
	}
}
