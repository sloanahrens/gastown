package daemon

import (
	"context"
	"errors"

	"github.com/steveyegge/gastown/internal/daemonimage"
)

// imagePinBDFloor asks the compare whether a bd tip contains the samples build,
// the judgement daemonimage.CheckBDFloor makes (gt-37k16e). The compare's base is
// the tip and its head the floor: that direction counts the commits only the
// floor has, so a swapped call would count the tip's own commits instead.
func imagePinBDFloor(ctx context.Context, cmp ImagePinCompare, owner, repo, tip string) error {
	info, err := cmp.CompareCommits(ctx, owner, repo, tip, daemonimage.BDSamplesFloor)
	if err != nil {
		return daemonimage.CheckBDFloor(tip, 0, err)
	}
	if info == nil {
		return daemonimage.CheckBDFloor(tip, 0, errors.New("the compare answered no comparison"))
	}
	return daemonimage.CheckBDFloor(tip, info.TotalCommits, nil)
}
