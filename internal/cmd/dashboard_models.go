package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
)

// The header strip answers what the town is running on: the model the polecat
// role resolves to, the pool's seats, and the model the reviewer runs. The
// models are read from config the town already keeps — settings/config.json,
// and the reviewer's own config — and re-read whole on every poll, so a model
// switched in settings shows on the page within one. The seat counts are the
// dispatcher's roster (dashboardSeatRoster), so the strip counts the occupancy
// a sling is refused against. Nothing here writes: a dashboard that changed
// what the town runs on would not be a read of it. The models panel under the
// strip reads the same files and two more — the daemon's patrol config and the
// town model table — to list the seats and the model per role.

// modelsReader reads the strip's models.
type modelsReader struct {
	townRoot string
	omPath   string
	// now is the clock the last-24-hours window is measured from; nil reads
	// the host's.
	now func() time.Time
}

func newModelsReader(townRoot string) *modelsReader {
	return &modelsReader{townRoot: townRoot, omPath: omConfigPath()}
}

// modelsActivityWindow is how far back the panel counts each agent's landings
// and rejections.
const modelsActivityWindow = 24 * time.Hour

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
	om := loadOMConfig(r.omPath)
	m.OM = omModelName(om.Backend)

	if tsErr != nil {
		ts = nil
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	m.SeatRows = modelSeatRows(r.townRoot, ts, tsErr, now)
	if ts != nil {
		m.Unrecorded = ts.PolecatPool.UnrecordedAgent()
	}
	m.SeatCap = modelSeatCap(ts, m.SeatRows)
	m.Activity, m.ActivityUnreadable = r.agentActivity()
	m.Roles = modelRoleRows(modelRoleInputs{
		townRoot: r.townRoot, ts: ts, tsErr: tsErr,
		omBackend: om.Backend, table: loadModelTable(filepath.Join(r.townRoot, "settings", "models.json")),
	})
	return m
}

// agentActivity counts the last 24 hours of landings and rejections per agent,
// the panel's answer to which seat the town's work is really going to. The
// landings come from each rig's landings file, the record the landing worker
// writes with the agent the landing ran on; the rejections come from the
// town's review log, the verdicts the worker refused the landing on. Both are
// files the worker writes and the page reads, never the daemon's log. A
// record naming no agent — a crew landing, or one written before the records
// carried agents — is counted for nobody, so the table never credits a seat
// with work nobody proved it ran.
//
// A file that is missing is a window with nothing to count; one that cannot be
// read, or a rig whose name is unusable, is left out of the count rather than
// failing the whole panel: the other seats' numbers are still true. Those
// sources are returned by name, because a count that lost a file and a count
// that found nothing in one are the same empty table, and only the panel's
// naming of the failures tells the operator which the window is (gt-h3cwd).
func (r *modelsReader) agentActivity() ([]dashboard.AgentActivity, []string) {
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	since := now.Add(-modelsActivityWindow)

	unreadable := []string{}
	landed := map[string]int{}
	rigs, err := knownRigNames(r.townRoot)
	if err != nil {
		// A town that never wrote a rig registry has no rigs, and its HQ
		// landings are the whole window; one that wrote a registry that
		// cannot be read has landings this count cannot name.
		if !errors.Is(err, config.ErrNotFound) {
			unreadable = append(unreadable, "the rig registry")
		}
		rigs = nil
	}
	for _, rig := range append([]string{"hq"}, rigs...) {
		f, err := land.RigLandingsFile(r.townRoot, rig)
		if err != nil {
			unreadable = append(unreadable, rig+" landings")
			continue
		}
		recs, err := f.Since(since)
		if err != nil {
			unreadable = append(unreadable, rig+" landings")
			continue
		}
		for _, rec := range recs {
			if agent := strings.TrimSpace(rec.Agent); agent != "" {
				landed[agent]++
			}
		}
	}

	rejected := map[string]int{}
	recs, err := land.TownReviewsFile(r.townRoot).Since(since)
	if err != nil {
		unreadable = append(unreadable, "the review log")
	} else {
		for _, a := range windowAttempts(recs) {
			agent := strings.TrimSpace(a.rec.Agent)
			// A request_changes verdict refused the landing with or without
			// the Rejected flag, which only an approving verdict's findings
			// set (gt-vu18r). Any other verdict is no approval either, as the
			// rework panel reads it (dashboard.ReworkRefused).
			if agent != "" && dashboard.ReworkRefused(a.rec.Verdict, a.rec.Rejected, a.rec.Findings, a.later) {
				rejected[agent]++
			}
		}
	}

	agents := make([]string, 0, len(landed)+len(rejected))
	seen := map[string]bool{}
	for _, by := range []map[string]int{landed, rejected} {
		for agent := range by {
			if !seen[agent] {
				seen[agent] = true
				agents = append(agents, agent)
			}
		}
	}
	sort.Strings(agents)
	out := make([]dashboard.AgentActivity, 0, len(agents))
	for _, agent := range agents {
		out = append(out, dashboard.AgentActivity{Agent: agent, Landed: landed[agent], Rejected: rejected[agent]})
	}
	return out, unreadable
}

// windowAttempt is one review line the agent counters read from the window, and
// whether a later attempt of the same bead follows it.
type windowAttempt struct {
	rec   land.ReviewRecord
	later bool
}

