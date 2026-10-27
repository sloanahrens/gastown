package land

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/lock"
)

const (
	// submissionSubjectLimit caps the commit subjects a record holds, the
	// same cap the submission battery puts on its state.
	submissionSubjectLimit = 20
	// submissionNumstatLimit caps the numstat lines a record holds.
	submissionNumstatLimit = 80
)

// SubmissionRecord is the first-sight record of one submission: what was
// submitted, and the commits the landing worker read from its clone at that
// moment. The battery asks from it after the branch is pruned at landing.
type SubmissionRecord struct {
	// SeenAt is when the landing worker first saw the submission. The
	// battery orders its asks by it.
	SeenAt time.Time `json:"seen_at"`
	Bead   string    `json:"bead"`
	Rig    string    `json:"rig"`
	Branch string    `json:"branch"`
	Head   string    `json:"head"`
	Target string    `json:"target"`
	// Submitted is the READY TO LAND block's time, zero when the block has
	// none.
	Submitted time.Time `json:"submitted,omitzero"`
	// Subjects are the commit subjects on target..head, newest first, at
	// most submissionSubjectLimit. Empty when the clone could not read them,
	// and the battery then reads its own clone.
	Subjects []string `json:"subjects,omitempty"`
	// Numstat is `git diff --numstat` for target..head, at most
	// submissionNumstatLimit lines. Written only with Subjects.
	Numstat string `json:"numstat,omitempty"`
}

// SubmissionsSeenFile is the name of the first-sight log under the town's jev
// runtime directory.
const SubmissionsSeenFile = "submissions-seen.jsonl"

// SubmissionsSeenPath is the first-sight log: one record per submission, written
// by the landing worker and read by the submission battery.
func SubmissionsSeenPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", SubmissionsSeenFile)
}

// ReadSubmissionRecords returns every record in the log at path, oldest first.
// A missing log is empty, and a line that does not parse is skipped.
func ReadSubmissionRecords(path string) ([]SubmissionRecord, error) {
	return ReadRecentSubmissionRecords(path, math.MaxInt)
}

// FirstSeenIn answers when the host first saw a bead's submission at a head,
// from the first-sight log at path. The answer is the oldest record for the
// key, because that is the first sight; a bead and head with no record answers
// zero and no error, and the caller must read that as "the host saw nothing",
// not as a time (gt-8yk78).
func FirstSeenIn(path string) func(bead, head string) (time.Time, error) {
	return func(bead, head string) (time.Time, error) {
		recs, err := ReadSubmissionRecords(path)
		if err != nil {
			return time.Time{}, fmt.Errorf("reading the first-sight log: %w", err)
		}
		var seen time.Time
		for _, rec := range recs {
			if rec.Bead != bead || rec.Head != head {
				continue
			}
			if seen.IsZero() || rec.SeenAt.Before(seen) {
				seen = rec.SeenAt
			}
		}
		return seen, nil
	}
}

