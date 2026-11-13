package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// holdStaleHeldLabel is the hold label beside the overseer-only and
// needs-overseer spellings. It is spelled "held": `bd list --label held --all`
// run from the gastown rig on 2026-11-13 returns the beads that carry it.
const holdStaleHeldLabel = "held"

// holdStaleLabels are the labels that hold a bead for the overseer, in the
// order the summary lists them. gt:needs-overseer and needs-overseer are both
// read: the readers hold each spelling beside the other (internal/dispatch/dispatch_hold.go).
var holdStaleLabels = []string{
	dispatch.OverseerOnlyLabel,
	"needs-overseer",
	dispatch.NeedsOverseerLabel,
	holdStaleHeldLabel,
}

const (
	// holdStaleDepsClosedAfter is the age past which a hold whose dependencies
	// are all closed is raised at medium severity. Age is the bead's created
	// time: the time its hold label was added is not on the issue.
	holdStaleDepsClosedAfter = 24 * time.Hour
	// holdStaleAnyDepsAfter is the age past which a hold is raised at high
	// severity whatever its dependencies, so a blocked hold cannot sit forever.
	holdStaleAnyDepsAfter = 72 * time.Hour
)

// collectHoldStale raises one item per bead that wears a hold label and has
// been held past a window, measured from the bead's created time: the time its
// label was added is not on the issue. A bead held over one day with every
// blocking dependency closed is raised medium; a bead held over three days is
// raised high whatever its dependencies. The key is the bead, so one bead
// raises one item however many ticks it stays held, and the item clears when
// the labels go or the bead closes.
//
// Dependencies are read with one bd show per candidate older than one day, so
// the reads are bounded to the holds that could raise an item. A failed label
// list fails the collection, so the previous tick's items are kept
// (collectAttention). A bead that cannot be judged (an unparseable created
// time, a failed show, or a show that returns nothing) is skipped with one
// log line naming it, and the other beads' items are still raised.
func (s *attentionSources) collectHoldStale(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	seen := map[string]bool{}
	for _, rig := range s.holdRigs() {
		for _, label := range holdStaleLabels {
			issues, err := s.holdBeads(ctx, rig, label)
			if err != nil {
				return nil, fmt.Errorf("%s: listing %s holds: %w", rig, label, err)
			}
			for _, is := range issues {
				if is == nil || seen[is.ID] || beads.IssueStatus(strings.TrimSpace(is.Status)).IsTerminal() {
					continue
				}
				seen[is.ID] = true
				it, ok, err := s.judgeHold(ctx, rig, is)
				if err != nil {
					s.d.logger.Printf("attention: collector %s: skipped %s: %v", attention.KindHoldStale, is.ID, err)
					continue
				}
				if ok {
					out = append(out, it)
				}
			}
		}
	}
	return out, nil
}

// judgeHold judges one held bead against the two rules and returns its item,
// if it raises one.
func (s *attentionSources) judgeHold(ctx context.Context, rig string, is *beads.Issue) (attention.Item, bool, error) {
	created, err := time.Parse(time.RFC3339, is.CreatedAt)
	if err != nil {
		return attention.Item{}, false, fmt.Errorf("created %q is not RFC 3339: %w", is.CreatedAt, err)
	}
	age := s.now.Sub(created)
	if age <= holdStaleDepsClosedAfter {
		return attention.Item{}, false, nil
	}

	severity := attention.SeverityMedium
	raised := "over one day, every dependency closed"
	if age > holdStaleAnyDepsAfter {
		severity = attention.SeverityHigh
		raised = "over three days, whatever its dependencies"
	} else {
		full, err := s.holdBead(ctx, rig, is.ID)
		if err != nil {
			return attention.Item{}, false, fmt.Errorf("reading dependencies: %w", err)
		}
		if full == nil {
			return attention.Item{}, false, errors.New("listed as held but not shown")
		}
		if len(beads.UnresolvedBlockerIDs(full.Dependencies)) > 0 {
			return attention.Item{}, false, nil
		}
	}

	return attention.Item{
		Key:      "hold-stale:" + is.ID,
		Kind:     attention.KindHoldStale,
		Severity: severity,
		Rig:      rig,
		Bead:     is.ID,
		Summary: fmt.Sprintf("%s held %s by created time (labels %s); raised: %s",
			is.ID, parkedAge(age), strings.Join(holdStaleLabelsOn(is.Labels), ", "), raised),
	}, true, nil
}

// holdStaleLabelsOn is the hold labels a bead carries, in holdStaleLabels order.
func holdStaleLabelsOn(labels []string) []string {
	var out []string
	for _, want := range holdStaleLabels {
		for _, have := range labels {
			if have == want {
				out = append(out, want)
				break
			}
		}
	}
	return out
}

// holdStaleItem picks the hold-stale items out of the kind's previous items, so
// a failed collector keeps only its own (collectAttention).
func holdStaleItem(it attention.Item) bool {
	return it.Kind == attention.KindHoldStale && strings.HasPrefix(it.Key, "hold-stale:")
}
