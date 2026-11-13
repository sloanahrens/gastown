package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	previewHead      = "0123456789abcdef0123456789abcdef01234567"
	previewOtherHead = "89abcdef0123456789abcdef0123456789abcdef"
	previewRequest   = reviewPreviewRequestPrefix + previewHead
	previewBranch    = "polecat/slit/gt-jx66mp+mweedkx1"
)

// previewFixture is a reviewPreview over an in-memory work bead. The note
// reader returns notes, the appender adds to them, and each poll moves the fake
// clock by one interval and then runs daemon, which writes the block the test
// wants the daemon to write.
type previewFixture struct {
	p              *reviewPreview
	notes          string
	appended       []string
	polls          int
	start, clock   time.Time
	daemon         func(f *previewFixture)
	stdout, stderr bytes.Buffer
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	f := &previewFixture{start: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)}
	f.clock = f.start
	f.p = &reviewPreview{
		out:      &f.stdout,
		errOut:   &f.stderr,
		bead:     "gt-jx66mp",
		head:     previewHead,
		pushHead: func() error { return nil },
		readNotes: func() (string, error) {
			return f.notes, nil
		},
		appendNote: func(note string) error {
			f.appended = append(f.appended, note)
			f.notes = previewNotesAppend(f.notes, note)
			return nil
		},
		poll: func(d time.Duration) {
			f.polls++
			f.clock = f.clock.Add(d)
			if f.daemon != nil {
				f.daemon(f)
			}
		},
		now:      func() time.Time { return f.clock },
		wait:     10 * time.Minute,
		interval: 30 * time.Second,
	}
	return f
}

// previewNotesAppend adds line to notes on a new line, as bd does.
func previewNotesAppend(notes, line string) string {
	if notes == "" {
		return line
	}
	return notes + "\n" + line
}

// previewBlock is the LOCAL ROUND block the daemon writes for head, with two
// findings.
func previewBlock(head string) string {
	return land.FormatLocalRound(1, head, []land.Finding{
		{ID: "f1", Severity: "major", Path: "internal/cmd/review_preview.go", Line: 42, Title: "poll never ends"},
		{ID: "f2", Severity: "minor", Path: "internal/cmd/x.go", Line: 7, Title: "stale comment"},
	}, time.Date(2026, 11, 13, 3, 1, 0, 0, time.UTC))
}

func TestReviewPreviewPrintsFindings(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	f.daemon = func(f *previewFixture) {
		if f.polls == 2 {
			f.notes = previewNotesAppend(f.notes, previewBlock(previewHead))
		}
	}

	if err := f.p.run(); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if len(f.appended) != 1 || f.appended[0] != previewRequest {
		t.Fatalf("appended = %q, want exactly one %q", f.appended, previewRequest)
	}
	if f.polls != 2 {
		t.Fatalf("polls = %d, want 2: the block is written on the second poll", f.polls)
	}
	for _, want := range []string{"f1 major internal/cmd/review_preview.go:42 poll never ends", "f2 minor internal/cmd/x.go:7 stale comment"} {
		if !strings.Contains(f.stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, f.stdout.String())
		}
	}
	if f.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want none for a round that answered", f.stderr.String())
	}
}

func TestReviewPreviewApprovingRoundSaysNoFindings(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	f.daemon = func(f *previewFixture) {
		if f.polls == 1 {
			f.notes = previewNotesAppend(f.notes, land.FormatLocalRound(1, previewHead, nil, f.clock))
		}
	}

	if err := f.p.run(); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if !strings.Contains(f.stdout.String(), "no findings") {
		t.Errorf("stdout = %q, want it to say no findings", f.stdout.String())
	}
}

