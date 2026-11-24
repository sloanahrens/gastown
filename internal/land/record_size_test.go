package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/git"
)

// numstatRepo is the fixture's repository handle with the landed diff's
// numstat and the commit log canned: every other read of the landing goes to
// the in-memory fake, so the landing path itself is the one under test.
type numstatRepo struct {
	Repo
	out   string
	err   error
	calls []string

	commits   []git.CommitLineStats // the commit-log read; nil asks the fake
	commitErr error
}

func (r *numstatRepo) DiffNumstat(base, head string) (string, error) {
	r.calls = append(r.calls, base+".."+head)
	return r.out, r.err
}

// CommitLineStatsInRange answers from the test's canned log only: a fake with
// no log canned fails, so no test reads a repository for a count it did not
// set (gt-dhun8o).
func (r *numstatRepo) CommitLineStatsInRange(revRange string, limit int) ([]git.CommitLineStats, error) {
	if r.commits == nil && r.commitErr == nil {
		return nil, errors.New("no commit log canned")
	}
	return r.commits, r.commitErr
}

// sizeLander is the fixture's lander with its repository reads wrapped, so the
// numstat the landing records is the test's own canned output.
func (f *landFixture) sizeLander(t *testing.T, repo *numstatRepo, out *bytes.Buffer) *Lander {
	t.Helper()
	lf, err := RigLandingsFile(f.town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if f.merger == nil {
		f.merger = &localMerger{f: f}
	}
	l := f.lander()
	l.Landings = lf
	l.Out = out
	l.openRepo = func(dir string) Repo {
		repo.Repo = f.git.Open(dir)
		return repo
	}
	return l
}

// TestRunawayBeadLandingRecordsItsSize: the landing record carries the added
// lines and file count of the landed diff, summed from its numstat. A binary
// file's "-" adds no lines and still counts as a file, and a quoted path (a
// space or a non-ASCII name) counts as one file (gt-8ldz1g).
func TestRunawayBeadLandingRecordsItsSize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		numstat      string
		added, files int
	}{
		{
			name:    "the added column is summed and each file counts",
			numstat: "12\t3\tinternal/land/land.go\n5\t0\tinternal/land/note.go\n",
			added:   17, files: 2,
		},
		{
			name:    "a binary file adds no lines and counts as a file",
			numstat: "-\t-\tassets/logo.png\n4\t1\tinternal/land/land.go\n",
			added:   4, files: 2,
		},
		{
			name:    "a quoted path counts as one file",
			numstat: "1\t0\t\"docs/a b.md\"\n2\t0\t\"docs/café.md\"\n",
			added:   3, files: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			repo := &numstatRepo{out: tc.numstat}
			l := f.sizeLander(t, repo, &bytes.Buffer{})

			if _, err := l.Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			rec := f.recordSize(t)
			if rec.AddedLines != tc.added || rec.FilesChanged != tc.files {
				t.Errorf("record size = %d added lines, %d files; want %d and %d",
					rec.AddedLines, rec.FilesChanged, tc.added, tc.files)
			}
			if rec.Commits != 0 {
				t.Errorf("record commits = %d, want none: the fake canned no log", rec.Commits)
			}
			// The size is the landed diff's, read from the base the record
			// holds to the commit that landed — never the submitted head.
			if want := []string{rec.Base + ".." + rec.LandedCommit}; len(repo.calls) != 1 || repo.calls[0] != want[0] {
				t.Errorf("numstat read %q; want %q", repo.calls, want)
			}
		})
	}
}

// TestRunawayBeadUnreadableDiffLeavesSizeZero: a diff that cannot be read
// leaves both zero, is logged, and the landing is recorded all the same
// (gt-8ldz1g).
func TestRunawayBeadUnreadableDiffLeavesSizeZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo *numstatRepo
	}{
		{"the diff command fails", &numstatRepo{err: errors.New("no such revision")}},
		{"the output is not numstat", &numstatRepo{out: "this is not a numstat line\n"}},
		{"a count is not an integer", &numstatRepo{out: "x\t0\ta.go\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			var log bytes.Buffer
			l := f.sizeLander(t, tc.repo, &log)

			if _, err := l.Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v; an unreadable size must not fail the landing", err)
			}
			rec := f.recordSize(t)
			if rec.AddedLines != 0 || rec.FilesChanged != 0 {
				t.Errorf("record size = %d added lines, %d files; want zeros", rec.AddedLines, rec.FilesChanged)
			}
			if !strings.Contains(log.String(), "landing size not recorded") {
				t.Errorf("landing log = %q; want it to name the size it could not record", log.String())
			}
		})
	}
}

