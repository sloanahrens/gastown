package cmd

import (
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The header strip answers what the town is running on: the model the polecat
// role resolves to, the pool's seats, and the model the reviewer runs. Each is
// read from config the town already keeps — settings/config.json, and the
// reviewer's own config — and re-read whole on every poll, so a model switched
// in settings shows on the page within one. Nothing here writes, and nothing
// here runs a command: a dashboard that changed what the town runs on would not
// be a read of it.

// modelsReader reads the strip's models.
type modelsReader struct {
	townRoot string
	omPath   string
}

func newModelsReader(townRoot string) *modelsReader {
	return &modelsReader{townRoot: townRoot, omPath: omConfigPath()}
}

// read returns the strip's models. Each config is read on its own: one that is
// missing or unparseable leaves its field empty, for the page to call unknown,
// and the other still reports. A field nobody could read is never shown as a
// value, because "unknown" is what the operator needs to know.
func (r *modelsReader) read() *dashboard.Models {
	m := &dashboard.Models{}
	// A settings file that is not there loads as defaults — a town that never
	// set a model runs the role's own — but one that is there and does not
	// parse is an error, and then the town's model is not something to guess.
	if ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(r.townRoot)); err == nil && ts != nil {
		m.Polecat = strings.TrimSpace(ts.RoleAgents["polecat"])
		if ts.PolecatPool != nil && ts.PolecatPool.MaxOverflow > 0 {
			m.SeatCap = ts.PolecatPool.MaxOverflow
		}
	}
	m.OM = omModelName(loadOMConfig(r.omPath).Backend)
	return m
}
