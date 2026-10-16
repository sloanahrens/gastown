package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
)

// The review-backlog rule (gt-ee6ox): landing.risk_path items wake one by one
// only after attention.RiskPathPromoteAfter, so a pile of them wakes nobody
// until each has aged. review.backlog raises one item when the open count
// passes operational.overseer.review_backlog_threshold (default 15), or rises
// by more than review_backlog_rise (default 10) over the lowest count of the
// last hour, and says both numbers in its summary.
//
// The item is raised once per crossing: while it stands, more items change its
// summary and nothing else. It clears when the count falls under half the
// threshold, so a queue hovering at the limit does not wake on every tick.

// attentionReviewBacklogWindow is the span the rise is measured over.
const attentionReviewBacklogWindow = time.Hour

// reviewBacklogKey is the item's key; there is one backlog, not one per rig.
const reviewBacklogKey = "review:backlog"

// backlogSample is the open risk-path count one tick saw.
type backlogSample struct {
	at   time.Time
	open int
}

// backlogLimits are the rule's two knobs. A zero field reads as its default, so
// a test or a caller that never set them gets the documented behavior.
type backlogLimits struct {
	threshold int
	rise      int
}

func (l backlogLimits) thresholdV() int {
	if l.threshold > 0 {
		return l.threshold
	}
	return config.DefaultReviewBacklogThreshold
}

func (l backlogLimits) riseV() int {
	if l.rise > 0 {
		return l.rise
	}
	return config.DefaultReviewBacklogRise
}

// collectReviewBacklog raises review:backlog while the risk-path queue that
// collectRiskPaths just counted is past its threshold, has risen too fast, or
// was raised on an earlier tick and has not yet fallen under half the
// threshold. It must run after collectRiskPaths in the collector order.
//
// The count is the items the risk-path collector observed, whether or not a
// person acked them in the queue: an ack hides a row, it does not review the
// landing. When the risk-path collector failed this tick there is no count, so
// the item keeps its previous state rather than clearing on a read error; that
// failure is already logged in the collector's name.
func (s *attentionSources) collectReviewBacklog(ctx context.Context) ([]attention.Item, error) {
	if !s.riskCounted {
		return prevOfKind(s.prev, attention.KindReviewBacklog), nil
	}
	open := s.riskOpen
	threshold, riseLimit := s.backlog.thresholdV(), s.backlog.riseV()

	cutoff := s.now.Add(-attentionReviewBacklogWindow)
	lowest := open
	kept := s.backlogSamples[:0:0]
	for _, smp := range s.backlogSamples {
		if smp.at.Before(cutoff) {
			continue
		}
		kept = append(kept, smp)
		lowest = min(lowest, smp.open)
	}
	s.backlogSamples = append(kept, backlogSample{at: s.now, open: open})
	rise := open - lowest

	_, standing := attention.Find(s.prev, reviewBacklogKey)
	raise := open > threshold || rise > riseLimit
	hold := standing && open*2 >= threshold
	if !raise && !hold {
		return nil, nil
	}
	return []attention.Item{{
		Key:      reviewBacklogKey,
		Kind:     attention.KindReviewBacklog,
		Severity: attention.SeverityHigh,
		Summary: fmt.Sprintf("%d open risk-path reviews, %+d in the last hour (wakes past %d, or a rise over %d)",
			open, rise, threshold, riseLimit),
	}}, nil
}
