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

// thrashAdapter labels the thrash log from two read-only files the town keeps:
// the bead respawn counts and the overrun guard's records. A file that is
// missing labels nothing; one that does not read or parse also labels nothing
// and Context says so, so the report shows the gap rather than a bead with no
// data.
type thrashAdapter struct {
	town string

	// counts caches the respawn counts file, reread when it changes.
	counts countsCache

	// newest is each bead's latest placement in the batch Prefetch saw, and
	// batched says Prefetch ran at all.
	newest  map[string]time.Time
	batched bool

	// respawnBroken marks a respawn counts file the last Label could not read;
	// overrunBroken and unbatched count the lines left unlabeled without an
	// overrun record or a batch. Context reports all three.
	respawnBroken bool
	overrunBroken int
	unbatched     int
}

// respawnRecord is one bead's entry in the respawn counts file.
type respawnRecord struct {
	Count       int
	LastRespawn time.Time
}

// countsCache holds the parsed respawn counts and the file state it was read
// at, so a Label reads the file again only after it changes.
type countsCache struct {
	loaded bool
	size   int64
	mod    time.Time
	counts map[string]respawnRecord
	err    error
}

func (a *thrashAdapter) Battery() string { return ThrashBattery }
func (a *thrashAdapter) LogPath() string { return thrash.LogPath(a.town) }

// Prefetch finds each bead's newest placement, so Label can tell a placement a
// later one followed from the newest, which no respawn has had time to follow.
func (a *thrashAdapter) Prefetch(_ context.Context, items []Item) {
	a.batched = true
	a.unbatched = 0
	a.newest = map[string]time.Time{}
	for _, it := range items {
		if it.Time.After(a.newest[it.ID]) {
			a.newest[it.ID] = it.Time
		}
	}
}

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
	// A line asked under another question set answers a different question, so
	// it must not pool into this battery's score.
	if l.QuestionVersion != thrash.Version {
		return Item{}, fmt.Errorf("thrash line asked question set %d, the scorecard reads %d", l.QuestionVersion, thrash.Version)
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

// Label sets will_respawn from the bead's respawn record and needs_split from
// the overrun guard's record. A record without a stop is a negative, so a bead
// on that file is labeled. A record whose stop has begun but not finished leaves
// needs_split unlabeled, as the dispatch battery does, because the guard has
// decided to stop the bead. A bead with neither file is unlabeled. A bead still
// running when it is labeled reads as a negative until its guard stops it.
func (a *thrashAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	labels := map[QuestionID]bool{}
	var rules []string

	counts, err := a.respawnCounts()
	a.respawnBroken = err != nil
	if rec, ok := counts[item.ID]; ok {
		if will, known := a.willRespawn(item, rec); known {
			labels[qWillRespawn] = will
			rules = append(rules, "respawn-count")
		}
	}

	stop, err := overrunGuardRecord(a.town, item.ID)
	if err != nil {
		a.overrunBroken++
	}
	switch stop {
	case stopDone:
		labels[qNeedsSplit] = true
		rules = append(rules, "overrun-stop")
	case stopNone:
		labels[qNeedsSplit] = false
		rules = append(rules, "overrun-no-stop")
	case stopInProgress:
		rules = append(rules, "overrun-stop-pending")
	}

	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// willRespawn says whether a respawn followed the placement item, and whether
// the record can tell. A count below respawnThreshold is a first run, a
// negative. Without a last_respawn stamp the record cannot place a respawn
// against a placement, so nothing is labeled. A respawn stamped after the
// placement is a positive; otherwise a later placement of the bead settles it as
// a negative and the newest stays unlabeled, since no respawn has had the chance
// to follow it yet.
func (a *thrashAdapter) willRespawn(item Item, rec respawnRecord) (will, known bool) {
	if rec.Count < respawnThreshold {
		return false, true
	}
	if rec.LastRespawn.IsZero() {
		return false, false
	}
	if rec.LastRespawn.After(item.Time) {
		return true, true
	}
	if !a.batched {
		a.unbatched++
		return false, false
	}
	newest, ok := a.newest[item.ID]
	if !ok || !item.Time.Before(newest) {
		return false, false
	}
	return false, true
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
	if a.unbatched > 0 {
		lines = append(lines, fmt.Sprintf("the placements were not batched: will_respawn is left unlabeled on %d line(s)", a.unbatched))
	}
	return lines
}

// respawnCounts returns each bead's respawn count. A missing file is no counts
// and no error. The file is reread whenever its size or mtime moves, so a
// report that outlives a rewrite labels from the new counts. An error means the
// file exists and cannot be read or parsed.
func (a *thrashAdapter) respawnCounts() (map[string]respawnRecord, error) {
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
func readRespawnCounts(path string) (map[string]respawnRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted townRoot
	if err != nil {
		return nil, err
	}
	var state struct {
		Beads map[string]struct {
			Count       int       `json:"count"`
			LastRespawn time.Time `json:"last_respawn"`
		} `json:"beads"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	counts := make(map[string]respawnRecord, len(state.Beads))
	for id, rec := range state.Beads {
		counts[id] = respawnRecord{Count: rec.Count, LastRespawn: rec.LastRespawn}
	}
	return counts, nil
}

// overrunStop is how far the overrun guard got with a bead's stop.
type overrunStop int

const (
	// stopAbsent: the bead has no record.
	stopAbsent overrunStop = iota
	// stopNone: a record that carries neither stop stamp.
	stopNone
	// stopInProgress: stop_began_at is set and stopped_at is not. The guard
	// writes stopped_at last and retries its checkpoint and push steps across
	// ticks, so this is its normal form while a stop runs.
	stopInProgress
	// stopDone: a record with stopped_at.
	stopDone
)

// overrunGuardRecord reads the overrun guard's record for bead and says how far
// its stop got. stopAbsent leaves needs_split unlabeled. An error means the
// record exists and cannot be read or parsed.
func overrunGuardRecord(town, bead string) (overrunStop, error) {
	data, err := os.ReadFile(filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json")) //nolint:gosec // G304: bead id is checked by Parse
	if errors.Is(err, fs.ErrNotExist) {
		return stopAbsent, nil
	}
	if err != nil {
		return stopAbsent, err
	}
	var rec struct {
		StopBeganAt time.Time `json:"stop_began_at"`
		StoppedAt   time.Time `json:"stopped_at"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return stopAbsent, err
	}
	switch {
	case !rec.StoppedAt.IsZero():
		return stopDone, nil
	case !rec.StopBeganAt.IsZero():
		return stopInProgress, nil
	}
	return stopNone, nil
}