// TestLandingRecordCountsItsRangeCommits: the record carries how many commits
// the risk-path file list covers, base..head; two landings in a row each
// record their own head, count and files (gt-dhun8o).
func TestLandingRecordCountsItsRangeCommits(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// The tip auto-save commit squashes the landing: base..landed reads 1
	// where base..head, the file list's range, reads 2.
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	f.git.Commit(t, f.origin, fixtureBranch, "feat: add b", map[string]string{"b.txt": "work\n"})
	two := f.git.Commit(t, f.origin, fixtureBranch, checkpoint.WIPCommitPrefix+" 2026-09-30", map[string]string{"c.txt": "more\n"})
	f.ready(two)
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("first Land: %v", err)
	}
	second := Work{BeadID: "gt-def", Rig: "gastown", Branch: "polecat/opal/gt-def+x2", Target: "main", Worker: "opal"}
	f.git.SetRef(t, f.origin, "refs/heads/"+second.Branch, f.originMain())
	second.Head = f.git.Commit(t, f.origin, second.Branch, "feat: add d", map[string]string{"d.txt": "d\n"})
	f.bd.Seed(beads.Issue{ID: "gt-def", Status: "hooked", Type: "task", Labels: []string{LabelReadyToLand},
		Assignee: "gastown/polecats/opal", Notes: FormatReadyNote(second)})
	f.base = f.originMain()
	if _, err := f.lander().Land(context.Background(), second); err != nil {
		t.Fatalf("second Land: %v", err)
	}
	lines := f.landingLines()
	if len(lines) != 2 {
		t.Fatalf("landings file holds %d records, want 2", len(lines))
	}
	for i, want := range []struct {
		head           string
		commits, files int
	}{{two, 2, 2}, {second.Head, 1, 1}} {
		var rec LandingRecord
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if i == 0 && rec.LandedCommit == rec.Head {
			t.Fatalf("the first landing did not squash, so the test cannot tell base..head from base..landed")
		}
		if rec.Head != want.head || rec.Commits != want.commits || rec.FilesChanged != want.files {
			t.Errorf("record %d = %s, %d commits, %d files; want %s, %d and %d",
				i, rec.Head, rec.Commits, rec.FilesChanged, want.head, want.commits, want.files)
		}
	}
}

// TestLandingRecordHasNoCommitCountWhenTheRangeIsUncountable: a failed log
// read, and a walk its limit stops, record no count and the landing stands
// (gt-dhun8o).
func TestLandingRecordHasNoCommitCountWhenTheRangeIsUncountable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo *numstatRepo
	}{
		{"the log fails", &numstatRepo{commitErr: errors.New("bad revision")}},
		{"the limit stops the walk", &numstatRepo{commits: make([]git.CommitLineStats, landingCommitScanLimit)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			var log bytes.Buffer
			if _, err := f.sizeLander(t, tc.repo, &log).Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v; an uncountable range must not fail the landing", err)
			}
			if rec := f.recordSize(t); rec.Commits != 0 {
				t.Errorf("record commits = %d, want none", rec.Commits)
			}
			if tc.repo.commitErr != nil && !strings.Contains(log.String(), "landing commit count not recorded") {
				t.Errorf("landing log = %q; want the failed count", log.String())
			}
		})
	}
}

// recordSize reads the one landing record the fixture's lander appended.
func (f *landFixture) recordSize(t *testing.T) LandingRecord {
	t.Helper()
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landings file holds %d records; want 1", len(lines))
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decoding the landing record: %v", err)
	}
	return rec
}
