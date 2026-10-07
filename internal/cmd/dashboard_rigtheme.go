package cmd

import (
	"path/filepath"

	"github.com/steveyegge/gastown/internal/polecat"
)

// The Rigs panel's Names column: the name theme each rig draws its polecats
// from, which is what lets a short name on the page be read back to its rig.
// The theme is the pool's own reading (polecat.EffectiveTheme), so the column
// cannot name a theme the rig does not name its polecats from (gt-yieek).

// rigThemeSamples is how many sample names a rig row carries for the column's
// tooltip: enough of the pool to recognize it, few enough for one title line.
const rigThemeSamples = 5

// newDashRigThemes reads a rig's effective theme and a few of its names. A rig
// naming from an explicit list reports no theme; a theme whose names cannot be
// read still reports the theme and leaves the samples empty, so one bad theme
// file costs a tooltip rather than the column.
func newDashRigThemes(townRoot string) func(rig string) (string, []string) {
	return func(rig string) (string, []string) {
		theme := polecat.EffectiveTheme(filepath.Join(townRoot, rig), rig)
		if theme == "" {
			return "", nil
		}
		names, err := polecat.ResolveThemeNames(townRoot, theme)
		if err != nil {
			return theme, nil
		}
		if len(names) > rigThemeSamples {
			names = names[:rigThemeSamples]
		}
		return theme, names
	}
}
