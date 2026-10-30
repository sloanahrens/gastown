package wiringtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/jev/drift"
	"github.com/steveyegge/gastown/internal/jev/failcause"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
	"github.com/steveyegge/gastown/internal/jev/notes"
	"github.com/steveyegge/gastown/internal/jev/progress"
	"github.com/steveyegge/gastown/internal/jev/rework"
	"github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/jev/stale"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/jev/targets"
	"github.com/steveyegge/gastown/internal/jev/testsel"
	"github.com/steveyegge/gastown/internal/jev/thrash"
)

// newTown lays out what a battery's Open reads from the town: the rig registry
// naming the rig, and the rig's beads directory and repository directory. The
// files are only checked for existence, so the contents stay minimal.
func newTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(constants.MayorRigsPath(town)), 0o700))
	registry := `{"version":1,"rigs":{"` + dedupe.RigName + `":{"git_url":"https://example.invalid/gastown.git"}}}`
	require.NoError(t, os.WriteFile(constants.MayorRigsPath(town), []byte(registry), 0o600))
	for _, dir := range []string{".beads", filepath.Join("mayor", "rig")} {
		require.NoError(t, os.MkdirAll(filepath.Join(town, dedupe.RigName, dir), 0o700))
	}
	return town
}

// TestShadowScrubWiredInEveryBattery: each battery's pass asks through a client
// that runs the shadow check exactly when its scrub_check setting is on. A
// battery that drops the setting on its way to the client fails here.
//
// The key is synthetic: the test swaps the key reader, so no host key is read.
//
//testpolicy:allow parallel — swaps the process-wide key reader, which no other test may see mid-run; the cleanup restores it before the test returns.
func TestShadowScrubWiredInEveryBattery(t *testing.T) {
	restore := jev.SetKeyReaderForTest(func() (string, error) { return "synthetic-test-key", nil })
	t.Cleanup(restore)

	// open builds the battery's pass under town with scrub_check set to check
	// and returns the client it asks through.
	type open func(town string, check bool) (any, error)
	batteries := []struct {
		name string
		open open
	}{
		{"attention", func(town string, check bool) (any, error) {
			p, err := attention.Open(town, attention.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"conflict", func(town string, check bool) (any, error) {
			p, err := conflict.Open(town, conflict.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"dedupe", func(town string, check bool) (any, error) {
			p, err := dedupe.Open(town, dedupe.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"dispatch", func(town string, check bool) (any, error) {
			p, err := dispatch.Open(town, dispatch.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"drift", func(town string, check bool) (any, error) {
			p, err := drift.Open(town, drift.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"failcause", func(town string, check bool) (any, error) {
			p, err := failcause.Open(town, failcause.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"landing", func(town string, check bool) (any, error) {
			p, err := landing.Open(town, landing.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"logtriage", func(town string, check bool) (any, error) {
			p, err := logtriage.Open(town, logtriage.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"notes", func(town string, check bool) (any, error) {
			p, err := notes.Open(town, notes.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"progress", func(town string, check bool) (any, error) {
			p, err := progress.Open(town, new(progress.Queue), progress.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"rework", func(town string, check bool) (any, error) {
			p, err := rework.Open(town, rework.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"rules", func(town string, check bool) (any, error) {
			p, err := rules.Open(town, rules.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"stale", func(town string, check bool) (any, error) {
			p, err := stale.Open(town, []string{dedupe.RigName}, stale.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"submission", func(town string, check bool) (any, error) {
			p, err := submission.Open(town, submission.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"targets", func(town string, check bool) (any, error) {
			p, err := targets.Open(town, targets.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"testsel", func(town string, check bool) (any, error) {
			p, err := testsel.Open(town, testsel.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
		{"thrash", func(town string, check bool) (any, error) {
			p, err := thrash.Open(town, thrash.Settings{USDPerBillionTokens: 3, ScrubCheck: check}, nil)
			if err != nil {
				return nil, err
			}
			return p.Asker, nil
		}},
	}

	for _, b := range batteries {
		for _, check := range []bool{false, true} {
			t.Run(b.name, func(t *testing.T) {
				asker, err := b.open(newTown(t), check)
				require.NoError(t, err)
				client, ok := asker.(*jev.Client)
				require.True(t, ok, "the battery asks through a Jev client")
				assert.Equal(t, check, client.ShadowScrubbing(), "scrub_check %v reaches the client", check)
			})
		}
	}
}
