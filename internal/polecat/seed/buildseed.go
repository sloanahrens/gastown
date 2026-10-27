package seed

import (
	"fmt"
	"path"
	"strings"
)

// BuildRigSeed chooses the commit the build rig is seeded at. The build rig's pin
// is used when it covers every module the build commit requires. A pin that
// lacks one falls back to the build commit: slower to seed, but complete, and a
// line names the module so seed-pins gets bumped. Only a pin that cannot be read
// at all is an error. The image itself always builds the build commit.
func BuildRigSeed(pins []Source, buildRig, buildCommit, town string, git Git) (commit string, notes []string, err error) {
	if !isCommit(buildCommit) {
		return "", nil, fmt.Errorf("cannot read the build commit: %q is not a 7+ character hex commit, so build from a git checkout", buildCommit)
	}
	pin := ""
	for _, p := range pins {
		if p.Rig == buildRig {
			pin = p.Commit
		}
	}
	if pin == "" || pin == buildCommit {
		return buildCommit, nil, nil
	}
	dir := path.Join(town, buildRig, "mayor", "rig")
	missing, err := missingModules(git, dir, pin, buildCommit)
	if err != nil {
		return "", nil, fmt.Errorf("pin-drift guard: rig %s pin %s is not readable in its canonical checkout, so it cannot be compared with the build commit %s (%v): fetch or fix that checkout, then rerun", buildRig, short(pin), short(buildCommit), err)
	}
	if len(missing) > 0 {
		return buildCommit, []string{fmt.Sprintf("seed: pin %s lacks %s; seeding at the build commit (bump seed-pins)", short(pin), strings.Join(missing, ", "))}, nil
	}
	return pin, []string{fmt.Sprintf("seed: pin %s covers build commit %s; seeding at the pin", short(pin), short(buildCommit))}, nil
}
