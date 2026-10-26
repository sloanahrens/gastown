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
	err   error
	state string
	asked bool
}

func (f *fakeClaimsBattery) Answer(ctx context.Context, state string) (float64, error) {
	f.asked, f.state = true, state
	return f.p, f.err
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

// The flag is shown below the threshold and silent at or above it. A check
// that could not run says so on the error stream, never on the flag stream, so
// a failure is never read as a clean verdict (gt-b9j3o.11).
func TestShowClaimsFlagsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		p       float64
		err     error
		wantOut string
		wantErr bool
	}{
		{"flagged at zero", 0, nil, "claims_verified 0%", false},
		{"flagged just under the threshold", 0.49, nil, "claims_verified 49%", false},
		{"clean at the threshold", 0.5, nil, "", false},
		{"clean well above", 0.9, nil, "", false},
		{"battery cannot answer", 0, errors.New("jev unreachable"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeClaimsBattery{p: tc.p, err: tc.err}
			var out, errOut bytes.Buffer
			showClaimsFlags(context.Background(), &out, &errOut, &fakeClaimsGit{}, b, &beads.Issue{ID: "gt-1"}, "origin/main")
			if tc.wantOut == "" {
				if out.Len() != 0 {
					t.Errorf("flag stream got %q, want nothing", out.String())
				}
			} else if !strings.HasPrefix(out.String(), tc.wantOut+":") {
				t.Errorf("flag stream got %q, want a line starting %q", out.String(), tc.wantOut+":")
			}
			if got := errOut.Len() > 0; got != tc.wantErr {
				t.Errorf("error stream got %q, want output %v", errOut.String(), tc.wantErr)
			}
		})
	}
}

// A battery that cannot answer is reported and the submission carries on: the
// function returns nothing, so there is no error for the caller to act on
// (gt-b9j3o.11).
func TestClaimsBatteryFailureDoesNotStopTheSubmission(t *testing.T) {
	t.Parallel()
	b := jevClaimsBattery{newAsker: func() (claimsAsker, error) {
		return &fakeClaimsAsker{err: errors.New("jev unreachable")}, nil
	}}
	var out, errOut bytes.Buffer
	showClaimsFlags(context.Background(), &out, &errOut, &fakeClaimsGit{}, b, &beads.Issue{ID: "gt-1"}, "origin/main")
	if out.Len() != 0 {
		t.Fatalf("flag stream got %q for a failed battery, want nothing", out.String())
	}
	if !strings.Contains(errOut.String(), "not checked") || !strings.Contains(errOut.String(), "jev unreachable") {
		t.Errorf("error stream got %q, want the failure named", errOut.String())
	}
}

// A client that cannot be built is a battery that cannot answer.
func TestClaimsBatteryWithoutAClientReportsIt(t *testing.T) {
	t.Parallel()
	b := jevClaimsBattery{newAsker: func() (claimsAsker, error) { return nil, errors.New("no key") }}
	if _, err := b.Answer(context.Background(), "state"); err == nil {
		t.Fatal("Answer returned no error without a client")
	}
}

// A probability outside [0, 1] or a missing answer is not a verdict.
func TestClaimsBatteryRejectsAnAnswerThatIsNotAProbability(t *testing.T) {
	t.Parallel()
	cases := map[string]*fakeClaimsAsker{
		"above one":  {p: 1.5},
		"below zero": {p: -0.1},
	}
	for name, asker := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := jevClaimsBattery{newAsker: func() (claimsAsker, error) { return asker, nil }}
			if p, err := b.Answer(context.Background(), "state"); err == nil {
				t.Fatalf("Answer = %v with no error, want an error", p)
			}
		})
	}
	t.Run("missing answer", func(t *testing.T) {
		t.Parallel()
		b := jevClaimsBattery{newAsker: func() (claimsAsker, error) {
			return &noAnswerAsker{}, nil
		}}
		if _, err := b.Answer(context.Background(), "state"); err == nil {
			t.Fatal("Answer returned no error for an ask with no claims_verified answer")
		}
	})
}

// noAnswerAsker returns answers that hold no claims_verified probability.
type noAnswerAsker struct{}

func (noAnswerAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	return &jev.Answers{}, nil
}

// The battery reads the submission's state for this bead and branch, and asks
// only the claims_verified question (gt-b9j3o.11).
func TestJevBatteryAsksTheClaimsQuestionAboutTheState(t *testing.T) {
	t.Parallel()
	asker := &fakeClaimsAsker{p: 0.9}
	b := jevClaimsBattery{newAsker: func() (claimsAsker, error) { return asker, nil }}
	git := &fakeClaimsGit{subjects: []string{"fix the landing tick"}, numstat: "3\t1\tinternal/done/done.go\n"}
	issue := &beads.Issue{ID: "gt-1", Title: "fix the landing tick"}

	showClaimsFlags(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, git, b, issue, "origin/main")

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

// A missing issue or a failed git read is reported and the battery is not asked.
func TestShowClaimsFlagsSkipsWithoutAnIssueOrGitReads(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		git   *fakeClaimsGit
		issue *beads.Issue
	}{
		"no issue":       {&fakeClaimsGit{}, nil},
		"git read fails": {&fakeClaimsGit{err: errors.New("bad ref")}, &beads.Issue{ID: "gt-1"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &fakeClaimsBattery{p: 0.1}
			var out, errOut bytes.Buffer
			showClaimsFlags(context.Background(), &out, &errOut, tc.git, b, tc.issue, "origin/main")
			if out.Len() != 0 || b.asked {
				t.Errorf("flag stream %q, battery asked %v; want neither", out.String(), b.asked)
			}
			if !strings.Contains(errOut.String(), "not checked") {
				t.Errorf("error stream %q, want the skip named", errOut.String())
			}
		})
	}
}
