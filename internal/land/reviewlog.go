package land

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/constants"
)

// ReviewsFileName is the town's om review log inside the landings directory.
// RigLandingsFile refuses the rig name that would make a rig's landing file
// collide with it.
const ReviewsFileName = "reviews.jsonl"

const (
	// maxReviewLineBytes bounds one line of the review log, for the reader and
	// the writer both. A reader skips a longer line rather than failing the
	// read: one line past the bound would otherwise stop the queue for its rig
	// for good, since every later read would fail on the same line (gt-3arxj).
	maxReviewLineBytes = 1 << 20

	// maxRecordedClaimBytes bounds the claim text one finding keeps on a review
	// line. om's claim is what a follow-up bead is filed from and is short in
	// practice; a runaway one is cut so a line of many findings still fits
	// maxReviewLineBytes (gt-3arxj).
	maxRecordedClaimBytes = 8 << 10

	// maxQueueLineFindings bounds the findings one queue line carries, beside
	// the byte bound, so no landing's held findings make an oversized line
	// however short its claims are (gt-3arxj).
	maxQueueLineFindings = 64

	// maxCompactLineBytes is the longest line compaction will copy. A line past
	// it fails the compaction, which is fail-soft: the queue keeps working, it
	// is just not compacted (gt-3arxj).
	maxCompactLineBytes = 16 << 20
)

// truncatedClaim marks claim text cut at maxRecordedClaimBytes, so a reader
// knows om wrote more than the line keeps.
const truncatedClaim = "…[truncated]"

// reviewsMu serializes this process's writes to the review log: the daemon
// gives the log to one landing worker per rig, and a compaction that ran
// beside another rig's append would replace the file under it and lose the
// line (gt-3arxj). The daemon is the log's only writer.
var reviewsMu sync.Mutex

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
	// follow-up bead is filed from. Claim is om's own account, so it can quote
	// the code it is about; the log is the private landings tree (0600, no
	// symlink), and the claim is cut at maxRecordedClaimBytes so one runaway
	// finding cannot make the line unreadable. A count-only line written before
	// the field carries none, and still reads (gt-3arxj).
	FindingText []ReviewFindingText `json:"finding_text,omitempty"`
	// Queue is set on a queue line, which the follow-up feed appends in place of
	// a verdict: findings an earlier landing's backpressure bound held back. It
	// is a line about the feed's own work rather than a verdict, so the readers
	// that count verdicts skip it (gt-3arxj).
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
			ID: f.ID, Severity: f.Severity, File: f.Path, Line: f.Line, Title: f.Title, Claim: boundedClaim(f.Detail),
		})
	}
	return out
}

// boundedClaim cuts claim text to maxRecordedClaimBytes on a rune boundary and
// marks the cut, so a finding keeps as much of om's account as a line can hold
// and says when it kept only some (gt-3arxj).
func boundedClaim(s string) string {
	if len(s) <= maxRecordedClaimBytes {
		return s
	}
	cut := maxRecordedClaimBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedClaim
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

// ReviewsFile is the town's om review log, one ReviewRecord per line. Verdicts
// are only ever appended; the feed's queue lines are the one thing a later
// landing may take out again (Compact), because they are the feed's own
// bookkeeping and not a record. The landing worker writes it; the dashboard
// reads it, because the daemon's own log is not a source it may parse.
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
	reviewsMu.Lock()
	defer reviewsMu.Unlock()
	return appendJSONLine(f.Path, append(line, '\n'), nil)
}

// AppendQueue appends this landing's held findings as queue lines: findings a
// landing's backpressure bound held back, kept in the log until the feed has
// room to file them (gt-3arxj). A landing whose held findings do not fit one
// line gets one line per chunk, each under the reader's bound, so the feed can
// always read the queue back.
func (f *ReviewsFile) AppendQueue(q QueuedFollowup) error {
	chunks := queueChunks(q)
	reviewsMu.Lock()
	defer reviewsMu.Unlock()
	for _, chunk := range chunks {
		rec := ReviewRecord{BeadID: q.Bead, Rig: q.Rig, At: q.At,
			Queue: &FollowupQueue{Landed: q.Landed, Score: q.Score, Findings: chunk}}
		line, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("encoding review record: %w", err)
		}
		if err := appendJSONLine(f.Path, append(line, '\n'), nil); err != nil {
			return err
		}
	}
	return nil
}

