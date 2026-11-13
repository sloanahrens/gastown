package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// TestCensusHasSessionAnswersAnUnlistedSeatOnlyFromAGoodListing: a seat the
// gateway's listing does not name is not live, with no probe, only when that
// listing is good. An empty listing and a failed listing cannot say the seat is
// absent, so the read takes the seat's probe.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestCensusHasSessionAnswersAnUnlistedSeatOnlyFromAGoodListing(t *testing.T) {
	const seat = "gt-gastown-p0"
	tests := []struct {
		name        string
		listReply   string
		listErr     error
		wantNotLive bool
		wantProbes  int
	}{
		{
			name:        "good listing without the seat is not live with no probe",
			listReply:   `{"sandboxes":[{"name":"gt-gastown-other","phase":"Ready"}]}`,
			wantNotLive: true,
			wantProbes:  0,
		},
		{
			name:       "empty listing takes the probe",
			listReply:  `{"sandboxes":[]}`,
			wantProbes: 1,
		},
		{
			name:       "failed listing takes the probe",
			listErr:    errors.New("gateway unreachable"),
			wantProbes: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			session.ResetSandboxRoutingForTest(town)
			t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
			if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: seat, Rig: "gastown", Polecat: "p0", Gateway: "gw"}); err != nil {
				t.Fatal(err)
			}
			gateway := &sandbox.Fake{
				Reply: map[string]string{"list": tt.listReply},
				Err:   map[string]error{},
			}
			if tt.listErr != nil {
				gateway.Err["list"] = tt.listErr
			}
			t.Cleanup(session.BeginSandboxCensus(gateway.Runner()))

			lister := &countingSeatLister{fakeLister: fakeLister{}}
			running, err := session.CensusSessionReader{Tmux: lister}.HasSession(seat)
			if tt.wantNotLive {
				if !errors.Is(err, session.ErrSeatNotLive) {
					t.Errorf("HasSession(%s) error = %v, want ErrSeatNotLive", seat, err)
				}
			} else if err != nil || !running {
				t.Errorf("HasSession(%s) = %v, %v; want the probe's running answer", seat, running, err)
			}
			if got := lister.readsOf("has-session")[seat]; got != tt.wantProbes {
				t.Errorf("has-session %s: probes = %d, want %d", seat, got, tt.wantProbes)
			}
		})
	}
}