// windowAttempts folds a window's review lines the way the rework panel does
// (dashboard.AggregateRework): one line per attempt, the later of two for the
// same attempt, and each bead's attempts in time order. The panel and the
// counters then ask the same question of each attempt, so they count the same
// rejections.
func windowAttempts(recs []land.ReviewRecord) []windowAttempt {
	type beadKey struct{ rig, bead string }
	type attemptKey struct {
		beadKey
		attempt int
	}
	latest := map[attemptKey]land.ReviewRecord{}
	for _, rec := range recs {
		k := attemptKey{beadKey{rec.Rig, rec.BeadID}, rec.Attempt}
		if old, ok := latest[k]; !ok || !rec.At.Before(old.At) {
			latest[k] = rec
		}
	}
	byBead := map[beadKey][]land.ReviewRecord{}
	for _, rec := range latest {
		k := beadKey{rec.Rig, rec.BeadID}
		byBead[k] = append(byBead[k], rec)
	}
	var out []windowAttempt
	for _, rs := range byBead {
		sort.Slice(rs, func(i, j int) bool {
			if !rs[i].At.Equal(rs[j].At) {
				return rs[i].At.Before(rs[j].At)
			}
			return rs[i].Attempt < rs[j].Attempt
		})
		for i, rec := range rs {
			out = append(out, windowAttempt{rec: rec, later: i < len(rs)-1})
		}
	}
	return out
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

// dashboardSeatRoster is the strip's seat count: the roster the spec dispatcher
// counts, so the page reads the occupancy a sling is refused against, never
// every polecat the inventory lists (gt-ly90n). A read that fails is no count
// at all; poolLiveFor is one batched seat read, on a poll that runs only while
// a page is open (gt-lj299).
func dashboardSeatRoster(live poolShowLive) dashboard.SeatRoster {
	return dashboard.SeatRoster{Live: live.live, Total: live.total, Read: live.ok}
}

// modelSeatRows is the panel's seat table: one row per entry of
// polecat_pool.seats, in the loader's order (sorted by agent name), the same
// seats gt pool show prints. A seat that is closed, by a cap of 0 or by its
// window, is a row that says so rather than a row of zeros. The live counts are
// the hub's. A seat map the loader rejects is one unknown row naming the
// settings file, not a table of seats the dispatcher would refuse to run.
func modelSeatRows(townRoot string, ts *config.TownSettings, tsErr error, now time.Time) []dashboard.ModelSeat {
	settings := filepath.Base(config.TownSettingsPath(townRoot))
	if tsErr != nil {
		return []dashboard.ModelSeat{{Unknown: true, Key: "polecat_pool.seats (" + settings + ")"}}
	}
	seats, err := config.Seats(ts, config.AgentResolver(townRoot, ts))
	if err != nil {
		return []dashboard.ModelSeat{{Unknown: true, Key: "polecat_pool.seats (" + settings + ")"}}
	}
	if len(seats) == 0 {
		// A town with no seats is one closed row, not no rows: the strip reads
		// no rows as a settings read that has not come back.
		return []dashboard.ModelSeat{{Closed: true, State: "no seats", Key: "polecat_pool.seats (" + settings + ")"}}
	}
	rows := make([]dashboard.ModelSeat, 0, len(seats))
	for _, s := range seats {
		row := dashboard.ModelSeat{
			Agent: s.Agent, Cap: s.Cap, State: "open",
			Key: "polecat_pool.seats." + s.Agent + " (" + settings + ")",
		}
		switch {
		case s.Cap == 0:
			row.Closed, row.State = true, "closed (cap 0)"
		case s.Window != nil && s.Window.Closed(now):
			row.Closed, row.State = true, "closed until "+s.Window.OpensAt(now).Format("15:04")
		}
		rows = append(rows, row)
	}
	return rows
}

// modelSeatCap is the cap the strip shows the live count against: the town's
// total when polecat_pool names one, else the caps of the seats open now added
// up. Zero is a town that declares neither, which the strip shows as the live
// count alone.
func modelSeatCap(ts *config.TownSettings, rows []dashboard.ModelSeat) int {
	if ts != nil && ts.PolecatPool.GetMaxTotal() > 0 {
		return ts.PolecatPool.GetMaxTotal()
	}
	total := 0
	for _, row := range rows {
		if !row.Closed && !row.Unknown {
			total += row.Cap
		}
	}
	return total
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
	townRoot  string
	ts        *config.TownSettings
	tsErr     error
	omBackend []string
	table     *modelTable
}

// deepReviewSchedule is the scheduled sling that runs the daily deep review.
const deepReviewSchedule = "deep-review"

// deepReviewModelSchedules are the scheduled slings the deep-review panel reads:
// the slice and risk schedules and the legacy one they replaced.
var deepReviewModelSchedules = []string{"deep-review-slice", "deep-review-risk", deepReviewSchedule}

// modelRoleRows is the panel's model-per-role table: the reviewer and the crew.
// The polecat and the deep review have no row, since they run on the seats of
// polecat_pool.seats, which the seat table above lists (gt-mukgk). Each role is
// read from its own key; one whose config cannot be read has no model, which
// the page calls unknown, and is never checked against the table. A role the
// table names whose live setting differs carries the table's value as Drift.
func modelRoleRows(in modelRoleInputs) []dashboard.ModelRole {
	settings := filepath.Base(config.TownSettingsPath(in.townRoot))
	roleAgent := func(role string) (string, string) {
		if in.tsErr != nil {
			return "", settings + " could not be read"
		}
		return strings.TrimSpace(in.ts.RoleAgents[role]), ""
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

	crew := dashboard.ModelRole{Role: "crew", Key: "role_agents.crew (" + settings + ")"}
	crew.Model, crew.Note = roleAgent("crew")
	if crew.Model == "" && crew.Note == "" {
		crew.Note = "role_agents.crew is not set"
	}
	if want := in.table.wantAgent("crew"); crew.Model != "" && want != "" && want != crew.Model {
		crew.Drift = want
	}
	return []dashboard.ModelRole{om, crew}
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
