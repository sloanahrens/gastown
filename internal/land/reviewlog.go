package land

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// ReviewsFileName is the town's om review log inside the landings directory.
// RigLandingsFile refuses the rig name that would make a rig's landing file
// collide with it.
const ReviewsFileName = "reviews.jsonl"

// ReviewRecord is one om verdict the landing worker reached: the line the
// dashboard's rework table counts. It is one line per verdict that judged the
// tree (approve or request_changes); a review that errored, was skipped or was
// waived by the overseer judged nothing and has no line (gt-mctxx).
type ReviewRecord struct {
	BeadID string `json:"bead"`
	Rig    string `json:"rig"`
	// Attempt is the rework attempt the verdict belongs to: the review
	// rejections the bead already carries, plus one. A verdict the landing
	// refuses stops at MaxReworkAttempts with the block it writes, so a reader
	// matching a refusal to its MERGE REJECTION block by number finds it
	// (gt-k10bq).
	Attempt int     `json:"attempt"`
	Score   float64 `json:"score"`
	Verdict string  `json:"verdict"`
	// Rejected says the landing refused the work on this verdict, whatever the
	// verdict says: an approval whose findings findingsRejection turned away
	// reads approve above and is still a rejection (gt-vu18r). Lines written
	// before the field carry none, and read as the verdict alone. A
	// request_changes verdict is a rejection too, with or without the flag.
	Rejected bool `json:"rejected,omitempty"`
	// Agent is the agent the polecat's seat ran on, so a reader can count a
	// seat's rejections beside its landings. A crew review has no seat and
	// records none, and lines written before the field carry none
	// (gt-mukgk.6).
	Agent string `json:"agent,omitempty"`
	// Branch is the branch the reviewed work was submitted on — the author's,
	// not the merge candidate — so the review's seat is on the line rather
	// than in the work bead's notes, which a rework and a hand edit rewrite
	// (gt-b9j3o.16).
	Branch string `json:"branch,omitempty"`
	// AuthorKind is the kind of seat that wrote Branch: polecat, crew,
	// overseer or unknown (AuthorKind). Lines written before the field carry
	// none, and a reader attributes them from the bead's notes as it did
	// before (gt-b9j3o.14, gt-b9j3o.16).
	AuthorKind string `json:"author_kind,omitempty"`
	// PromptSHA is the stored prompt the author seat ran: the sha256 the spawn
	// stamped on the work bead, truncated there to 12 hex digits. It is
	// written only when the bead carries that seat's stamp — a crew landing,
	// an older bead and a spawn that stored no prompt carry no field rather
	// than an empty one (gt-js6di, gt-b9j3o.16).
	PromptSHA string `json:"prompt_sha,omitempty"`
	// Findings is how many findings the verdict carried, whatever their
	// severity.
	Findings int `json:"findings"`
	// Bar is the review threshold in the reviewed tree's .om.json, 0 when the
	// tree names none.
	Bar float64   `json:"bar"`
	At  time.Time `json:"at"`
}

// IsRejection reports whether the landing refused the work on this verdict:
// om asked for changes, or the worker turned an approval away on its findings.
func (r ReviewRecord) IsRejection() bool {
	return r.Rejected || r.Verdict == VerdictRequestChanges
}

// ReviewsFile is the town's append-only om review log, one ReviewRecord per
// line. The landing worker writes it; the dashboard reads it, because the
// daemon's own log is not a source it may parse.
type ReviewsFile struct {
	Path string
}

// TownReviewsFile is <town>/.runtime/landings/reviews.jsonl.
func TownReviewsFile(townRoot string) *ReviewsFile {
	return &ReviewsFile{Path: filepath.Join(constants.TownRuntimePath(townRoot), "landings", ReviewsFileName)}
}

// Append writes rec as one line, with the landings file's guarantees: a 0700
// directory, no symlinked file, a torn tail repaired first and a short write
// rolled back.
func (f *ReviewsFile) Append(rec ReviewRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding review record: %w", err)
	}
	return appendJSONLine(f.Path, append(line, '\n'), nil)
}

// Since returns the records at or after t, in file order. A file that does not
// exist yet is no records, and a torn or unparsable line is skipped, as the
// landings file's readers do: the lines around it are still the log.
func (f *ReviewsFile) Since(t time.Time) ([]ReviewRecord, error) {
	fh, err := os.Open(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening reviews file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	var recs []ReviewRecord
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec ReviewRecord
		if json.Unmarshal(raw, &rec) != nil || rec.At.Before(t) {
			continue
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	return recs, nil
}
