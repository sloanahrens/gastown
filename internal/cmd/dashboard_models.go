package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
// be a read of it. The models panel under the strip reads the same files and
// two more — the daemon's patrol config and the town model table — to list the
// seats and the model per role.

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
	ts, tsErr := config.LoadOrCreateTownSettings(config.TownSettingsPath(r.townRoot))
	if tsErr == nil && ts != nil {
		m.Polecat = strings.TrimSpace(ts.RoleAgents["polecat"])
		if ts.PolecatPool != nil && ts.PolecatPool.MaxSeats > 0 {
			m.SeatCap = ts.PolecatPool.MaxSeats
		}
	}
	om := loadOMConfig(r.omPath)
	m.OM = omModelName(om.Backend)

	if tsErr != nil {
		ts = nil
	}
	// The daemon's patrol config is a second file the panel reads whole: a
	// missing one is a town with no second seat and no schedules, an
	// unparseable one is neither, and the panel says so.
	patrols, patrolsErr := loadDashPatrols(r.townRoot)
	m.SeatRows = modelSeatRows(r.townRoot, ts, tsErr, patrols, patrolsErr)
	m.Roles = modelRoleRows(modelRoleInputs{
		townRoot: r.townRoot, ts: ts, tsErr: tsErr, patrols: patrols, patrolsErr: patrolsErr,
		omBackend: om.Backend, table: loadModelTable(filepath.Join(r.townRoot, "settings", "models.json")),
	})
	return m
}

// loadDashPatrols reads the daemon's patrol config, nil with no error when the
// file is not there.
func loadDashPatrols(townRoot string) (*config.PatrolsConfig, error) {
	pc, err := config.LoadDaemonPatrolConfig(config.DaemonPatrolConfigPath(townRoot))
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return pc.Patrols, nil
}

// modelSeatRows is the panel's seat table: the pool seat, the pro seat and the
// second seat, the same three gt pool show prints, each with the key it was
// read from. A seat nothing may fill is closed, not a row of zeros. The live
// counts are the hub's.
func modelSeatRows(townRoot string, ts *config.TownSettings, tsErr error, patrols *config.PatrolsConfig, patrolsErr error) []dashboard.ModelSeat {
	settings := filepath.Base(config.TownSettingsPath(townRoot))
	poolKey := "polecat_pool.agent, max_seats (" + settings + ")"
	proKey := "polecat_pool.pro_agent, polecat_pool.pro_max (" + settings + ")"
	secondKey := "patrols.spec_dispatch.hooked_agent, max_hooked (" + config.DaemonPatrolConfigFileName + ")"

	pool := dashboard.ModelSeat{Seat: "pool", Key: poolKey}
	switch {
	case tsErr != nil:
		pool.Unknown = true
	case ts == nil || ts.PolecatPool == nil || strings.TrimSpace(ts.PolecatPool.Agent) == "":
		pool.Closed = true
	default:
		pool.Agent = strings.TrimSpace(ts.PolecatPool.Agent)
		pool.Cap = ts.PolecatPool.MaxSeats
		if pool.Cap <= 0 {
			pool.Cap, pool.CapDefault = defaultSpecSeatCap, true
		}
	}

	// The pro seat reads as the dispatcher reads it (specBudgetFromConfig), so
	// a row is open here exactly when a seat is dispatched there: no
	// polecat_pool section or a pro_max of 0 is closed, an unset pro_max is the
	// dispatcher's own default of 1, and an unset pro_agent its default agent.
	pro := dashboard.ModelSeat{Seat: "pro", Key: proKey}
	switch {
	case tsErr != nil:
		pro.Unknown = true
	case ts == nil || ts.PolecatPool == nil:
		pro.Closed = true
	default:
		if cap := ts.PolecatPool.GetProMax(); cap <= 0 {
			pro.Closed = true
		} else {
			pro.Agent = strings.TrimSpace(ts.PolecatPool.GetProAgent())
			pro.Cap = cap
			pro.CapDefault = ts.PolecatPool.ProMax == nil
		}
	}

	second := dashboard.ModelSeat{Seat: "second", Key: secondKey}
	switch {
	case patrolsErr != nil:
		second.Unknown = true
	case patrols == nil || patrols.SpecDispatch == nil || patrols.SpecDispatch.MaxHooked <= 0:
		second.Closed = true
	default:
		second.Agent = strings.TrimSpace(patrols.SpecDispatch.HookedAgent)
		if second.Agent == "" {
			second.Agent = defaultSpecHookedAgent
		}
		second.Cap = patrols.SpecDispatch.MaxHooked
	}
	return []dashboard.ModelSeat{pool, pro, second}
}

// modelTable is the town model table, settings/models.json: the profile each
// role should run on, and what each profile is.
type modelTable struct {
	Profiles map[string]struct {
		Preset    string   `json:"preset"`
		OMBackend []string `json:"om_backend"`
	} `json:"profiles"`
	Roles map[string]string `json:"roles"`
}

