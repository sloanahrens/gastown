package rehearsal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRehearsalTownSling(t *testing.T) {
	t.Parallel()
	failed := errors.New("signal: killed")
	const leaked = "bd: secret output"
	cases := []struct {
		name, rig, id, agent, reason string
		close                        bool
		wantErr                      string // "", "deadline", or the command the error must name
		wantArgv                     []string
	}{
		{name: "sling runs the stated argv under a five minute deadline", rig: "rehearsal", id: "rh-01", agent: "claude", wantArgv: []string{"gt", "sling", "rh-01", "rehearsal", "--agent", "claude"}},
		{name: "a sling past its bound wraps context.DeadlineExceeded", rig: "rehearsal", id: "rh-02", agent: "claude", wantErr: "deadline", wantArgv: []string{"gt", "sling", "rh-02", "rehearsal", "--agent", "claude"}},
		{name: "a failed sling names gt sling and carries no output", rig: "rehearsal", id: "rh-03", agent: "claude", wantErr: "gt sling", wantArgv: []string{"gt", "sling", "rh-03", "rehearsal", "--agent", "claude"}},
		{name: "a rig's close runs bd -C the rig", rig: "rehearsal", id: "rh-04", reason: "the run finished", close: true, wantArgv: []string{"bd", "-C", "rehearsal", "close", "rh-04", "--reason=the run finished"}},
		{name: "no rig leaves -C out", id: "rh-05", reason: "the run failed", close: true, wantArgv: []string{"bd", "close", "rh-05", "--reason=the run failed"}},
		{name: "a failed close names bd close and carries no output", id: "rh-06", reason: "the run failed", close: true, wantErr: "bd close", wantArgv: []string{"bd", "close", "rh-06", "--reason=the run failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotArgv []string
			var bound time.Duration
			town := Town{Rig: tc.rig, Run: func(ctx context.Context, argv []string) ([]byte, error) {
				gotArgv = argv
				if deadline, ok := ctx.Deadline(); ok {
					bound = time.Until(deadline)
				}
				if tc.wantErr != "" {
					return []byte(leaked), failed
				}
				return nil, nil
			}}
			ctx := context.Background()
			if tc.wantErr == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			var err error
			if tc.close {
				err = town.Close(ctx, tc.id, tc.reason)
			} else {
				err = town.Sling(ctx, tc.id, tc.agent)
			}
			if got, want := strings.Join(gotArgv, "\x00"), strings.Join(tc.wantArgv, "\x00"); got != want {
				t.Errorf("ran argv %q, want %q", got, want)
			}
			switch {
			case tc.wantErr == "deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error = %v; want it to wrap %v", err, context.DeadlineExceeded)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Contains(err.Error(), leaked) {
					t.Fatalf("error = %v; want it to name %q and carry no output", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("error = %v; want nil", err)
			case !tc.close && (bound < 5*time.Minute-5*time.Second || bound > 5*time.Minute):
				t.Errorf("run's deadline is %v away, want five minutes", bound)
			}
		})
	}
}
