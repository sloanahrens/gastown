package done

import (
	"context"
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

// claimsBatteryTimeout bounds one battery call, so a slow host cannot hold the
// submission (gt-b9j3o.11).
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

// claimsBatteryClient builds the Jev client. Tests replace it so no model runs.
var claimsBatteryClient = func() (claimsAsker, error) {
	return jev.NewClient()
}

// claimsBatteryAnswer asks the claims_verified question about the state and
// returns the probability that the MR notes describe a verification that was
// run. Any failure reports ok=false: a battery that cannot answer says nothing.
var claimsBatteryAnswer = func(ctx context.Context, state string) (float64, bool) {
	c, err := claimsBatteryClient()
	if err != nil {
		return 0, false
	}
	qs := map[string]jev.Question{
		submission.QClaimsVerified: submission.Questions()[submission.QClaimsVerified],
	}
	ctx, cancel := context.WithTimeout(ctx, claimsBatteryTimeout)
	defer cancel()
	ans, err := c.Ask(ctx, state, qs)
	if err != nil || ans == nil {
		return 0, false
	}
	p, ok := ans.Noul[submission.QClaimsVerified]
	return p, ok
}

// showClaimsFlags shows the polecat the claims_verified flag for the branch
// when the battery answers below claimsFlagBelow. It prints nothing otherwise
// and returns nothing, so no battery outcome can stop the submission.
func showClaimsFlags(ctx context.Context, w io.Writer, g claimsGit, issue *beads.Issue, baseRef string) {
	if issue == nil {
		return
	}
	subjects, err := g.CommitSubjects(baseRef, "HEAD")
	if err != nil {
		return
	}
	numstat, err := g.DiffNumstat(baseRef, "HEAD")
	if err != nil {
		return
	}
	p, ok := claimsBatteryAnswer(ctx, submission.ClaimsState(issue, subjects, numstat))
	if !ok || p >= claimsFlagBelow {
		return
	}
	fmt.Fprintf(w, "claims_verified %.0f%%: the MR notes may not name a check that was actually run, with its result. Check them before the landing; this does not block the submission.\n", p*100)
}