// ReadRecentSubmissionRecords returns the newest n records in the log at path,
// oldest first. A missing log is empty, and a line that does not parse is
// skipped before the n are taken.
func ReadRecentSubmissionRecords(path string, n int) ([]SubmissionRecord, error) {
	var recs []SubmissionRecord
	if err := battery.ReadRecent(path, n, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// TrimSubmissionRecords drops the records keep rejects from the log at path,
// and returns how many it dropped. A kept line is copied byte for byte, and a
// line that does not parse is kept, so the record shape never changes.
//
// The log is rewritten in place, under the lock Append takes. Every reader and
// writer locks the log's own inode, so the lock excludes all of them only while
// the log stays that inode. A rename over the log would leave an Append that
// had already opened the old inode holding a lock nobody else takes.
func TrimSubmissionRecords(path string, keep func(SubmissionRecord) bool) (int, error) {
	// FlockAcquire creates a missing lock file at 0644, and the log is 0600,
	// so a missing log is left alone before the lock is taken.
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	unlock, err := lock.FlockAcquire(path)
	if err != nil {
		return 0, fmt.Errorf("locking %s: %w", path, err)
	}
	defer unlock()

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// The whole log is read before a byte is written back, so the rewrite
	// never overwrites a line it has not read. Lines have no length cap: a
	// record over a scanner's limit would stop the trim, and the log could
	// then never shrink.
	var kept bytes.Buffer
	dropped := 0
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rec SubmissionRecord
			if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &rec) == nil && !keep(rec) {
				dropped++
			} else {
				kept.Write(line)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if dropped == 0 {
		return 0, nil
	}
	if _, err := f.WriteAt(kept.Bytes(), 0); err != nil {
		return 0, fmt.Errorf("writing the trimmed log: %w", err)
	}
	if err := f.Truncate(int64(kept.Len())); err != nil {
		return 0, fmt.Errorf("trimming %s: %w", path, err)
	}
	return dropped, nil
}

// SubmissionKey names one submission by its bead and head, so a resubmission at
// a new head is a new submission.
func SubmissionKey(bead, head string) string {
	return bead + " " + head
}

// SubmissionGit reads a submission's commits from the rig's clone.
// *git.Git is the production one.
type SubmissionGit interface {
	CommitMessages(base, head string) ([]git.CommitMessage, error)
	DiffNumstat(base, head string) (string, error)
}

var _ SubmissionGit = (*git.Git)(nil)

// SubmissionRecorder writes a submission's first-sight record the first time the
// landing worker sees it. It is not safe for concurrent use: one goroutine calls
// it.
type SubmissionRecorder struct {
	// Path is the first-sight log, SubmissionsSeenPath in production.
	Path string
	// Git reads the commits, from the rig's clone the landing worker lands
	// through: a polecat's branch is in it from the moment the polecat pushes.
	Git SubmissionGit
	// Remote is the remote whose target branch the commits are read against,
	// the ref the landing merges onto. Empty means origin.
	Remote string
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Warn reports a clone read that failed, so a record written without its
	// commits is visible in the daemon log. Nil drops the report.
	Warn func(format string, args ...any)

	keys   map[string]bool
	loaded bool
}

// Record writes w's first-sight record unless one already exists for its bead
// and head, and reports whether it wrote one. The keys already written are read
// from the log once, so a recorded submission reads no commits. The clone is
// not fetched, so a clone that lacks the commits writes a record without
// subjects, and the battery reads them from its own clone instead.
func (r *SubmissionRecorder) Record(w Work) (bool, error) {
	if !r.loaded {
		recs, err := ReadSubmissionRecords(r.Path)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", r.Path, err)
		}
		r.keys = make(map[string]bool, len(recs))
		for _, rec := range recs {
			r.keys[SubmissionKey(rec.Bead, rec.Head)] = true
		}
		r.loaded = true
	}
	key := SubmissionKey(w.BeadID, w.Head)
	if r.keys[key] {
		return false, nil
	}

	rec := SubmissionRecord{
		SeenAt:    r.now(),
		Bead:      w.BeadID,
		Rig:       w.Rig,
		Branch:    w.Branch,
		Head:      w.Head,
		Target:    w.Target,
		Submitted: w.Submitted,
	}
	base := r.remote() + "/" + w.Target
	msgs, err := r.Git.CommitMessages(base, w.Head)
	if err != nil {
		r.warnf("%s: first-sight record written without commits, the battery reads its own clone: %v", w.BeadID, err)
	} else if subjects := commitSubjects(msgs); len(subjects) > 0 {
		out, err := r.Git.DiffNumstat(base, w.Head)
		if err != nil {
			// A record holds subjects with their stat or neither, so the
			// battery never asks about commits against a diff it lacks.
			r.warnf("%s: first-sight record written without commits, the stat could not be read: %v", w.BeadID, err)
		} else {
			rec.Subjects = subjects
			rec.Numstat = firstLines(out, submissionNumstatLimit)
		}
	}
	if err := battery.Append(r.Path, rec); err != nil {
		return false, fmt.Errorf("appending to %s: %w", r.Path, err)
	}
	r.keys[key] = true
	return true, nil
}

func (r *SubmissionRecorder) remote() string {
	if r.Remote == "" {
		return "origin"
	}
	return r.Remote
}

func (r *SubmissionRecorder) warnf(format string, args ...any) {
	if r.Warn != nil {
		r.Warn(format, args...)
	}
}

func (r *SubmissionRecorder) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// commitSubjects returns the first line of each message, newest first, at most
// submissionSubjectLimit. Nil when there are none.
func commitSubjects(msgs []git.CommitMessage) []string {
	var out []string
	for _, m := range msgs {
		if len(out) == submissionSubjectLimit {
			break
		}
		subject, _, _ := strings.Cut(strings.TrimSpace(m.Message), "\n")
		out = append(out, strings.TrimSpace(subject))
	}
	return out
}

// firstLines keeps at most n lines of s, with no trailing newline.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
