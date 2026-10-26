package done

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// claimsFlagBelow is the probability under which gt done shows the polecat the
// claims_verified flag (gt-b9j3o.11). The flag is shown; it never blocks.
const claimsFlagBelow = 0.5

// claimsBatteryTimeout bounds the wait for one battery answer (gt-b9j3o.11).
const claimsBatteryTimeout = 30 * time.Second

// claimsGit is the git reads the flag needs. The real repository satisfies it;
// a repository without them gets no flag, and no flag never blocks.
type claimsGit interface {
	CommitSubjects(base, head string) ([]string, error)
	DiffNumstat(base, head string) (string, error)
}

// claimsAsker is the one Ask call the battery makes; *jev.Client satisfies it.
type claimsAsker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// claimsBattery answers the claims_verified question about a state, and returns
// the probability that the MR notes describe a verification that was run. A
// non-nil error means the battery could not answer, which is not a verdict.
type claimsBattery interface {
	Answer(ctx context.Context, state string) (float64, error)
}

// jevClaimsBattery asks Jev through a client built by newAsker. The submit
// path builds it with newJevClaimsAsker; a unit test passes a fake.
type jevClaimsBattery struct {
	newAsker func() (claimsAsker, error)
}

// newJevClaimsAsker builds the hosted Jev client from its key files.
func newJevClaimsAsker() (claimsAsker, error) {
	return jev.NewClient()
}

// Answer asks the claims question under claimsBatteryTimeout. The timeout
// bounds the wait for an answer, so a slow host costs at most that long before
// the submission goes on.
func (b jevClaimsBattery) Answer(ctx context.Context, state string) (float64, error) {
	c, err := b.newAsker()
	if err != nil {
		return 0, fmt.Errorf("jev client: %w", err)
	}
	qs := map[string]jev.Question{
		submission.QClaimsVerified: submission.Questions()[submission.QClaimsVerified],
	}
	ctx, cancel := context.WithTimeout(ctx, claimsBatteryTimeout)
	defer cancel()
	ans, err := c.Ask(ctx, state, qs)
	if err != nil {
		return 0, fmt.Errorf("jev ask: %w", err)
	}
	if ans == nil {
		return 0, errors.New("jev ask: no answers")
	}
	p, ok := ans.Noul[submission.QClaimsVerified]
	if !ok {
		return 0, errors.New("jev ask: no claims_verified answer")
	}
	if p < 0 || p > 1 {
		return 0, fmt.Errorf("jev ask: claims_verified probability %v is outside [0, 1]", p)
	}
	return p, nil
}

// showClaimsFlags shows the polecat the claims_verified flag for the branch
// when the battery answers below claimsFlagBelow. A clean verdict prints
// nothing. A check that could not run says so on errOut, so a failure is never
// mistaken for a clean verdict. Nothing here returns an error: no outcome can
// stop the submission (gt-b9j3o.11).
func showClaimsFlags(ctx context.Context, out, errOut io.Writer, g claimsGit, b claimsBattery, issue *beads.Issue, baseRef string) {
	notChecked := func(why error) {
		fmt.Fprintf(errOut, "claims_verified: not checked (%v); the submission continues\n", why)
	}
	if issue == nil {
		notChecked(errors.New("no source bead"))
		return
	}
	subjects, err := g.CommitSubjects(baseRef, "HEAD")
	if err != nil {
		notChecked(fmt.Errorf("commit subjects: %w", err))
		return
	}
	numstat, err := g.DiffNumstat(baseRef, "HEAD")
	if err != nil {
		notChecked(fmt.Errorf("diff numstat: %w", err))
		return
	}
	p, err := b.Answer(ctx, submission.ClaimsState(issue, subjects, numstat))
	if err != nil {
		notChecked(err)
		return
	}
	if p >= claimsFlagBelow {
		return
	}
	fmt.Fprintf(out, "claims_verified %.0f%%: the MR notes may not name a check that was actually run, with its result. Check them before the landing; this does not block the submission.\n", p*100)
}
