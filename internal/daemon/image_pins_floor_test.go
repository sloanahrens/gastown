package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/daemonimage"
)

func TestImagePinBDFloor(t *testing.T) {
	t.Parallel()
	const owner, repo, tip = "owner", "beads", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	floor := daemonimage.BDSamplesFloor
	forward, swapped := pinsReadCompareKey(tip, floor), pinsReadCompareKey(floor, tip)
	boom := errors.New("forgejo: 500")
	tests := []struct {
		name    string
		behind  map[string]int
		nilFor  map[string]bool
		errs    map[string]error
		want    string
		wrapped error
	}{
		// The swapped answer is a different count (3), so a compare whose base
		// and head are swapped fails this row rather than missing an answer.
		{name: "the tip descends from the floor", behind: map[string]int{forward: 0, swapped: 3}},
		{name: "the tip does not contain the floor", behind: map[string]int{forward: 3, swapped: 0}, want: "does not contain the samples build"},
		{name: "the compare errors", errs: map[string]error{forward: boom}, want: "forgejo: 500", wrapped: boom},
		{name: "the compare answers nothing", nilFor: map[string]bool{forward: true}, want: "answered no comparison"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmp := &fakePinCompare{behind: tt.behind, nilFor: tt.nilFor, errs: tt.errs}
			err := imagePinBDFloor(context.Background(), cmp, owner, repo, tip)
			require.Len(t, cmp.calls, 1)
			assert.Equal(t, pinsReadCompareCall{owner: owner, repo: repo, base: tip, head: floor}, cmp.calls[0],
				"the compare's base is the bd tip and its head the samples build")
			if tt.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range []string{"bd-floor guard", floor, tip, tt.want} {
				assert.Contains(t, err.Error(), want)
			}
			if tt.wrapped != nil {
				assert.ErrorIs(t, err, tt.wrapped)
			}
		})
	}
}
