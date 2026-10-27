package land

import (
	"strings"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/promptstamp"
)

// promptStampAuthor is the author a spawn stamps its prompt under: the polecat
// seat the branch names (promptstamp.Author). A crew branch has no stamp,
// because the prompt store is a polecat spawn's (gt-js6di).
func promptStampAuthor(w Work) string {
	seat := authorkind.Seat(w.Branch)
	if seat == "" || !strings.HasPrefix(w.Branch, PolecatBranchPrefix) {
		return ""
	}
	return promptstamp.Author(w.Rig, seat)
}

// promptStampSHA returns the prompt hash the author seat last stamped on the
// work bead, "" when the bead carries none from that seat. A bead's comments
// arrive oldest first, so the last stamp is the newest attempt's: a rework
// respawn stamps again, and that prompt is the one the review judged. A stamp
// another author wrote names that author's prompt, not this work's, so it is
// not one (gt-b9j3o.16).
func promptStampSHA(comments []beads.Comment, w Work) string {
	author := promptStampAuthor(w)
	if author == "" {
		return ""
	}
	sha := ""
	for _, c := range comments {
		if strings.TrimSpace(c.Author) != author {
			continue
		}
		if got, ok := promptstamp.Parse(c.Text); ok {
			sha = got
		}
	}
	return sha
}
