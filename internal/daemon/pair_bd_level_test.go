package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/bdskew"
)

// TestPreRollGuardPairBdLevel: one read of a commit's bd pin and the schema level
// that pin carries, through the clone's pins file and the lister. Each refusal
// names the read that failed and returns no level.
func TestPreRollGuardPairBdLevel(t *testing.T) {
	t.Parallel()
	pinsOf := func(text string) pairReadyPinsRead { return func(string) (string, error) { return text, nil } }
	failingRun := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("git: bad object") }
	tests := []struct {
		name    string
		commit  string
		pins    pairReadyPinsRead
		run     func(context.Context, string, ...string) ([]byte, error)
		wantPin string
		want    int
		wantErr string
	}{
		{name: "reads the pin and its level", commit: pairGreen, pins: pinsOf(pairImagePins(pairBdPin)), wantPin: pairBdPin, want: 67},
		{name: "a commit that is not a full sha", commit: "main", pins: pinsOf(pairImagePins(pairBdPin)), wantErr: "not a full sha"},
		{name: "a rig with no clone", commit: pairGreen, wantErr: "no local clone"},
		{name: "the pins file cannot be read", commit: pairGreen, pins: func(string) (string, error) { return "", errors.New("no such path") }, wantErr: "reading the image pins at " + pairGreen},
		{name: "a pins file with no bd line", commit: pairGreen, pins: pinsOf("om https://forgejo.test/town/om.git " + pairOther + "\n"), wantErr: "reading the bd pin"},
		{name: "the lister fails", commit: pairGreen, pins: pinsOf(pairImagePins(pairBdPin)), run: failingRun, wantPin: pairBdPin, wantErr: "the schema level bd " + pairBdPin[:7]},
		{name: "a listing with no migration", commit: pairGreen, pins: pinsOf(pairImagePins(pairBdPin)), run: pairTreeRun(t, []string{"README.md"}), wantPin: pairBdPin, wantErr: bdskew.ErrEmptyListing.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := tt.run
			if run == nil {
				run = pairTreeRun(t, pairBdTree(67))
			}

			pin, level, err := pairBdLevelAt(context.Background(), t.TempDir(), tt.commit, tt.pins, run)

			assert.Equal(t, tt.wantPin, pin)
			assert.Equal(t, tt.want, level)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tt.wantErr), "error %q does not name %q", err, tt.wantErr)
		})
	}
}
