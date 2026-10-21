package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/thrash"
)

// ThrashBattery is the thrash battery's name, the --battery value that scores
// the log the thrash pass writes.
const ThrashBattery = "thrash"

func init() {
	Register(ThrashBattery, func(townRoot string) Adapter { return &thrashAdapter{town: townRoot} })
}

// The question ids the thrash battery logs. They must match the json keys of
// thrash.Answers; TestScorecardThrashQuestionIDsMatchLogKeys fails when they
// drift.
const (
	qWillRespawn QuestionID = "will_respawn"
	qNeedsSplit  QuestionID = "needs_split"
)

// respawnThreshold is the respawn count at which a bead is labeled as one that
// was respawned. A count of one is the first run, so it is labeled not
// respawned.
const respawnThreshold = 2

// thrashAdapter labels the thrash log from two files the town already keeps:
// the bead respawn counts and the overrun guard's records. Both are read-only,
// and a file that is missing or does not parse labels nothing rather than
// failing the report.
type thrashAdapter struct {
	town string

	// counts is the respawn count per bead, read once on the first Label.
	counts       map[string]int
	countsLoaded bool
}

func (a *thrashAdapter) Battery() string { return ThrashBattery }
func (a *thrashAdapter) LogPath() string { return thrash.LogPath(a.town) }

// Parse reads one thrash line. The item is the bead, stamped with its
// placement, and the agent rides beside will_respawn as context.
func (a *thrashAdapter) Parse(raw []byte) (Item, error) {
	var l thrash.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Bead == "" || strings.ContainsAny(l.Bead, `/\`) || l.AttachedAt.IsZero() {
		return Item{}, errors.New("thrash line needs a bead id and an attached_at stamp")
	}
	noul := func(p float64) Answer { return Answer{Noul: &p} }
	item := Item{
		ID:    l.Bead,
		Time:  l.AttachedAt,
		Model: l.Model,
		Answers: map[QuestionID]Answer{
			qWillRespawn: noul(l.Answers.WillRespawn),
			qNeedsSplit:  noul(l.Answers.NeedsSplit),
		},
	}
	if l.Agent != "" {
		item.Context = map[QuestionID]string{qWillRespawn: l.Agent}
	}
	return item, nil
}

// Label sets will_respawn from the bead's respawn count and needs_split from
// the overrun guard's stop. A bead with neither source on file is unlabeled.
// needs_split is labeled only when a stop is on record: a record without one
// is pending, and no file says a bead ran long without a stop, so that is
// left unlabeled too.
func (a *thrashAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	if !a.countsLoaded {
		a.counts = readRespawnCounts(a.town)
		a.countsLoaded = true
	}
	labels := map[QuestionID]bool{}
	var rules []string
	if n, ok := a.counts[item.ID]; ok {
		labels[qWillRespawn] = n >= respawnThreshold
		rules = append(rules, "respawn-count")
	}
	if overrunGuardStopped(a.town, item.ID) {
		labels[qNeedsSplit] = true
		rules = append(rules, "overrun-stop")
	}
	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// readRespawnCounts returns each bead's respawn count from
// bead-respawn-counts.json. A missing or unparsable file is no counts.
func readRespawnCounts(town string) map[string]int {
	data, err := os.ReadFile(filepath.Join(constants.TownRuntimePath(town), "bead-respawn-counts.json")) //nolint:gosec // G304: path from trusted townRoot
	if err != nil {
		return nil
	}
	var state struct {
		Beads map[string]struct {
			Count int `json:"count"`
		} `json:"beads"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	counts := make(map[string]int, len(state.Beads))
	for id, rec := range state.Beads {
		counts[id] = rec.Count
	}
	return counts
}

// overrunGuardStopped reports whether the overrun guard's record for bead
// carries a stop. A missing, unreadable or unparsable record is not a stop.
func overrunGuardStopped(town, bead string) bool {
	data, err := os.ReadFile(filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json")) //nolint:gosec // G304: bead id is checked by Parse
	if err != nil {
		return false
	}
	var rec struct {
		StoppedAt time.Time `json:"stopped_at"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return false
	}
	return !rec.StoppedAt.IsZero()
}
