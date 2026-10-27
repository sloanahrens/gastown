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
	// overseer or unknown (authorkind.Kind). Lines written before the field
	// carry none, and a reader attributes them from the bead's notes as it did
	// before (gt-b9j3o.14, gt-b9j3o.16).
	AuthorKind string `json:"author_kind,omitempty"`
	// PromptSHA is the stored prompt the author seat ran: the sha256 the spawn
	// stamped on the work bead (promptstamp.Format). It is written only when
	// the bead carries that seat's stamp — a crew landing, an older bead and a
	// spawn that stored no prompt carry no field rather than an empty one
	// (gt-js6di, gt-b9j3o.16).
	PromptSHA string `json:"prompt_sha,omitempty"`
	// Findings is how many findings the verdict carried, whatever their
	// severity.
	Findings int `json:"findings"`
	// FindingText is those findings whole: the file, line, title and claim a
	// follow-up bead is filed from. A count-only line written before the field
	// carries none, and still reads (gt-3arxj).
	FindingText []ReviewFindingText `json:"finding_text,omitempty"`
	// Queue is set on a queue line, which the follow-up feed appends in place of
	// a verdict: findings an earlier landing's backpressure bound held back. A
	// queue line carries no verdict, so the readers that count verdicts skip it
	// (gt-3arxj).
	Queue *FollowupQueue `json:"followup_queue,omitempty"`
	// Bar is the review threshold in the reviewed tree's .om.json, 0 when the
	// tree names none.
	Bar float64   `json:"bar"`
	At  time.Time `json:"at"`
}

// ReviewFindingText is one om finding as the review log keeps it (gt-3arxj).
type ReviewFindingText struct {
	ID       string `json:"id,omitempty"`
	Severity string `json:"severity,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title,omitempty"`
	// Claim is om's own account of the finding, which no other field of the
	// record carries (gt-3arxj).
	Claim string `json:"claim,omitempty"`
}

// reviewFindingTexts is a verdict's findings as its record keeps them.
func reviewFindingTexts(findings []Finding) []ReviewFindingText {
	if len(findings) == 0 {
		return nil
	}
	out := make([]ReviewFindingText, 0, len(findings))
	for _, f := range findings {
		out = append(out, ReviewFindingText{
			ID: f.ID, Severity: f.Severity, File: f.Path, Line: f.Line, Title: f.Title, Claim: f.Detail,
		})
	}
	return out
}

// FollowupQueue is a queue line's payload: the findings one landing's
// backpressure bound held back, with what a later bead needs to name the
// landing that raised them (gt-3arxj).
type FollowupQueue struct {
	// Landed is the commit the held landing merged.
	Landed string `json:"landed,omitempty"`
	// Score is the score om approved that landing at.
	Score    float64             `json:"score,omitempty"`
	Findings []ReviewFindingText `json:"findings"`
}

// QueuedFollowup is one queue line as the feed reads it: the landing whose
// findings were held, and the findings themselves.
type QueuedFollowup struct {
	Bead     string
	Rig      string
	At       time.Time
	Landed   string
	Score    float64
	Findings []ReviewFindingText
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

// AppendQueue appends one queue line: findings a landing's backpressure bound
// held back, kept in the log until the feed has room to file them (gt-3arxj).
func (f *ReviewsFile) AppendQueue(q QueuedFollowup) error {
	return f.Append(ReviewRecord{BeadID: q.Bead, Rig: q.Rig, At: q.At,
		Queue: &FollowupQueue{Landed: q.Landed, Score: q.Score, Findings: q.Findings}})
}

// Queued returns the queue lines, oldest first. A torn or unparsable line is
// skipped: the lines around it are still the queue.
func (f *ReviewsFile) Queued() ([]QueuedFollowup, error) {
	fh, err := os.Open(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening reviews file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	var queued []QueuedFollowup
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec ReviewRecord
		if json.Unmarshal(raw, &rec) != nil || rec.Queue == nil {
			continue
		}
		queued = append(queued, QueuedFollowup{Bead: rec.BeadID, Rig: rec.Rig, At: rec.At,
			Landed: rec.Queue.Landed, Score: rec.Queue.Score, Findings: rec.Queue.Findings})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	return queued, nil
}

// Since returns the verdict records at or after t, in file order. A file that
// does not exist yet is no records, and a torn or unparsable line is skipped,
// as the landings file's readers do: the lines around it are still the log. A
// queue line is not a verdict and is left out, so a reader joining a bead's
// first record joins its verdict rather than the feed's bookkeeping
// (gt-3arxj).
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
		if json.Unmarshal(raw, &rec) != nil || rec.At.Before(t) || rec.Queue != nil {
			continue
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	return recs, nil
}