// queueChunks splits a landing's held findings into the lines the queue keeps:
// as many findings as fit under maxReviewLineBytes, at most
// maxQueueLineFindings of them, with each claim bounded first.
func queueChunks(q QueuedFollowup) [][]ReviewFindingText {
	var chunks [][]ReviewFindingText
	var cur []ReviewFindingText
	size := 0
	for _, rt := range q.Findings {
		rt.Claim = boundedClaim(rt.Claim)
		raw, err := json.Marshal(rt)
		if err != nil {
			raw = nil // an unmarshalable field cannot be measured; the byte count below still counts it
		}
		if len(cur) > 0 && (size+len(raw) > maxReviewLineBytes-maxReviewLineBytes/4 || len(cur) >= maxQueueLineFindings) {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, rt)
		size += len(raw) + 1
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// Queued returns the queue lines, oldest first. A torn or unparsable line is
// skipped, and so is one longer than maxReviewLineBytes: the lines around it
// are still the queue, and one line the feed cannot read must not stop it
// draining every line after that one (gt-3arxj).
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
	err = scanReviewLines(fh, func(raw []byte) {
		if len(raw) == 0 {
			return
		}
		var rec ReviewRecord
		if json.Unmarshal(raw, &rec) != nil || rec.Queue == nil {
			return
		}
		queued = append(queued, QueuedFollowup{Bead: rec.BeadID, Rig: rec.Rig, At: rec.At,
			Landed: rec.Queue.Landed, Score: rec.Queue.Score, Findings: rec.Queue.Findings})
	})
	if err != nil {
		return nil, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	return queued, nil
}

// scanReviewLines calls fn with each line of r, trimmed, and skips a line
// longer than maxReviewLineBytes rather than failing the read. A failed read
// would be the same failure on every later read, so one bad line would stop the
// queue for its rig for good; the lines around it are still the log, as with a
// torn line (gt-3arxj). Only an I/O error is returned.
func scanReviewLines(r io.Reader, fn func(line []byte)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	buf := make([]byte, 0, 64*1024)
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !over {
			if len(buf)+len(chunk) > maxReviewLineBytes {
				over = true
				buf = buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if !over {
				fn(bytes.TrimSpace(buf))
			}
			over, buf = false, buf[:0]
		case errors.Is(err, bufio.ErrBufferFull):
			// The line runs past the read buffer; keep reading it.
		case errors.Is(err, io.EOF):
			// A file that does not end in a newline still ends in a line.
			if !over {
				fn(bytes.TrimSpace(buf))
			}
			return nil
		default:
			return err
		}
	}
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
	err = scanReviewLines(fh, func(raw []byte) {
		if len(raw) == 0 {
			return
		}
		var rec ReviewRecord
		if json.Unmarshal(raw, &rec) != nil || rec.At.Before(t) || rec.Queue != nil {
			return
		}
		recs = append(recs, rec)
	})
	if err != nil {
		return nil, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	return recs, nil
}

// Compact rewrites the log without the queue lines the feed is done with:
// consumed reports whether a queue line's findings are all covered, and such a
// line is dropped. Every other line — a verdict, a queue line still waiting,
// even a line no reader can parse — is copied byte for byte, so no record is
// rewritten and a count-only record survives as it was (gt-3arxj). It returns
// how many lines it dropped.
//
// The new file is written beside the log and renamed over it, so a reader sees
// the log before or after the compaction and never a half-written one. Every
// write to the log in this process holds reviewsMu, so no other rig's landing
// worker can append into the file that is being replaced.
func (f *ReviewsFile) Compact(consumed func(QueuedFollowup) bool) (int, error) {
	reviewsMu.Lock()
	defer reviewsMu.Unlock()
	dir := filepath.Dir(f.Path)
	if err := ensureLandingsDir(dir); err != nil {
		return 0, err
	}
	fh, err := os.OpenFile(f.Path, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("opening reviews file: %w", err)
	}
	var kept [][]byte
	dropped := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), maxCompactLineBytes)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec ReviewRecord
		if json.Unmarshal(raw, &rec) == nil && rec.Queue != nil && consumed(QueuedFollowup{
			Bead: rec.BeadID, Rig: rec.Rig, At: rec.At,
			Landed: rec.Queue.Landed, Score: rec.Queue.Score, Findings: rec.Queue.Findings,
		}) {
			dropped++
			continue
		}
		kept = append(kept, append([]byte(nil), raw...))
	}
	serr := sc.Err()
	if cerr := fh.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return 0, fmt.Errorf("reading reviews file %s: %w", f.Path, serr)
	}
	if dropped == 0 {
		return 0, nil
	}
	tmp := f.Path + ".compact"
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC|oNoFollow, 0o600)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", tmp, err)
	}
	for _, line := range kept {
		if _, err := tf.Write(append(line, '\n')); err != nil {
			_ = tf.Close()
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("writing %s: %w", tmp, err)
		}
	}
	if err := tf.Sync(); err != nil {
		_ = tf.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("syncing %s: %w", tmp, err)
	}
	if err := tf.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.Path); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("replacing reviews file %s: %w", f.Path, err)
	}
	return dropped, nil
}