func TestReviewPreviewUnavailableNeverBlocks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// daemon writes the daemon's answer, if any, on each poll.
		daemon func(f *previewFixture)
		// reason is a fragment the stderr line must carry.
		reason string
		// fullWait is true when the run must wait out the whole bound.
		fullWait bool
	}{
		{name: "no block before the bound", daemon: nil, reason: "no LOCAL ROUND block", fullWait: true},
		{
			name: "daemon writes an unavailable block",
			daemon: func(f *previewFixture) {
				if f.polls == 1 {
					f.notes = previewNotesAppend(f.notes, "LOCAL ROUND (1): unavailable - om is paused")
				}
			},
			reason: "om is paused",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPreviewFixture(t)
			f.daemon = c.daemon

			if err := f.p.run(); err != nil {
				t.Fatalf("run() = %v, want nil: the preview never fails the submit", err)
			}
			if !strings.Contains(f.stderr.String(), reviewPreviewUnavailable) {
				t.Errorf("stderr = %q, want %q", f.stderr.String(), reviewPreviewUnavailable)
			}
			if !strings.Contains(f.stderr.String(), c.reason) {
				t.Errorf("stderr = %q, want the reason %q", f.stderr.String(), c.reason)
			}
			if f.stdout.Len() != 0 {
				t.Errorf("stdout = %q, want none when the round did not answer", f.stdout.String())
			}
			if c.fullWait && f.clock.Sub(f.start) < f.p.wait {
				t.Errorf("gave up after %s, want the full bound %s", f.clock.Sub(f.start), f.p.wait)
			}
		})
	}
}

func TestReviewPreviewRequestsOncePerHead(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)

	// The daemon does not answer in time: the request is open.
	if err := f.p.run(); err != nil {
		t.Fatalf("first run() = %v, want nil", err)
	}
	// A second run on the same head, the request still open, appends nothing.
	if err := f.p.run(); err != nil {
		t.Fatalf("second run() = %v, want nil", err)
	}
	if len(f.appended) != 1 {
		t.Fatalf("after two runs on one head, appended %d requests, want 1: %q", len(f.appended), f.appended)
	}

	// The daemon answers the open request: the next run prints it and asks nothing.
	f.notes = previewNotesAppend(f.notes, previewBlock(previewHead))
	pollsBefore := f.polls
	if err := f.p.run(); err != nil {
		t.Fatalf("third run() = %v, want nil", err)
	}
	if len(f.appended) != 1 {
		t.Fatalf("a run after the block appended a request: %q", f.appended)
	}
	if f.polls != pollsBefore {
		t.Fatalf("a run after the block polled %d times, want none: the answer is already in the notes", f.polls-pollsBefore)
	}
	if !strings.Contains(f.stdout.String(), "f1 major") {
		t.Errorf("stdout = %q, want the answered findings", f.stdout.String())
	}

	// A new head is a new request.
	f.p.head = previewOtherHead
	f.daemon = nil
	if err := f.p.run(); err != nil {
		t.Fatalf("run on another head = %v, want nil", err)
	}
	if want := reviewPreviewRequestPrefix + previewOtherHead; len(f.appended) != 2 || f.appended[1] != want {
		t.Fatalf("appended = %q, want a second request %q", f.appended, want)
	}
}

func TestReviewPreviewRequestsAgainAfterUnavailable(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	f.daemon = func(f *previewFixture) {
		if f.polls == 1 {
			f.notes = previewNotesAppend(f.notes, "LOCAL ROUND (1): unavailable - om is paused")
		}
	}
	if err := f.p.run(); err != nil {
		t.Fatalf("first run() = %v, want nil", err)
	}

	// The unavailable block closed that request: a new run asks again.
	f.daemon = nil
	if err := f.p.run(); err != nil {
		t.Fatalf("second run() = %v, want nil", err)
	}
	if len(f.appended) != 2 || f.appended[1] != previewRequest {
		t.Fatalf("appended = %q, want a second request after the unavailable block", f.appended)
	}
}

