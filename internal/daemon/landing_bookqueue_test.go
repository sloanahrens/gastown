package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// TestDeferredBookkeepingWiring: a listed rig's Lander gets the deferred flag
// and its Worker the queue, while an unlisted or unconfigured rig gets neither
// (gt-jz0tp6).
func TestDeferredBookkeepingWiring(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		rig    string
		cfg    *config.LandingWorkerConfig
		wantOn bool
	}{
		{
			name: "rig listed",
			rig:  "gastown",
			cfg: &config.LandingWorkerConfig{DeferredBookkeeping: &config.DeferredBookkeepingConfig{
				Rigs: []string{"gastown", "longeye"},
			}},
			wantOn: true,
		},
		{
			name: "rig not listed",
			rig:  "gastown",
			cfg: &config.LandingWorkerConfig{DeferredBookkeeping: &config.DeferredBookkeepingConfig{
				Rigs: []string{"longeye"},
			}},
		},
		{
			name: "no deferred_bookkeeping block",
			rig:  "gastown",
			cfg:  &config.LandingWorkerConfig{},
		},
		{
			name: "no config",
			rig:  "gastown",
			cfg:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := &land.Lander{Repo: "/sentinel/repo"}
			worker := &landworker.Worker{Rig: "gastown"}

			d.wireBookQueue(tc.rig, tc.cfg, lander, worker)

			if !tc.wantOn {
				if lander.DeferBookkeeping {
					t.Fatalf("an off rig's Lander was given DeferBookkeeping; want the inline path")
				}
				if worker.Books != nil {
					t.Fatalf("an off rig's Worker was given a BookQueue; want none")
				}
				if got := logs.String(); got != "" {
					t.Fatalf("an off rig logged %q; want nothing", got)
				}
				return
			}
			if !lander.DeferBookkeeping {
				t.Fatalf("the listed rig's Lander was not given DeferBookkeeping")
			}
			if worker.Books == nil {
				t.Fatalf("the listed rig's Worker was given no BookQueue")
			}
			if worker.Books.Logf == nil {
				t.Fatalf("the listed rig's BookQueue has no Logf, so a panicked job would be silent")
			}
			worker.Books.Logf("the bookkeeping job for %s panicked", "gt-abc")
			if got := logs.String(); !strings.Contains(got, "the bookkeeping job for gt-abc panicked") {
				t.Fatalf("the BookQueue's Logf wrote %q to the daemon log; want the job's message", got)
			}
		})
	}
}
