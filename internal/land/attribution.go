package land

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// RangeCheck inspects the commits a landing would bring onto the target
// (base..head in g) and returns a Rejection when they must not land. An error
// means the check could not run; it is an infrastructure failure, never a
// verdict on the work.
type RangeCheck func(g *git.Git, base, head string) (*Rejection, error)

// Attribution is refused only in its real forms: a Co-Authored-By or
// Signed-off-by trailer naming claude or anthropic, a "Generated with/by ...
// Claude" line, or the robot-emoji badge. A bare product mention such as
// "Claude Code hooks" in a subject is allowed: two such mentions blocked
// landings on 2026-09-30.
var (
	attributionTrailerRE   = regexp.MustCompile(`(?i)^\s*(co-authored-by|signed-off-by)\s*:.*\b(claude|anthropic)\b`)
	attributionGeneratedRE = regexp.MustCompile(`(?i)^\s*(?:🤖\s*)?generated\s+(?:with|by)\b.*\b(?:claude|anthropic)\b`)
)

// AttributionLine returns the first line of msg that is AI attribution, or
// "" when there is none.
func AttributionLine(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		if attributionTrailerRE.MatchString(line) || attributionGeneratedRE.MatchString(line) || strings.Contains(line, "🤖") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// AttributionCheck refuses a range carrying AI attribution. The author can
// fix it (reword the commit), so it is rework, not human work.
func AttributionCheck(g *git.Git, base, head string) (*Rejection, error) {
	msgs, err := g.CommitMessages(base, head)
	if err != nil {
		return nil, fmt.Errorf("reading commit messages %s..%s: %w", shortSHA(base), shortSHA(head), err)
	}
	for _, m := range msgs {
		if line := AttributionLine(m.Message); line != "" {
			return &Rejection{Kind: RejectPolicy, Rework: true,
				Reason: fmt.Sprintf("commit %s carries AI attribution (%q); reword it without the attribution, rebase onto the target and re-gate", shortSHA(m.SHA), NoteField(line))}, nil
		}
	}
	return nil, nil
}
