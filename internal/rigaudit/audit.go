package rigaudit

import (
	"slices"
	"strings"
)

// Input is the town state Problems audits: the rigs, the decoded config, and a
// loader for each rig's settings.
type Input struct {
	Unparked, Parked []string
	Config           any
	LoadSettings     func(rig string) (map[string]any, error)
}

// KnownRigs returns the unparked rigs followed by the parked ones, in a new
// slice, leaving both inputs unchanged.
func (in Input) KnownRigs() []string {
	return slices.Concat(in.Unparked, in.Parked)
}

// Problems runs the list and settings checks over in and returns their
// problems sorted by Kind, then Text. A parked rig counts as a known name in
// the config's lists, but only an unparked rig's settings are checked.
func Problems(in Input) []Problem {
	problems := CheckLists(in.Config, in.Unparked, in.KnownRigs())
	problems = append(problems, CheckSettings(in.Unparked, in.LoadSettings)...)
	slices.SortFunc(problems, func(a, b Problem) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Text, b.Text)
	})
	return problems
}
