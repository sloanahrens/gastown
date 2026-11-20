package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// The dispatch section of the Dolt panel: the open beads the
// spec-dispatch-failed label holds out of the queue, one row per registered
// rig. Before changing which label counts as a held bead, read
// internal/townhealth/townhealth.go: the health line counts the same one, so
// the panel and the line must agree on what is stuck (gt-6i64xw).

// doltDispatchTitle is the section's heading.
const doltDispatchTitle = "Beads the dispatcher failed on"

// doltDispatchShown is how many of a rig's beads its row lists before counting
// the rest as "+N more": enough to start clearing them without pushing the
// other rigs' rows off the panel.
const doltDispatchShown = 10

func init() {
	registerDoltSection("dispatch", readDoltDispatchSection)
}

// readDoltDispatchSection is the section's registered read. The per-rig lister
// is injected so the rows are testable without a database.
func readDoltDispatchSection(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
	return doltDispatchSection(ctx, doltDispatchRigIssues)
}

// doltDispatchSection reads one row per registered rig: the rig's open beads
// carrying the label, listed as "id: title". A rig whose store cannot be read
// says so in its own row and leaves the other rigs drawing; the registry itself
// failing fails the section, since it would leave no row to say anything in.
func doltDispatchSection(ctx context.Context, list func(townRoot, rig string) ([]*beads.Issue, error)) (dashboard.DoltSection, error) {
	townRoot, ok := doltTownRoot(ctx)
	if !ok {
		return dashboard.DoltSection{}, errors.New("the dispatch section got no town root")
	}
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return dashboard.DoltSection{}, err
	}
	section := dashboard.DoltSection{
		Title: doltDispatchTitle,
		Note:  "open beads labelled " + constants.LabelSpecDispatchFailed + ", listed from each rig's own bd store",
		Head:  []string{"rig", "open", "beads"},
	}
	for _, rig := range rigs {
		if rig == "" {
			// An unnamed registry entry resolves to the town's store, so its
			// row would draw the town's beads as another rig's.
			section.Rows = append(section.Rows, []string{rig, "?", "unreadable: the registry names no rig"})
			continue
		}
		issues, err := list(townRoot, rig)
		if err != nil {
			// Unknown is not zero: a rig whose store failed must not read as a
			// rig with nothing held out of the queue.
			section.Rows = append(section.Rows, []string{rig, "?", "unreadable: " + dashWhy(err)})
			continue
		}
		section.Rows = append(section.Rows, doltDispatchRow(rig, issues))
	}
	return section, nil
}

// doltDispatchRow is one rig's row: how many beads it holds, then their ids and
// titles, the first doltDispatchShown only.
func doltDispatchRow(rig string, issues []*beads.Issue) []string {
	shown := issues
	if len(shown) > doltDispatchShown {
		shown = shown[:doltDispatchShown]
	}
	listed := make([]string, 0, len(shown))
	for _, issue := range shown {
		listed = append(listed, issue.ID+": "+issue.Title)
	}
	cell := "none"
	if len(listed) > 0 {
		cell = strings.Join(listed, ", ")
	}
	if more := len(issues) - len(shown); more > 0 {
		cell += fmt.Sprintf(", +%d more", more)
	}
	return []string{rig, strconv.Itoa(len(issues)), cell}
}

// doltDispatchRigIssues lists one rig's open beads carrying the label, from that
// rig's own store: a list pinned to the town's store answers with the town's
// beads and misses the rigs'. Before folding this into one store-wide list,
// read dashboard_followups.go: it walks the stores for the same reason.
func doltDispatchRigIssues(townRoot, rig string) ([]*beads.Issue, error) {
	return beads.NewWithBeadsDir(townRoot, doltserver.FindRigBeadsDir(townRoot, rig)).
		List(beads.ListOptions{Status: string(beads.StatusOpen), Label: constants.LabelSpecDispatchFailed})
}
