package cmd

import (
	"context"
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
			got := hostAgentWithLaunchd(context.Background(), hostprobe.Probes{V: hostprobe.SchemaVersion}, read)
			if askedKind != "launchd" {
				t.Errorf("read the %q job, want launchd", askedKind)
			}
			if got.Launchd == nil {
				t.Fatal("the agent left the launchd field nil, which a reader takes as unknown")
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

// TestHostAgentWithLaunchdBoundsTheRead pins that a launchctl that never
// answers fails the field when the context ends, and never reads as a job that
// is not running.
func TestHostAgentWithLaunchdBoundsTheRead(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	read := func(string) templates.SupervisorState {
		<-release
		return templates.SupervisorState{Kind: "launchd", LastExit: -1}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := hostAgentWithLaunchd(ctx, hostprobe.Probes{V: hostprobe.SchemaVersion}, read)
	if got.Launchd == nil || got.Launchd.Err == "" {
		t.Fatalf("a read that outlived its context published %+v, want an error in the field", got.Launchd)
	}
}
