package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
// the bead respawn counts and the overrun guard's records. Both are read-only.
// A file that is missing labels nothing. A file that is there but does not
// read or parse also labels nothing, and Context reports it, so the report
// shows the gap instead of reading it as a bead with no data.
type thrashAdapter struct {
	town string

	// counts caches the respawn counts file, reread when it changes.
	counts countsCache

	// respawnBroken is true when the last Label found the respawn counts file
	// unreadable. overrunBroken counts the lines whose overrun record was
	// unreadable. Context reports both.
	respawnBroken bool
	overrunBroken int
}

// countsCache holds the parsed respawn counts and the file state it was read
// at, so a Label reads the file again only after it changes.
type countsCache struct {
	loaded bool
	size   int64
	mod    time.Time
	counts map[string]int
	err    error
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
// the overrun guard's record. A count below respawnThreshold and a record
// without a stop are both negatives, so a bead on either file is labeled.
// A bead with neither file is unlabeled. A bead still running when it is
// labeled reads as a negative until its guard stops it, as a first run does
// for will_respawn.
func (a *thrashAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	labels := map[QuestionID]bool{}
	var rules []string

	counts, err := a.respawnCounts()
	a.respawnBroken = err != nil
	if n, ok := counts[item.ID]; ok {
		labels[qWillRespawn] = n >= respawnThreshold
		rules = append(rules, "respawn-count")
	}

	stopped, onRecord, err := overrunGuardRecord(a.town, item.ID)
	if err != nil {
		a.overrunBroken++
	}
	if onRecord {
		labels[qNeedsSplit] = stopped
		if stopped {
			rules = append(rules, "overrun-stop")
		} else {
			rules = append(rules, "overrun-no-stop")
		}
	}

	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// Context reports the files a label could not read. A missing file is normal
// and is not reported.
func (a *thrashAdapter) Context() []string {
	var lines []string
	if a.respawnBroken {
		lines = append(lines, "bead-respawn-counts.json is unreadable: will_respawn is left unlabeled")
	}
	if a.overrunBroken > 0 {
		lines = append(lines, fmt.Sprintf("overrun-guard records are unreadable: needs_split is left unlabeled on %d line(s)", a.overrunBroken))
	}
	return lines
}

// respawnCounts returns each bead's respawn count. A missing file is no counts
// and no error. The file is reread whenever its size or mtime moves, so a
// report that outlives a rewrite labels from the new counts. An error means the
// file exists and cannot be read or parsed.
func (a *thrashAdapter) respawnCounts() (map[string]int, error) {
	path := filepath.Join(constants.TownRuntimePath(a.town), "bead-respawn-counts.json")
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := &a.counts
	if !c.loaded || info.Size() != c.size || !info.ModTime().Equal(c.mod) {
		c.counts, c.err = readRespawnCounts(path)
		c.size, c.mod, c.loaded = info.Size(), info.ModTime(), true
	}
	return c.counts, c.err
}

// readRespawnCounts parses bead-respawn-counts.json at path.
func readRespawnCounts(path string) (map[string]int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted townRoot
	if err != nil {
		return nil, err
	}
	var state struct {
		Beads map[string]struct {
			Count int `json:"count"`
		} `json:"beads"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(state.Beads))
	for id, rec := range state.Beads {
		counts[id] = rec.Count
	}
	return counts, nil
}

// overrunGuardRecord reads the overrun guard's record for bead. onRecord is
// false when there is no record, which leaves needs_split unlabeled. Otherwise
// stopped says whether the record carries a stop. An error means the record
// exists and cannot be read or parsed.
func overrunGuardRecord(town, bead string) (stopped, onRecord bool, err error) {
	data, err := os.ReadFile(filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json")) //nolint:gosec // G304: bead id is checked by Parse
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var rec struct {
		StoppedAt time.Time `json:"stopped_at"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return false, false, err
	}
	return !rec.StoppedAt.IsZero(), true, nil
}
