package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hmhints"
	"github.com/steveyegge/gastown/internal/taskmap"
)

// how_history.go is `gt how --history`: after the listing, the history-man
// records closest to the job the user looked up (gt-j669b4.7). The query goes
// through historyRunner so tests drive the section from fixed lines and no
// test starts hm. A lookup that cannot run degrades to one line and never
// fails the command: `gt how` must not hold a session.

const (
	// historyLimit is how many hits the section shows.
	historyLimit = 5

	// historyQueryTimeout bounds one hm query, so a slow hm cannot hold the
	// command past it: the section degrades to its one line instead.
	historyQueryTimeout = 20 * time.Second
)

// howHistoryFlag is --history: add the history section after the listing.
var howHistoryFlag bool

func init() {
	howCmd.Flags().BoolVar(&howHistoryFlag, "history", false, "after the listing, show the top history-man hits for the task")
}

// historyRunner answers one history lookup: at most limit lines, best hit
// first. The production runner is hmHistory.
type historyRunner interface {
	Query(ctx context.Context, text string, limit int) ([]string, error)
}

// hmHistory is the real runner: the hits hmhints decodes from `hm query`, one
// line each. A host without hm is hmhints.ErrUnavailable, which the section
// prints as its reason.
type hmHistory struct{ run hmhints.Runner }

func (h hmHistory) Query(ctx context.Context, text string, limit int) ([]string, error) {
	hits, err := hmhints.Query(ctx, h.run, text, limit)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(hits))
	for _, hit := range hits {
		lines = append(lines, historyHitLine(hit))
	}
	return lines, nil
}

// historyHitLine renders one hit in hm's printed shape: rank, score, kind,
// rig/id and title.
func historyHitLine(h hmhints.Hit) string {
	id := h.ID
	if h.Rig != "" {
		id = h.Rig + "/" + h.ID
	}
	return fmt.Sprintf("#%d %.2f %s %s — %s", h.Rank, h.Score, h.Kind, id, h.Title)
}

// writeHowHistory appends the history section for args to b. The text a lookup
// asks about is the group's description plus the user's word when the word
// names a group, and the word alone otherwise. A lookup that cannot run or
// finds nothing says so in one line, and the listing it follows still
// succeeds.
func writeHowHistory(ctx context.Context, b *strings.Builder, h historyRunner, m howMap, args []string) {
	text, ok := howHistoryText(m, args)
	if !ok {
		b.WriteString("history: not available (no word to look up)\n")
		return
	}
	lines, err := h.Query(ctx, text, historyLimit)
	if err != nil {
		fmt.Fprintf(b, "history: not available (%s)\n", oneLine(err.Error()))
		return
	}
	lines = nonBlankLines(lines)
	if len(lines) == 0 {
		b.WriteString("history: no hits\n")
		return
	}
	b.WriteString("history:\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// howHistoryText is what one lookup asks hm about: the group's description,
// which names the job, followed by the word the user typed. A word that names
// no group is the text on its own; no usable word means there is nothing to
// look up.
func howHistoryText(m howMap, args []string) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	word := args[0]
	if strings.TrimSpace(word) == "" {
		return "", false
	}
	if g := taskmap.Group(word); hasGroup(m.Groups(), g) {
		if desc := m.Describe(g); desc != "" {
			return desc + " " + word, true
		}
	}
	return word, true
}

// oneLine squashes reason's whitespace, so an error that spans lines keeps the
// section's degrade to one line.
func oneLine(reason string) string {
	return strings.Join(strings.Fields(reason), " ")
}

// nonBlankLines drops lines that hold only whitespace, so a runner's empty
// entry cannot print as a blank hit.
func nonBlankLines(lines []string) []string {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	return kept
}