// loadModelTable reads the table, nil when it is missing or does not parse: a
// table nobody can read has no opinion, and no role is called drifted by it.
func loadModelTable(path string) *modelTable {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return nil
	}
	var t modelTable
	if json.Unmarshal(b, &t) != nil {
		return nil
	}
	return &t
}

// wantAgent is the agent the table gives role, "" when it gives none.
func (t *modelTable) wantAgent(role string) string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.Profiles[t.Roles[role]].Preset)
}

// wantBackend is the reviewer command the table gives role, nil when none.
func (t *modelTable) wantBackend(role string) []string {
	if t == nil {
		return nil
	}
	return t.Profiles[t.Roles[role]].OMBackend
}

type modelRoleInputs struct {
	townRoot   string
	ts         *config.TownSettings
	tsErr      error
	patrols    *config.PatrolsConfig
	patrolsErr error
	omBackend  []string
	table      *modelTable
}

// deepReviewSchedule is the scheduled sling that runs the daily deep review.
const deepReviewSchedule = "deep-review"

// modelRoleRows is the panel's model-per-role table. Each role is read from
// its own key; one whose config cannot be read has no model, which the page
// calls unknown, and is never checked against the table. A role the table
// names whose live setting differs carries the table's value as Drift: the
// same comparison gt-models.sh show makes.
func modelRoleRows(in modelRoleInputs) []dashboard.ModelRole {
	settings := filepath.Base(config.TownSettingsPath(in.townRoot))
	roleAgent := func(role string) (string, string) {
		if in.tsErr != nil {
			return "", settings + " could not be read"
		}
		return strings.TrimSpace(in.ts.RoleAgents[role]), ""
	}

	polecat := dashboard.ModelRole{Role: "polecat", Key: "role_agents.polecat (" + settings + ")"}
	polecat.Model, polecat.Note = roleAgent("polecat")
	if want := in.table.wantAgent("polecat"); polecat.Model != "" && want != "" && want != polecat.Model {
		polecat.Drift = want
	}

	om := dashboard.ModelRole{Role: "om-review", Key: "backend (om config.json)"}
	om.Model = omModelName(in.omBackend)
	if om.Model == "" {
		om.Note = "the om config could not be read or names no backend"
	} else if want := in.table.wantBackend("om-review"); len(want) > 0 && !sameCommand(want, in.omBackend) {
		// The family is what the page shows; a table that differs only in
		// flags shows the command itself, or the badge would read "sonnet vs
		// sonnet".
		om.Drift = omModelName(want)
		if om.Drift == om.Model {
			om.Drift = strings.Join(want, " ")
		}
	}

	deep := dashboard.ModelRole{Role: "deep-review", Key: "patrols.scheduled_slings[" + deepReviewSchedule + "].agent (" + config.DaemonPatrolConfigFileName + ")"}
	switch {
	case in.patrolsErr != nil:
		deep.Note = config.DaemonPatrolConfigFileName + " could not be read"
	default:
		entry := scheduledSling(in.patrols, deepReviewSchedule)
		if entry == nil {
			deep.Note = "no scheduled sling named " + deepReviewSchedule
		} else if deep.Model = strings.TrimSpace(entry.Agent); deep.Model == "" {
			deep.Note = "the schedule names no agent"
		}
	}
	if want := in.table.wantAgent("deep-review"); deep.Model != "" && want != "" && want != deep.Model {
		deep.Drift = want
	}

	crew := dashboard.ModelRole{Role: "crew", Key: "role_agents.crew (" + settings + ")"}
	crew.Model, crew.Note = roleAgent("crew")
	if crew.Model == "" && crew.Note == "" {
		crew.Note = "role_agents.crew is not set"
	}
	if want := in.table.wantAgent("crew"); crew.Model != "" && want != "" && want != crew.Model {
		crew.Drift = want
	}
	return []dashboard.ModelRole{polecat, om, deep, crew}
}

// scheduledSling returns the named entry of the scheduled_slings patrol, nil
// when there is none.
func scheduledSling(p *config.PatrolsConfig, name string) *config.ScheduledSlingEntry {
	if p == nil || p.ScheduledSlings == nil {
		return nil
	}
	for i := range p.ScheduledSlings.Entries {
		if p.ScheduledSlings.Entries[i].Name == name {
			return &p.ScheduledSlings.Entries[i]
		}
	}
	return nil
}

// sameCommand compares two reviewer commands word for word, a leading "~/"
// standing for the home directory, which is how the table spells its paths.
func sameCommand(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	home, _ := os.UserHomeDir()
	expand := func(s string) string {
		if home != "" && strings.HasPrefix(s, "~/") {
			return filepath.Join(home, s[2:])
		}
		return s
	}
	for i := range a {
		if expand(a[i]) != expand(b[i]) {
			return false
		}
	}
	return true
}
