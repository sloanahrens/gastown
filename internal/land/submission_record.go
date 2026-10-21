package land

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/jev/battery"
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
	var recs []SubmissionRecord
	if err := battery.ReadRecent(path, math.MaxInt, &recs); err != nil {
		return nil, err
	}
	return recs, nil
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
	if msgs, err := r.Git.CommitMessages(base, w.Head); err == nil {
		rec.Subjects = commitSubjects(msgs)
		if len(rec.Subjects) > 0 {
			if out, err := r.Git.DiffNumstat(base, w.Head); err == nil {
				rec.Numstat = firstLines(out, submissionNumstatLimit)
			}
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
