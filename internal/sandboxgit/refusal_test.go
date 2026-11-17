package sandboxgit

import (
	"strings"
	"testing"
)

// TestRefusal drives each rule Refusal states: every refusal branch, and the
// calls a sandbox runner accepts. One row per reason a call is refused, so a
// failure names the branch that broke.
func TestRefusal(t *testing.T) {
	t.Parallel()
	const (
		label = "sandbox-one"
		dir   = "/sandbox/gt/gastown/polecats/furiosa/gastown"
	)
	tests := []struct {
		name    string
		dir     string
		args    []string
		wantErr string // a substring of the refusal; "" means the call is allowed
	}{
		{"a call with no verb is refused", dir, nil, "refusing a git call with no verb"},
		{"a verb the save does not run is refused", dir, []string{"clone", "origin"}, "not a command the overrun save runs"},
		{"an empty clone directory is refused", "", []string{"status"}, "clone directory is empty"},
		{"a relative clone directory is refused", "polecats/furiosa/gastown", []string{"status"}, "is not absolute"},
		{"a clone directory holding .. is refused", "/sandbox/gt/../gt/gastown", []string{"status"}, "holds .."},
		{"a clone directory holding a newline is refused", "/sandbox/gt/gastown\n", []string{"status"}, "holds a newline"},
		{"an argument holding a newline is refused", dir, []string{"reset", "origin/main\n--hard"}, "holds a newline or NUL"},
		{"an argument holding a NUL is refused", dir, []string{"reset", "origin/main\x00"}, "holds a newline or NUL"},
		{"the -c option is refused", dir, []string{"status", "-c", "core.pager=sh"}, "can change what git runs"},
		{"the -C option is refused", dir, []string{"status", "-C", "/elsewhere"}, "can change what git runs"},
		{"the --git-dir option is refused", dir, []string{"status", "--git-dir=/elsewhere"}, "can change what git runs"},
		{"an abbreviated --git-dir is refused", dir, []string{"status", "--git-d"}, "can change what git runs"},
		{"the --exec-path option is refused", dir, []string{"status", "--exec-path=/tmp"}, "can change what git runs"},
		{"the --exec option is refused", dir, []string{"status", "--exe=/tmp/git-helper"}, "can change what git runs"},
		{"the --upload-pack option is refused", dir, []string{"fetch", "--upload-pack=/tmp/helper"}, "can change what git runs"},
		{"the --receive-pack option is refused", dir, []string{"push", "--receive-pack=/tmp/helper"}, "can change what git runs"},
		{"the --config-env option is refused", dir, []string{"status", "--config-env=core.pager=PG"}, "can change what git runs"},
		{"the address of a branch is allowed", dir, []string{"rev-parse", "--show-toplevel"}, ""},
		{"a reset is allowed", dir, []string{"reset", "--hard", "origin/main"}, ""},
		{"an ordinary read is allowed", dir, []string{"diff", "--stat"}, ""},
		{"an attached -C after the verb is left to the subcommand", dir, []string{"reset", "-Cdir", "origin/main"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Refusal(label, tc.dir, tc.args)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Refusal(%q, %q) = %v, want the call allowed", tc.dir, tc.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Refusal(%q, %q) = nil, want a refusal naming %q", tc.dir, tc.args, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal = %q, want it to name %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "sandbox "+label) {
				t.Errorf("refusal = %q, want it to name the sandbox %q", err, label)
			}
		})
	}
}