func TestReviewPreviewFailuresNeverBlock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(f *previewFixture)
		// wantAppends is how many requests the run appends before it gives up.
		wantAppends int
	}{
		{
			name: "push to origin fails",
			mutate: func(f *previewFixture) {
				f.p.pushHead = func() error { return errors.New("rejected") }
			},
		},
		{
			name: "first note read fails",
			mutate: func(f *previewFixture) {
				f.p.readNotes = func() (string, error) { return "", errors.New("bd unreachable") }
			},
		},
		{
			name: "request append fails",
			mutate: func(f *previewFixture) {
				f.p.appendNote = func(string) error { return errors.New("bd unreachable") }
			},
		},
		{
			name: "a later note read fails",
			mutate: func(f *previewFixture) {
				read := f.p.readNotes
				f.p.readNotes = func() (string, error) {
					if f.polls > 0 {
						return "", errors.New("bd unreachable")
					}
					return read()
				}
			},
			wantAppends: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPreviewFixture(t)
			c.mutate(f)

			if err := f.p.run(); err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}
			if !strings.Contains(f.stderr.String(), reviewPreviewUnavailable) {
				t.Errorf("stderr = %q, want %q", f.stderr.String(), reviewPreviewUnavailable)
			}
			if f.stdout.Len() != 0 {
				t.Errorf("stdout = %q, want none", f.stdout.String())
			}
			if len(f.appended) != c.wantAppends {
				t.Errorf("appended %d requests, want %d", len(f.appended), c.wantAppends)
			}
		})
	}
}

func TestReviewPreviewEnsureOriginHead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tip      string
		tipErr   error
		wantPush bool
		wantErr  bool
	}{
		{name: "origin already has the head", tip: previewHead, wantPush: false},
		{name: "origin has another tip", tip: previewOtherHead, wantPush: true},
		{name: "origin has no branch", tip: "", wantPush: true},
		{name: "origin cannot be read", tipErr: errors.New("no route to origin"), wantPush: false, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pushed := false
			err := ensureOriginHead(previewHead,
				func() (string, error) { return c.tip, c.tipErr },
				func() error { pushed = true; return nil })
			if (err != nil) != c.wantErr {
				t.Fatalf("ensureOriginHead() error = %v, want error %v", err, c.wantErr)
			}
			if pushed != c.wantPush {
				t.Fatalf("pushed = %v, want %v", pushed, c.wantPush)
			}
		})
	}
}

func TestReviewPreviewReadsTheAnswerForItsHead(t *testing.T) {
	t.Parallel()
	mine := land.FormatLocalRound(1, previewHead, []land.Finding{
		{ID: "f1", Severity: "major", Path: "a.go", Line: 1, Title: "mine"},
	}, time.Date(2026, 11, 13, 3, 1, 0, 0, time.UTC))
	other := land.FormatLocalRound(2, previewOtherHead, nil, time.Date(2026, 11, 13, 3, 2, 0, 0, time.UTC))

	cases := []struct {
		name         string
		after        string // the notes after this head's request
		wantAnswered bool
		wantHead     string
		wantReason   string
	}{
		{name: "a block for this head", after: mine, wantAnswered: true, wantHead: previewHead},
		{name: "a block for this head, then one for another head", after: mine + "\n" + other, wantAnswered: true, wantHead: previewHead},
		{name: "only a block for another head", after: other, wantAnswered: false},
		{name: "an unavailable block after a block for another head", after: other + "\nLOCAL ROUND (3): unavailable - om is paused", wantAnswered: true, wantReason: "om is paused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ans := readReviewPreviewAnswer(previewRequest+"\n"+c.after, previewHead)
			if ans.answered != c.wantAnswered {
				t.Fatalf("answered = %v, want %v", ans.answered, c.wantAnswered)
			}
			if ans.round.Head != c.wantHead {
				t.Errorf("round head = %q, want %q", ans.round.Head, c.wantHead)
			}
			if ans.reason != c.wantReason {
				t.Errorf("reason = %q, want %q", ans.reason, c.wantReason)
			}
		})
	}
}

