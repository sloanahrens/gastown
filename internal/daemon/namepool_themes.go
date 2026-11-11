package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// unresolvedThemeLines names each unparked rig whose namepool style has no
// usable theme file, one line per rig. A parked rig is left out: it dispatches
// nothing, so its theme is not the operator's concern this start. A park state
// that cannot be read is reported rather than skipped, because the rig's theme
// is then unchecked and the line says so.
func unresolvedThemeLines(townRoot string, rigs []string) []string {
	var lines []string
	for _, rig := range rigs {
		parked, err := townconfig.ParkState(townRoot, rig)
		if err != nil {
			lines = append(lines, fmt.Sprintf("namepool: rig %s: park state unreadable, so its theme file is not checked: %v", rig, err))
			continue
		}
		if parked != nil {
			continue
		}
		if _, err := polecat.UnresolvedTheme(townRoot, filepath.Join(townRoot, rig), rig); err != nil {
			lines = append(lines, fmt.Sprintf("%v; a sling into rig %s is refused until the theme file is back", err, rig))
		}
	}
	return lines
}

// warnUnresolvedThemes logs unresolvedThemeLines at startup. The pool refuses a
// sling into such a rig rather than name its polecat from another rig's theme,
// so this line is the operator's early view of the refusal (gt-pno1e6).
func (d *Daemon) warnUnresolvedThemes() {
	for _, line := range unresolvedThemeLines(d.config.TownRoot, d.getKnownRigs()) {
		d.logger.Print(line)
	}
}
