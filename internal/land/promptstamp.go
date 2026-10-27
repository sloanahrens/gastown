package land

import (
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// promptStampRE matches the whole comment a sandbox spawn posts on the work
// bead naming the prompt it stored: "prompt <sha256, at least 12 hex digits>
// (<n> bytes)" (gt-2lu00). Anchored, so a rejection note quoting a stamp names
// no prompt.
var promptStampRE = regexp.MustCompile(`^prompt ([0-9a-f]{12,64}) \([0-9]+ bytes\)$`)

// promptStampAuthor is the author a spawn stamps its prompt under: the polecat
// seat the branch names. A crew branch has no stamp, because the prompt store
// is a polecat spawn's (gt-js6di).
func promptStampAuthor(w Work) string {
	seat := AuthorSeat(w.Branch)
	if seat == "" || !strings.HasPrefix(w.Branch, PolecatBranchPrefix) {
		return ""
	}
	return w.Rig + "/polecats/" + seat
}

// promptStampSHA returns the prompt hash the author seat last stamped on the
// work bead, "" when the bead carries none from that seat. The last stamp is
// the one the newest attempt's prompt was stored under, since a rework respawn
// stamps again; a stamp another author wrote names that author's prompt, not
// this work's, so it is not one (gt-b9j3o.16).
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
		if m := promptStampRE.FindStringSubmatch(strings.TrimSpace(c.Text)); m != nil {
			sha = m[1]
		}
	}
	return sha
}