func TestReviewPreviewNotesOfShownBead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		issue     *beads.Issue
		showErr   error
		wantNotes string
		wantErr   bool
	}{
		{name: "the bead's notes", issue: &beads.Issue{ID: "gt-jx66mp", Notes: previewRequest}, wantNotes: previewRequest},
		{name: "the store cannot read the bead", showErr: errors.New("bd unreachable"), wantErr: true},
		{name: "the store returns no bead", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notes, err := reviewPreviewNotes("gt-jx66mp", func(string) (*beads.Issue, error) {
				return c.issue, c.showErr
			})
			if (err != nil) != c.wantErr {
				t.Fatalf("reviewPreviewNotes() error = %v, want error %v", err, c.wantErr)
			}
			if notes != c.wantNotes {
				t.Fatalf("reviewPreviewNotes() = %q, want %q", notes, c.wantNotes)
			}
		})
	}
}

func TestReviewPreviewBeadFromBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		branch string
		want   string
		ok     bool
	}{
		{branch: "polecat/slit/gt-jx66mp+mweedkx1", want: "gt-jx66mp", ok: true},
		{branch: "crew/seagull/gt-abc", want: "", ok: false},
		{branch: "", want: "", ok: false},
	}
	for _, c := range cases {
		got, ok := reviewPreviewBeadFromBranch(c.branch)
		if got != c.want || ok != c.ok {
			t.Errorf("reviewPreviewBeadFromBranch(%q) = %q, %v; want %q, %v", c.branch, got, ok, c.want, c.ok)
		}
	}
}

// fakePreviewRepo is the git the wiring reads the branch and head with and
// pushes through. originTip is what origin reports for the branch.
type fakePreviewRepo struct {
	branch, head       string
	branchErr, headErr error
	originTip          string
	pushes             []string
}

func (r *fakePreviewRepo) CurrentBranch() (string, error) { return r.branch, r.branchErr }

func (r *fakePreviewRepo) Rev(string) (string, error) { return r.head, r.headErr }

func (r *fakePreviewRepo) PushRemoteBranchTip(string, string) (string, error) {
	return r.originTip, nil
}

func (r *fakePreviewRepo) Push(remote, branch string, force bool) error {
	r.pushes = append(r.pushes, fmt.Sprintf("%s %s force=%v", remote, branch, force))
	return nil
}

// fakePreviewStore is the bead store the wiring reads notes from and appends
// the request to. Every call is recorded with the bead id it named.
type fakePreviewStore struct {
	issues   map[string]*beads.Issue
	shown    []string
	appended []string
}

func (s *fakePreviewStore) Show(id string) (*beads.Issue, error) {
	s.shown = append(s.shown, id)
	return s.issues[id], nil
}

func (s *fakePreviewStore) AppendNotes(id, note string) error {
	s.appended = append(s.appended, id+": "+note)
	iss := s.issues[id]
	if iss == nil {
		return errors.New("bead not found")
	}
	iss.Notes = previewNotesAppend(iss.Notes, note)
	return nil
}

func TestReviewPreviewWiresBranchHeadAndBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		originTip string
		wantPush  bool
	}{
		{name: "origin lacks the head", originTip: previewOtherHead, wantPush: true},
		{name: "origin has the head", originTip: previewHead, wantPush: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakePreviewRepo{branch: previewBranch, head: previewHead, originTip: c.originTip}
			store := &fakePreviewStore{issues: map[string]*beads.Issue{"gt-jx66mp": {ID: "gt-jx66mp"}}}
			clock := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
			polls := 0
			var stdout, stderr bytes.Buffer

			err := runReviewPreviewAt(reviewPreviewDeps{
				out:    &stdout,
				errOut: &stderr,
				repo:   repo,
				store:  store,
				poll: func(d time.Duration) {
					polls++
					clock = clock.Add(d)
					if polls == 2 {
						iss := store.issues["gt-jx66mp"]
						iss.Notes = previewNotesAppend(iss.Notes, previewBlock(previewHead))
					}
				},
				now:      func() time.Time { return clock },
				wait:     reviewPreviewWait,
				interval: reviewPreviewPollEvery,
			})
			if err != nil {
				t.Fatalf("runReviewPreviewAt() = %v, want nil", err)
			}

			wantPushes := 0
			if c.wantPush {
				wantPushes = 1
			}
			if len(repo.pushes) != wantPushes {
				t.Fatalf("pushes = %q, want %d push of the branch to origin", repo.pushes, wantPushes)
			}
			if c.wantPush && repo.pushes[0] != "origin "+previewBranch+" force=false" {
				t.Errorf("push = %q, want a plain push of %s to origin", repo.pushes[0], previewBranch)
			}
			for _, id := range store.shown {
				if id != "gt-jx66mp" {
					t.Errorf("notes read from %q, want the bead the branch names gt-jx66mp", id)
				}
			}
			if want := []string{"gt-jx66mp: " + previewRequest}; strings.Join(store.appended, "|") != strings.Join(want, "|") {
				t.Fatalf("appended = %q, want %q", store.appended, want)
			}
			if !strings.Contains(stdout.String(), "f1 major internal/cmd/review_preview.go:42 poll never ends") {
				t.Errorf("stdout = %q, want the findings of the block on the bead", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want none", stderr.String())
			}
		})
	}
}

// TestReviewPreviewBuildsForTheResolvedDirectory drives the success path
// runReviewPreviewFrom delegates to: the resolved directory is the one the
// dependencies are built for, and the round on them is reported.
func TestReviewPreviewBuildsForTheResolvedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	repo := &fakePreviewRepo{branch: previewBranch, head: previewHead, originTip: previewHead}
	store := &fakePreviewStore{issues: map[string]*beads.Issue{
		"gt-jx66mp": {ID: "gt-jx66mp", Notes: previewRequest + "\n" + previewBlock(previewHead)},
	}}
	var built []string
	var stdout, stderr bytes.Buffer

	err := runReviewPreviewFromWith(
		func() (string, error) { return dir, nil },
		&stdout, &stderr,
		func(cwd string, out, errOut io.Writer) reviewPreviewDeps {
			built = append(built, cwd)
			return reviewPreviewDeps{
				out:    out,
				errOut: errOut,
				repo:   repo,
				store:  store,
				poll: func(time.Duration) {
					t.Errorf("poll ran: the round on the bead answers the request at once")
				},
				now:      time.Now,
				wait:     reviewPreviewWait,
				interval: reviewPreviewPollEvery,
			}
		},
	)
	if err != nil {
		t.Fatalf("runReviewPreviewFromWith() = %v, want nil", err)
	}
	if len(built) != 1 || built[0] != dir {
		t.Fatalf("built for %q, want the resolved directory %s once", built, dir)
	}
	if !strings.Contains(stdout.String(), "f1 major internal/cmd/review_preview.go:42 poll never ends") {
		t.Errorf("stdout = %q, want the findings of the round on the bead", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want none", stderr.String())
	}
}

