package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/templates"
)

// TestHostAgentWithLaunchd pins what the agent publishes for the launchd job: a
// read job as its loaded state and pid, and an unreadable job as an error that
// never reads as a job that is not running.
func TestHostAgentWithLaunchd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		state   templates.SupervisorState
		want    hostprobe.Launchd
		wantErr bool
	}{
		{name: "a running job", state: templates.SupervisorState{Kind: "launchd", Loaded: true, PID: 223, LastExit: -1},
			want: hostprobe.Launchd{Loaded: true, PID: 223}},
		{name: "a job launchd does not know", state: templates.SupervisorState{Kind: "launchd", LastExit: -1},
			want: hostprobe.Launchd{}},
		{name: "a job that cannot be read", state: templates.SupervisorState{Kind: "launchd", LastExit: -1, Err: errors.New("launchctl: not found")},
			wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var askedKind string
			read := func(kind string) templates.SupervisorState { askedKind = kind; return tc.state }
			got := hostAgentWithLaunchd(hostprobe.Probes{V: hostprobe.SchemaVersion}, read)
			if askedKind != "launchd" {
				t.Errorf("read the %q job, want launchd", askedKind)
			}
			if tc.wantErr {
				if got.Launchd.Err == "" {
					t.Fatalf("an unreadable job published %+v, want an error in the field", got.Launchd)
				}
				return
			}
			if got.Launchd.Err != "" || got.Launchd.Value != tc.want {
				t.Errorf("published %+v, want %+v with no error", got.Launchd, tc.want)
			}
		})
	}
}
