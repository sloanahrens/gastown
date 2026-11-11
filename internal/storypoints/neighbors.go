package storypoints

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/hmhints"
)

// Neighbor is a past bead hm ranks close to the one being estimated. The
// estimate's note names it; hm does not set the number.
type Neighbor struct {
	ID    string
	Rig   string
	Title string
}

// neighborKind is the hm record kind of a bead. hm also ranks docs and other
// records, which a bead's estimate does not name.
const neighborKind = "bead"

// Nearest returns up to limit past beads that hm ranks closest to the title
// and Goal of issue. It never returns issue itself. An hm failure returns no
// neighbors and the error, and the caller estimates without them.
func Nearest(ctx context.Context, run hmhints.Runner, issue *beads.Issue, limit int) ([]Neighbor, error) {
	if issue == nil {
		return nil, errors.New("storypoints: nearest needs a bead")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("storypoints: nearest limit must be positive, got %d", limit)
	}
	// hmhints.Query cuts at its limit before the kind and self filters below,
	// so ask for every hit hm returns and cut to limit after filtering.
	hits, err := hmhints.Query(ctx, run, neighborQuery(issue), math.MaxInt)
	if err != nil {
		return nil, fmt.Errorf("storypoints: nearest beads: %w", err)
	}
	var out []Neighbor
	for _, h := range hits {
		if h.Kind != neighborKind || h.ID == issue.ID {
			continue
		}
		out = append(out, Neighbor{ID: h.ID, Rig: h.Rig, Title: h.Title})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// neighborQuery is the title and the Goal section of a bead, the part that
// says what the bead changes. The title comes first, so hmhints' length cut
// takes the Goal before the title.
func neighborQuery(issue *beads.Issue) string {
	title := strings.TrimSpace(issue.Title)
	goal := goalText(issue.Description)
	if goal == "" {
		return title
	}
	return title + "\n" + goal
}

// goalText is the body of the "## Goal" section of a bead description, up to
// the next "## " heading, without the dispatcher's attachment fields. A bead
// with no such section has no goal.
func goalText(desc string) string {
	lines := strings.Split(strings.TrimSpace(beads.StripAttachmentFields(desc)), "\n")
	for i, line := range lines {
		if !strings.EqualFold(strings.TrimSpace(line), "## Goal") {
			continue
		}
		var body []string
		for _, l := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(l), "## ") {
				break
			}
			body = append(body, l)
		}
		return strings.TrimSpace(strings.Join(body, "\n"))
	}
	return ""
}