// TestReviewPreviewProductionBuildBindsItsDirectory runs the production
// constructor on a directory and on a second one: the git it builds reads the
// branch of the first, and the bd store it builds reads that directory's beads.
func TestReviewPreviewProductionBuildBindsItsDirectory(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	writePreviewGitCheckout(t, work, previewBranch)
	if err := os.MkdirAll(filepath.Join(work, ".beads"), 0o755); err != nil {
		t.Fatalf("create the beads directory: %v", err)
	}
	var stdout, stderr bytes.Buffer

	d := reviewPreviewProductionBuild(work, &stdout, &stderr)
	branch, err := d.repo.CurrentBranch()
	if err != nil || branch != previewBranch {
		t.Errorf("current branch = %q, %v, want %s: the git is not bound to %s", branch, err, previewBranch, work)
	}
	store, ok := d.store.(*beads.Beads)
	if !ok {
		t.Fatalf("store = %T, want the bd store", d.store)
	}
	if !store.IsBeadsRepo() {
		t.Errorf("the bd store does not read %s: it finds no beads there", work)
	}

	elsewhere := t.TempDir()
	otherDeps := reviewPreviewProductionBuild(elsewhere, &stdout, &stderr)
	other, ok := otherDeps.store.(*beads.Beads)
	if !ok {
		t.Fatalf("store = %T, want the bd store", otherDeps.store)
	}
	if other.IsBeadsRepo() {
		t.Errorf("the bd store built for %s reads a directory with beads, want its own empty one", elsewhere)
	}
}

// writePreviewGitCheckout writes the files git reads for a plain checkout on
// branch at dir, so CurrentBranch answers without spawning git.
func writePreviewGitCheckout(t *testing.T, dir, branch string) {
	t.Helper()
	ref := filepath.Join(dir, ".git", "refs", "heads", filepath.FromSlash(branch))
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		t.Fatalf("create the branch ref directory: %v", err)
	}
	for path, content := range map[string]string{
		filepath.Join(dir, ".git", "HEAD"): "ref: refs/heads/" + branch + "\n",
		ref:                                previewHead + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func TestReviewPreviewRefusesWithoutWorkingDirectory(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := runReviewPreviewFrom(func() (string, error) {
		return "", errors.New("the caller's directory is not present here")
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runReviewPreviewFrom() = %v, want nil: the preview never fails the submit", err)
	}
	if !strings.Contains(stderr.String(), "no working directory") || !strings.Contains(stderr.String(), reviewPreviewUnavailable) {
		t.Errorf("stderr = %q, want the no-working-directory refusal", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want none", stdout.String())
	}
}

func TestReviewPreviewWiringRefusesBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		repo       *fakePreviewRepo
		wantStderr string
	}{
		{
			name:       "the current branch cannot be read",
			repo:       &fakePreviewRepo{branchErr: errors.New("git gone"), head: previewHead},
			wantStderr: "could not read the current branch",
		},
		{
			name:       "the branch names no work bead",
			repo:       &fakePreviewRepo{branch: "main", head: previewHead},
			wantStderr: "names no work bead",
		},
		{
			name:       "HEAD cannot be read",
			repo:       &fakePreviewRepo{branch: previewBranch, headErr: errors.New("git gone")},
			wantStderr: "could not read HEAD",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakePreviewStore{issues: map[string]*beads.Issue{"gt-jx66mp": {ID: "gt-jx66mp"}}}
			var stdout, stderr bytes.Buffer

			err := runReviewPreviewAt(reviewPreviewDeps{
				out:    &stdout,
				errOut: &stderr,
				repo:   c.repo,
				store:  store,
				poll: func(time.Duration) {
					t.Errorf("poll ran: a refused preview must not wait")
				},
				now:      time.Now,
				wait:     reviewPreviewWait,
				interval: reviewPreviewPollEvery,
			})
			if err != nil {
				t.Fatalf("runReviewPreviewAt() = %v, want nil: the preview never fails the submit", err)
			}
			if !strings.Contains(stderr.String(), c.wantStderr) || !strings.Contains(stderr.String(), reviewPreviewUnavailable) {
				t.Errorf("stderr = %q, want %q and %q", stderr.String(), c.wantStderr, reviewPreviewUnavailable)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want none", stdout.String())
			}
			if len(c.repo.pushes) != 0 || len(store.shown) != 0 || len(store.appended) != 0 {
				t.Errorf("wrote or read after a refusal: pushes %q, shown %q, appended %q", c.repo.pushes, store.shown, store.appended)
			}
		})
	}
}
