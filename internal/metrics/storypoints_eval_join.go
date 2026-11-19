package metrics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// EvalRow is one landing the story-points evaluation can measure: the latest
// estimate the bead carried, joined to the polecat minutes it actually took.
// The three input pointers are nil for an estimate that had no such input.
type EvalRow struct {
	Bead           string
	Rig            string
	LandedAt       time.Time
	ActualMinutes  float64
	BlendedMinutes float64
	ShapeMinutes   *float64
	EmbedMinutes   *float64
	JevMinutes     *float64
}

// EvalCounts accounts for every input the join read: the landing records it
// read, the rows it joined, and why it dropped the rest. A landing record
// dropped for no estimate is counted there and not under a time reason, so the
// time reasons describe landings that have an estimate.
type EvalCounts struct {
	Records      int // landing records read from every ledger
	Joined       int // rows returned
	NoEstimate   int // dropped: the estimate log holds no line for the bead
	LowerBound   int // dropped: the polecat time is a lower bound
	UpperBound   int // dropped: the polecat time is an upper bound
	Absent       int // dropped: the record carries no polecat time
	MalformedLog int // estimate-log lines that did not decode, counted by estlog.Read
}

// errEvalJoinNoTown reports joinEval with no town root, which names no ledger
// and no log: a relative path would read the process's own directory.
var errEvalJoinNoTown = errors.New("metrics: story-points eval join needs a town root")

// evalLanding is one landing record with the rig of the ledger it came from, so
// the join does not depend on the record's own rig field.
type evalLanding struct {
	rig string
	rec land.LandingRecord
}

// joinEval joins every landing the town's ledgers hold to the latest estimate
// the estimate log holds for its bead, keeps the records whose polecat time is
// a measurement, and returns the rows ordered by landed time.
//
// A ledger or log that cannot be read is an error naming the file; an input
// that does not exist yet reads as none, as the landing and log readers read
// it. The command that prints the evaluation is gt-htz2cb; until it lands, the
// tests are this function's only caller.
func joinEval(townRoot string) ([]EvalRow, EvalCounts, error) {
	var counts EvalCounts
	if townRoot == "" {
		return nil, counts, errEvalJoinNoTown
	}
	logged, malformed, err := estlog.Read(townRoot)
	if err != nil {
		return nil, counts, err
	}
	counts.MalformedLog = malformed
	// The log is read whole and in append order, so the last line a bead has
	// is its latest estimate: the one in force when it landed.
	latest := make(map[string]estlog.Record, len(logged))
	for _, rec := range logged {
		latest[rec.BeadID] = rec
	}
	landings, err := evalLandings(townRoot)
	if err != nil {
		return nil, counts, err
	}
	rows := make([]EvalRow, 0, len(landings))
	for _, l := range landings {
		counts.Records++
		est, ok := latest[l.rec.BeadID]
		if !ok {
			counts.NoEstimate++
			continue
		}
		switch {
		case l.rec.PolecatLowerBound:
			counts.LowerBound++
			continue
		case l.rec.PolecatUpperBound:
			counts.UpperBound++
			continue
		case l.rec.PolecatSecs <= 0:
			counts.Absent++
			continue
		}
		rows = append(rows, EvalRow{
			Bead:           l.rec.BeadID,
			Rig:            l.rig,
			LandedAt:       l.rec.LandedAt,
			ActualMinutes:  l.rec.PolecatSecs / 60,
			BlendedMinutes: est.BlendedMinutes,
			ShapeMinutes:   est.Minutes.Shape,
			EmbedMinutes:   est.Minutes.Embed,
			JevMinutes:     est.Minutes.Jev,
		})
	}
	// A stable sort keeps the ledgers' order for landings at the same instant,
	// so the same town gives the same rows from run to run.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].LandedAt.Before(rows[j].LandedAt) })
	counts.Joined = len(rows)
	return rows, counts, nil
}

// evalLandings reads every rig's landing record through the landing package's
// reader, each tagged with the rig of the ledger it came from. Rigs are visited
// in the order os.ReadDir returns them, which sorts by filename.
func evalLandings(townRoot string) ([]evalLanding, error) {
	dir := filepath.Join(constants.TownRuntimePath(townRoot), "landings")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A town that has never landed anything has no landings directory,
			// as it has no estimate log yet: no input is not a failed read.
			return nil, nil
		}
		return nil, fmt.Errorf("landings directory %s: %w", dir, err)
	}
	var out []evalLanding
	for _, entry := range entries {
		// reviews.jsonl is the om review ledger beside the rigs' files, not a
		// rig's landings.
		if !strings.HasSuffix(entry.Name(), ".jsonl") || entry.Name() == land.ReviewsFileName {
			continue
		}
		rig := strings.TrimSuffix(entry.Name(), ".jsonl")
		file, err := land.RigLandingsFile(townRoot, rig)
		if err != nil {
			return nil, fmt.Errorf("landings file %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		recs, err := file.Since(time.Time{})
		if err != nil {
			return nil, fmt.Errorf("landings file %s: %w", file.Path, err)
		}
		for _, rec := range recs {
			// A record the join cannot place is corrupt: the landing writer
			// fills both fields, so a line without them is refused rather than
			// counted as a landing with no bead or no time.
			if rec.BeadID == "" {
				return nil, fmt.Errorf("landings file %s: a landing record names no bead", file.Path)
			}
			if rec.LandedAt.IsZero() {
				return nil, fmt.Errorf("landings file %s: bead %s: a landing record has no landed time", file.Path, rec.BeadID)
			}
			out = append(out, evalLanding{rig: rig, rec: rec})
		}
	}
	return out, nil
}
