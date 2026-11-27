package rigaudit

import (
	"slices"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// ReadRigs returns the town's registered rig names, split into sorted
// unparked and parked lists. state answers a rig's operational state; only
// the answer "PARKED" parks it. A missing registry is an empty town, not an
// error; every other read or parse failure is returned.
func ReadRigs(townRoot string, state func(rig string) string) (unparked, parked []string, err error) {
	cfg, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, nil, err
	}
	unparked, parked = []string{}, []string{}
	for name := range cfg.Rigs {
		if state(name) == "PARKED" {
			parked = append(parked, name)
		} else {
			unparked = append(unparked, name)
		}
	}
	slices.Sort(unparked)
	slices.Sort(parked)
	return unparked, parked, nil
}
