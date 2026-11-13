package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// replyFixture is a reviewPreviewReply over an in-memory work bead. The notes
// start as the test sets them; the branch head is head, which the test moves
// to model a commit made after the round.
type replyFixture struct {
	r        reviewPreviewReply
	notes    string
	appended []string
	head     string
}

func newReplyFixture(notes string) *replyFixture {
	f := &replyFixture{notes: notes, head: previewOtherHead}
	f.r = reviewPreviewReply{
		bead:       "gt-16x50l",
		branchHead: func() (string, error) { return f.head, nil },
		readNotes:  func() (string, error) { return f.notes, nil },
		appendNote: func(note string) error {
			f.appended = append(f.appended, note)
			f.notes = previewNotesAppend(f.notes, note)
			return nil
		},
	}
	return f
}

// replyNotesAfterRound is the notes of a bead the daemon answered: the request
// for previewHead, then the round it wrote.
func replyNotesAfterRound() string {
	return previewRequest + "\n" + previewBlock(previewHead)
}

func TestReviewPreviewReplyAppends(t *testing.T) {
	t.Parallel()
	f := newReplyFixture(replyNotesAfterRound())

	if err := f.r.run("f1", land.LocalRoundFixed, "bounded the poll loop"); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if len(f.appended) != 1 {
		t.Fatalf("appended %d notes, want exactly one: %q", len(f.appended), f.appended)
	}
	replies := land.ParseLocalRoundReplies(f.notes, 1)
	if len(replies) != 1 {
		t.Fatalf("round 1 has %d replies in the notes, want 1: %q", len(replies), f.notes)
	}
	if got := replies[0]; got.FindingID != "f1" || got.Kind != land.LocalRoundFixed {
		t.Errorf("reply = %+v, want finding f1 with kind %q", got, land.LocalRoundFixed)
	}
}

func TestReviewPreviewReplyRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// mutate sets up the fixture for the refusal.
		mutate func(f *replyFixture)
		// finding and kind are the reply the run is asked for.
		finding, kind, text string
		// want is a fragment the error must carry: the problem it names.
		want string
	}{
		{
			name:    "unknown finding id",
			mutate:  func(f *replyFixture) {},
			finding: "f9", kind: land.LocalRoundFixed, text: "bounded the poll loop",
			want: "finding f9 is not in LOCAL ROUND (1)",
		},
		{
			name:    "empty text on a fixed reply",
			mutate:  func(f *replyFixture) {},
			finding: "f1", kind: land.LocalRoundFixed, text: "   ",
			want: "no text",
		},
		{
			name:    "fixed with the head the round reviewed",
			mutate:  func(f *replyFixture) { f.head = previewHead },
			finding: "f1", kind: land.LocalRoundFixed, text: "bounded the poll loop",
			want: "nothing changed since the round",
		},
		{
			name: "branch head cannot be read",
			mutate: func(f *replyFixture) {
				f.r.branchHead = func() (string, error) { return "", errors.New("no HEAD") }
			},
			finding: "f1", kind: land.LocalRoundFixed, text: "bounded the poll loop",
			want: "could not read the branch head",
		},
		{
			name: "notes cannot be read",
			mutate: func(f *replyFixture) {
				f.r.readNotes = func() (string, error) { return "", errors.New("bd down") }
			},
			finding: "f1", kind: land.LocalRoundFixed, text: "bounded the poll loop",
			want: "could not read the notes of gt-16x50l",
		},
		{
			name: "no round has been written yet",
			mutate: func(f *replyFixture) {
				f.notes = previewRequest
			},
			finding: "f1", kind: land.LocalRoundFixed, text: "bounded the poll loop",
			want: "no LOCAL ROUND block on gt-16x50l",
		},
		{
			name: "the append fails",
			mutate: func(f *replyFixture) {
				f.r.appendNote = func(string) error { return errors.New("bd down") }
			},
			finding: "f1", kind: land.LocalRoundFixed, text: "bounded the poll loop",
			want: "could not append the reply to gt-16x50l",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newReplyFixture(replyNotesAfterRound())
			c.mutate(f)
			before := f.notes

			err := f.r.run(c.finding, c.kind, c.text)
			if err == nil {
				t.Fatalf("run() = nil, want a refusal naming %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("run() = %q, want it to name %q", err, c.want)
			}
			if len(f.appended) != 0 || f.notes != before {
				t.Errorf("a refused reply wrote %q, want nothing", f.appended)
			}
		})
	}
}

func TestReviewPreviewReplyDisputedNeedsReason(t *testing.T) {
	t.Parallel()
	f := newReplyFixture(replyNotesAfterRound())

	// The head is unchanged: a disputed reply needs no fix, so it is not refused
	// for the head.
	f.head = previewHead
	err := f.r.run("f2", land.LocalRoundDisputed, "")
	if err == nil || !strings.Contains(err.Error(), "--disputed needs a reason") {
		t.Fatalf("run() without text = %v, want a refusal naming the missing reason", err)
	}
	if len(f.appended) != 0 {
		t.Fatalf("a disputed reply without text wrote %q, want nothing", f.appended)
	}

	if err := f.r.run("f2", land.LocalRoundDisputed, "the comment matches the code"); err != nil {
		t.Fatalf("run() with a reason = %v, want nil", err)
	}
	replies := land.ParseLocalRoundReplies(f.notes, 1)
	if len(replies) != 1 || replies[0].FindingID != "f2" || replies[0].Kind != land.LocalRoundDisputed {
		t.Errorf("replies = %+v, want one disputed reply to f2", replies)
	}
}

func TestReviewPreviewReplyKindNeedsExactlyOneFlag(t *testing.T) {
	t.Parallel()
	if _, err := reviewPreviewReplyKind(false, false); err == nil {
		t.Errorf("neither flag: want a refusal")
	}
	if _, err := reviewPreviewReplyKind(true, true); err == nil {
		t.Errorf("both flags: want a refusal")
	}
	if got, err := reviewPreviewReplyKind(true, false); err != nil || got != land.LocalRoundFixed {
		t.Errorf("--fixed = %q, %v; want %q", got, err, land.LocalRoundFixed)
	}
	if got, err := reviewPreviewReplyKind(false, true); err != nil || got != land.LocalRoundDisputed {
		t.Errorf("--disputed = %q, %v; want %q", got, err, land.LocalRoundDisputed)
	}
}
