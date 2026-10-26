package done

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// fakeClaimsGit is the git reads the flag makes, with fixed answers.
type fakeClaimsGit struct {
	subjects []string
	numstat  string
	err      error
}

func (f *fakeClaimsGit) CommitSubjects(base, head string) ([]string, error) {
	return f.subjects, f.err
}

func (f *fakeClaimsGit) DiffNumstat(base, head string) (string, error) {
	return f.numstat, f.err
}

// fakeClaimsBattery answers with one fixed probability, and records the state
// it was asked about.
type fakeClaimsBattery struct {
	p     float64
	ok    bool
	state string
	asked bool
}

func (f *fakeClaimsBattery) Answer(ctx context.Context, state string) (float64, bool) {
	f.asked, f.state = true, state
	return f.p, f.ok
}

// fakeClaimsAsker answers the claims question with one probability, or fails.
type fakeClaimsAsker struct {
	p     float64
	err   error
	state any
	qs    map[string]jev.Question
}

func (f *fakeClaimsAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	f.state, f.qs = state, qs
	if f.err != nil {
		return nil, f.err
	}
	return &jev.Answers{Noul: map[string]float64{submission.QClaimsVerified: f.p}}, nil
}

// The flag is shown below the threshold and silent at or above it, over the
// flagged and unflagged answers (gt-b9j3o.11).
func TestShowClaimsFlagsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		p       float64
		ok      bool
		wantOut string
	}{
		{"flagged at zero", 0, true, "claims_verified 0%"},
		{"flagged just under the threshold", 0.49, true, "claims_verified 49%"},
		{"unflagged at the threshold", 0.5, true, ""},
		{"unflagged well above", 0.9, true, ""},
		{"battery cannot answer", 0.1, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeClaimsBattery{p: tc.p, ok: tc.ok}
			var out bytes.Buffer
			showClaimsFlags(context.Background(), &out, &fakeClaimsGit{}, b, &beads.Issue{ID: "gt-1"}, "origin/main")
			if tc.wantOut == "" {
				if out.Len() != 0 {
					t.Fatalf("printed %q, want nothing", out.String())
				}
				return
			}
			if !strings.HasPrefix(out.String(), tc.wantOut+":") {
				t.Fatalf("printed %q, want a line starting %q", out.String(), tc.wantOut+":")
			}
		})
	}
}

// A battery that cannot answer prints nothing and returns nothing, so the
// submission carries on (gt-b9j3o.11).
func TestClaimsBatteryFailureDoesNotStopTheSubmission(t *testing.T) {
	t.Parallel()
	b := jevClaimsBattery{newAsker: func() (claimsAsker, error) {
		return &fakeClaimsAsker{err: errors.New("jev unreachable")}, nil
	}}
	var out bytes.Buffer
	showClaimsFlags(context.Background(), &out, &fakeClaimsGit{}, b, &beads.Issue{ID: "gt-1"}, "origin/main")
	if out.Len() != 0 {
		t.Fatalf("printed %q for a failed battery, want nothing", out.String())
	}
}

// A client that cannot be built is also a battery that cannot answer.
func TestClaimsBatteryWithoutAClientSaysNothing(t *testing.T) {
	t.Parallel()
	b := jevClaimsBattery{newAsker: func() (claimsAsker, error) { return nil, errors.New("no key") }}
	if p, ok := b.Answer(context.Background(), "state"); ok {
		t.Fatalf("answer = %v, want no answer", p)
	}
}

// The battery reads the submission's state for this bead and branch, and asks
// only the claims_verified question (gt-b9j3o.11).
func TestShowClaimsFlagsAsksTheClaimsQuestionAboutTheBranch(t *testing.T) {
	t.Parallel()
	asker := &fakeClaimsAsker{p: 0.9}
	b := jevClaimsBattery{newAsker: func() (claimsAsker, error) { return asker, nil }}
	git := &fakeClaimsGit{subjects: []string{"fix the landing tick"}, numstat: "3\t1\tinternal/done/done.go\n"}
	issue := &beads.Issue{ID: "gt-1", Title: "fix the landing tick"}

	showClaimsFlags(context.Background(), &bytes.Buffer{}, git, b, issue, "origin/main")

	state, ok := asker.state.(string)
	if !ok {
		t.Fatalf("state = %T, want the submission's string state", asker.state)
	}
	if !strings.Contains(state, "fix the landing tick") || !strings.Contains(state, "done.go") {
		t.Errorf("state %q does not carry the bead title and the changed file", state)
	}
	if len(asker.qs) != 1 || asker.qs[submission.QClaimsVerified].Type != "noul" {
		t.Errorf("questions = %v, want only the claims_verified noul", asker.qs)
	}
}

// A missing issue or a failed git read says nothing and asks nothing.
func TestShowClaimsFlagsSkipsWithoutAnIssueOrGitReads(t *testing.T) {
	t.Parallel()
	b := &fakeClaimsBattery{p: 0.1, ok: true}
	var out bytes.Buffer

	showClaimsFlags(context.Background(), &out, &fakeClaimsGit{}, b, nil, "origin/main")
	showClaimsFlags(context.Background(), &out, &fakeClaimsGit{err: errors.New("bad ref")}, b, &beads.Issue{ID: "gt-1"}, "origin/main")

	if out.Len() != 0 || b.asked {
		t.Fatalf("printed %q and asked the battery %v, want neither", out.String(), b.asked)
	}
}
