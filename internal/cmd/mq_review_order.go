package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// mqReviewOutOfOrder, when set, lets gt mq review gate an MR that a ready,
// higher-scored MR outranks; the value is the reason, printed with the review.
var mqReviewOutOfOrder string

// rankedMR is a ready merge request with its priority score.
type rankedMR struct {
	Issue *beads.Issue
	Score float64
}

// rankReadyMRs lists a rig's open merge requests that are ready for
// selection, highest priority score first: the order gt mq next uses.
func rankReadyMRs(b *beads.Beads, rigName string, now time.Time) ([]rankedMR, error) {
	issues, err := b.ListMergeRequests(beads.ListOptions{
		Label:    "gt:merge-request",
		Status:   "open",
		Priority: -1, // no priority filter
		Rig:      rigName,
	})
	if err != nil {
		return nil, fmt.Errorf("querying merge queue: %w", err)
	}
	var ranked []rankedMR
	for _, issue := range issues {
		if isMergeRequestReadyForSelection(issue) {
			ranked = append(ranked, rankedMR{Issue: issue, Score: calculateMRScore(issue, beads.ParseMRFields(issue), now)})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	return ranked, nil
}

// outOfOrderAhead returns the ready MR that should be processed before mrID,
// or nil when mrID is (or ties with) the top of the queue. queue must be
// ranked highest score first.
func outOfOrderAhead(mrID string, mrScore float64, queue []rankedMR) *rankedMR {
	if len(queue) == 0 {
		return nil
	}
	top := &queue[0]
	if top.Issue.ID == mrID || top.Score <= mrScore {
		return nil
	}
	return top
}

// mqReviewOrderRefusal checks that the MR under review is the one gt mq next
// would pick. The refinery used to gate from the list it noted at queue-scan,
// so an MR queued since, even a P0, waited behind lower-priority work
// (gt-tgey7). Returns "" when the review may proceed. A queue it cannot read
// is a refusal too: the check fails closed.
func mqReviewOrderRefusal(issue *beads.Issue, fields *beads.MRFields, rigName, beadsPath string) string {
	if mqReviewOutOfOrder != "" {
		return ""
	}
	now := time.Now()
	queue, err := rankReadyMRs(beads.New(beadsPath), rigName, now)
	if err != nil {
		return fmt.Sprintf("cannot check merge-queue priority order: %v (pass --out-of-order \"<reason>\" to review anyway)", err)
	}
	score := calculateMRScore(issue, fields, now)
	ahead := outOfOrderAhead(issue.ID, score, queue)
	if ahead == nil {
		return ""
	}
	return fmt.Sprintf("out of priority order: %s (P%d, score %.1f) is ready and outranks %s (score %.1f). "+
		"Process it first (gt mq next %s), or pass --out-of-order \"<reason>\" to review %s anyway.",
		ahead.Issue.ID, ahead.Issue.Priority, ahead.Score, issue.ID, score, rigName, issue.ID)
}
