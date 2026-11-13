package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
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

func TestReviewPreviewReplyFlagsRefuseAMissingFlag(t *testing.T) {
	t.Parallel()
	// Each row defines a prefix of the three flags, so the read fails at the
	// first one it lacks and the error is returned, not a default.
	cases := []struct {
		name    string
		defined []string
	}{
		{name: "fixed missing", defined: nil},
		{name: "disputed missing", defined: []string{"fixed"}},
		{name: "message missing", defined: []string{"fixed", "disputed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range c.defined {
				cmd.Flags().Bool(name, false, "")
			}
			if _, _, _, err := reviewPreviewReplyFlags(cmd); err == nil {
				t.Errorf("reviewPreviewReplyFlags() = nil error, want a refusal for a command missing a flag")
			}
		})
	}

	fixed, disputed, text, err := reviewPreviewReplyFlags(reviewPreviewReplyCmd)
	if err != nil {
		t.Fatalf("reviewPreviewReplyFlags(reply) = %v, want nil", err)
	}
	if fixed || disputed || text != "" {
		t.Errorf("defaults = %v, %v, %q; want all unset", fixed, disputed, text)
	}
}

func TestReviewPreviewReplyFromRefusesWithoutWorkingDirectory(t *testing.T) {
	t.Parallel()
	resolve := func() (string, error) { return "", errors.New("no cwd") }

	err := runReviewPreviewReplyFrom(resolve, "f1", land.LocalRoundFixed, "bounded the poll loop")
	if err == nil || !strings.Contains(err.Error(), "no working directory") {
		t.Fatalf("runReviewPreviewReplyFrom() = %v, want a refusal naming the working directory", err)
	}
	if !strings.Contains(err.Error(), "no cwd") {
		t.Errorf("runReviewPreviewReplyFrom() = %v, want the resolver's own error carried", err)
	}
}

func TestReviewPreviewReplyAtRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		repo *fakePreviewRepo
		// want is a fragment the error must carry.
		want string
	}{
		{
			name: "branch cannot be read",
			repo: &fakePreviewRepo{branchErr: errors.New("no repo"), head: previewOtherHead},
			want: "could not read the current branch",
		},
		{
			name: "branch names no work bead",
			repo: &fakePreviewRepo{branch: "main", head: previewOtherHead},
			want: "names no work bead",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			store := &fakePreviewStore{issues: map[string]*beads.Issue{}}

			err := runReviewPreviewReplyAt(c.repo, store, "f1", land.LocalRoundFixed, "bounded the poll loop")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("runReviewPreviewReplyAt() = %v, want a refusal naming %q", err, c.want)
			}
			if len(store.shown) != 0 || len(store.appended) != 0 {
				t.Errorf("a refused reply read %q and wrote %q, want no bead touched", store.shown, store.appended)
			}
		})
	}
}

func TestReviewPreviewReplyAtWiresBranchHeadAndBead(t *testing.T) {
	t.Parallel()
	// The branch names gt-jx66mp, so the notes are read from and the reply goes
	// onto that bead, and the head checked is the repo's HEAD.
	repo := &fakePreviewRepo{branch: previewBranch, head: previewOtherHead}
	store := &fakePreviewStore{issues: map[string]*beads.Issue{
		"gt-jx66mp": {ID: "gt-jx66mp", Notes: replyNotesAfterRound()},
	}}

	if err := runReviewPreviewReplyAt(repo, store, "f1", land.LocalRoundFixed, "bounded the poll loop"); err != nil {
		t.Fatalf("runReviewPreviewReplyAt() = %v, want nil", err)
	}
	if len(store.appended) != 1 || !strings.HasPrefix(store.appended[0], "gt-jx66mp: ") {
		t.Fatalf("appended = %q, want one note on gt-jx66mp", store.appended)
	}
	replies := land.ParseLocalRoundReplies(store.issues["gt-jx66mp"].Notes, 1)
	if len(replies) != 1 || replies[0].FindingID != "f1" || replies[0].Kind != land.LocalRoundFixed {
		t.Errorf("replies = %+v, want one fixed reply to f1", replies)
	}
}

func TestReviewPreviewReplyCommandRefusesBeforeAnyRead(t *testing.T) {
	t.Parallel()
	// Each refusal returns before the working directory is resolved, so none of
	// these reaches git or bd.
	cases := []struct {
		name string
		cmd  func(t *testing.T) *cobra.Command
		// want is a fragment the error must carry.
		want string
	}{
		{
			name: "flags not defined",
			cmd:  func(t *testing.T) *cobra.Command { return &cobra.Command{} },
			want: "flag",
		},
		{
			name: "neither --fixed nor --disputed",
			cmd:  func(t *testing.T) *cobra.Command { return replyTestCommand(t) },
			want: "give --fixed or --disputed",
		},
		{
			name: "both --fixed and --disputed",
			cmd:  func(t *testing.T) *cobra.Command { return replyTestCommand(t, "fixed=true", "disputed=true") },
			want: "not both",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := runReviewPreviewReply(c.cmd(t), []string{"f1"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("runReviewPreviewReply() = %v, want a refusal naming %q", err, c.want)
			}
		})
	}
}

// replyTestCommand is a reply command with its three flags defined. Each
// "name=value" in set is parsed onto the flag before the command runs.
func replyTestCommand(t *testing.T, set ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("fixed", false, "")
	cmd.Flags().Bool("disputed", false, "")
	cmd.Flags().StringP("message", "m", "", "")
	for _, kv := range set {
		name, value, _ := strings.Cut(kv, "=")
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", kv, err)
		}
	}
	return cmd
}
