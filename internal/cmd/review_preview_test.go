package cmd

import (
	"bytes"
	"errors"
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
