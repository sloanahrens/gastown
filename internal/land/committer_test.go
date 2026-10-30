package land

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// A landing commit's identity is explicit on every variable: the landing
// committer, and the head's author in both the name and the email, so neither
// the operator's git config nor an inherited environment reaches it (gt-vuv2n).
func TestCommittingEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		author  Identity
		want    []string
		wantErr bool
	}{
		{
			name:   "seat author",
			author: Identity{Name: "opal", Email: "opal@gastown.local"},
			want: []string{
				"GIT_COMMITTER_NAME=gt-landing",
				"GIT_COMMITTER_EMAIL=gt-landing@gastown.local",
				"GIT_AUTHOR_NAME=opal",
				"GIT_AUTHOR_EMAIL=opal@gastown.local",
			},
		},
		{
			name:   "human-looking author",
			author: Identity{Name: "Sloan Ahrens", Email: "sloan@example.com"},
			want: []string{
				"GIT_COMMITTER_NAME=gt-landing",
				"GIT_COMMITTER_EMAIL=gt-landing@gastown.local",
				"GIT_AUTHOR_NAME=Sloan Ahrens",
				"GIT_AUTHOR_EMAIL=sloan@example.com",
			},
		},
		{
			name:    "empty author name is an error",
			author:  Identity{Email: "opal@gastown.local"},
			wantErr: true,
		},
		{
			name:    "empty author email is an error",
			author:  Identity{Name: "opal"},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CommittingEnv(tc.author)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CommittingEnv(%+v) = %q, want an error", tc.author, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("CommittingEnv(%+v): %v", tc.author, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("CommittingEnv(%+v) = %q, want %q", tc.author, got, tc.want)
			}
		})
	}
}

// An unreadable head author fails the landing before the worktree is cut: the
// error names the head, and the target does not move (gt-vuv2n).
func TestLandFailsClosedWhenHeadAuthorUnreadable(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.authorOf = func(Work) (Identity, error) { return Identity{}, errors.New("no author on the head") }
	before := f.originMain()

	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "head author" {
		t.Fatalf("Land error = %v, want an InfraError at stage head author", err)
	}
	if !strings.Contains(err.Error(), shortSHA(f.work.Head)) {
		t.Errorf("Land error %q does not name the head %s", err, shortSHA(f.work.Head))
	}
	if got := f.originMain(); got != before {
		t.Errorf("origin main moved from %s to %s; a landing with no head author makes no commit", before, got)
	}
}

// A handle that cannot read commit authors is refused, not read around: the
// landing never falls back to git config for the author (gt-vuv2n).
func TestHeadAuthorRefusesAHandleWithoutAuthors(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.authorOf = nil
	_, err := l.headAuthor(f.git.Open(f.repo), f.base, f.work)
	if err == nil || !strings.Contains(err.Error(), "cannot read commit authors") {
		t.Fatalf("headAuthor on a handle without authors = %v, want a refusal", err)
	}
}

// summariesRepo is a handle whose range read is answered by the test, so the
// author read's failure and not-in-range paths run without a subprocess.
type summariesRepo struct {
	Repo
	sums []git.CommitSummary
	err  error
}

func (r summariesRepo) FullRangeCommitSummaries(base, head string) ([]git.CommitSummary, error) {
	return r.sums, r.err
}

// The head's author is the summary of the head commit itself: a read that
// fails, or a range that does not list the head, is an error and names no one
// (gt-vuv2n).
func TestHeadAuthorReadsTheHeadsSummary(t *testing.T) {
	t.Parallel()
	const head = "1111111111111111111111111111111111111111"
	other := git.CommitSummary{SHA: "2222222222222222222222222222222222222222", Author: "Seat Ivy", Email: "ivy@gastown.local"}
	seat := git.CommitSummary{SHA: head, Author: "Seat Opal", Email: "opal@gastown.local"}
	w := Work{Head: head}
	l := &Lander{}

	t.Run("the head's own summary", func(t *testing.T) {
		t.Parallel()
		got, err := l.headAuthor(summariesRepo{sums: []git.CommitSummary{other, seat}}, "base", w)
		if err != nil {
			t.Fatalf("headAuthor: %v", err)
		}
		if want := (Identity{Name: "Seat Opal", Email: "opal@gastown.local"}); got != want {
			t.Fatalf("headAuthor = %+v, want %+v", got, want)
		}
	})
	t.Run("a failed range read is an error", func(t *testing.T) {
		t.Parallel()
		if _, err := l.headAuthor(summariesRepo{err: errors.New("git log: bad revision")}, "base", w); err == nil {
			t.Fatal("headAuthor over a failed read = nil error, want one")
		}
	})
	t.Run("a range without the head is an error", func(t *testing.T) {
		t.Parallel()
		_, err := l.headAuthor(summariesRepo{sums: []git.CommitSummary{other}}, "base", w)
		if err == nil || !strings.Contains(err.Error(), "is not in") {
			t.Fatalf("headAuthor without the head = %v, want a not-in-range error", err)
		}
	})
}
