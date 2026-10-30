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

// writeKey puts a synthetic API key where the client's key search finds one, so
// a battery opened here builds without reading a key from the host. The
// hermetic harness points GT_TOWN_ROOT at its sandbox, and the key file sits
// under it as a battery's would.
func writeKey(t *testing.T) {
	t.Helper()
	town := os.Getenv("GT_TOWN_ROOT")
	require.NotEmpty(t, town, "the hermetic harness sets GT_TOWN_ROOT to its sandbox town")
	path := filepath.Join(town, "settings", "daemon.env")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(jev.EnvKey+"=synthetic-test-key\n"), 0o600))
	require.NoError(t, os.Chmod(path, 0o600))
}

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
// The key is synthetic: writeKey puts one where the search looks, so no host key
// is read.
func TestShadowScrubWiredInEveryBattery(t *testing.T) {
	t.Parallel()
	writeKey(t)

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
			name := b.name + "/off"
			if check {
				name = b.name + "/on"
			}
			t.Run(name, func(t *testing.T) {
				asker, err := b.open(newTown(t), check)
				require.NoError(t, err)
				client, ok := asker.(*jev.Client)
				require.True(t, ok, "the battery asks through a Jev client")
				assert.Equal(t, check, client.ShadowScrubbing(), "scrub_check %v reaches the client", check)
			})
		}
	}
}
