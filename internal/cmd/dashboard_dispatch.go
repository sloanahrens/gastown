package cmd

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// dispatchTickMarker is what the spec dispatcher's tick lines carry.
const dispatchTickMarker = "spec_dispatch: tick:"

var (
	dispatchTickRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) spec_dispatch: tick: (\d+) candidate\(s\), roster (.*?), (\d+) dispatched, (\d+) refused, (\d+) planning, (\d+) skipped, (\d+) failed, (\d+) held by the failed label(.*)$`)
	skippedItemRe  = regexp.MustCompile(`^([A-Za-z0-9]+-[A-Za-z0-9.]+) \((.*)\)$`)
	skippedMoreRe  = regexp.MustCompile(` \+(\d+) more$`)
)

// parseDispatchTick reads one spec_dispatch tick line: its counts, and the
// skipped beads it names, which the dispatcher writes after the counts as
// "; skipped: gt-a (reason); gt-b (reason) +N more" (gt-gzav5). A line that is
// not a tick yields nil.
func parseDispatchTick(line string) *dashboard.Dispatch {
	m := dispatchTickRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local)
	if err != nil {
		return nil
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	d := &dashboard.Dispatch{
		At: at, Candidates: n(2), Roster: m[3], Dispatched: n(4), Refused: n(5),
		Planning: n(6), Skipped: n(7), Failed: n(8), Held: n(9),
	}
	rest := strings.TrimSpace(m[10])
	rest = strings.TrimPrefix(rest, ";")
	rest = strings.TrimSpace(rest)
	const label = "skipped:"
	if !strings.HasPrefix(rest, label) {
		return d
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, label))
	if mm := skippedMoreRe.FindStringSubmatch(rest); mm != nil {
		d.More, _ = strconv.Atoi(mm[1])
		rest = strings.TrimSuffix(rest, mm[0])
	}
	for _, item := range strings.Split(rest, "; ") {
		item = strings.TrimSpace(item)
		if sm := skippedItemRe.FindStringSubmatch(item); sm != nil {
			d.Named = append(d.Named, dashboard.SkippedBead{Bead: sm[1], Reason: sm[2]})
		} else if item != "" {
			d.Named = append(d.Named, dashboard.SkippedBead{Reason: item})
		}
	}
	return d
}

// dashPolecatHints are the read-only commands worth running on a polecat that
// needs a look. They are shown, never run: the dashboard changes nothing, and
// nuke is offered only as its dry run.
func dashPolecatHints(rig, name, bead, state string) []string {
	who := rig + "/" + name
	switch state {
	case dashboard.StateStalled, dashboard.StateUnknown:
		return []string{"gt polecat status " + who, "gt polecat check-recovery " + who}
	case dashboard.StateRecovery, dashboard.StateReviewNeeded:
		return []string{"gt polecat check-recovery " + who, "gt polecat nuke " + who + " --dry-run"}
	case dashboard.StateNeedsHuman:
		if bead != "" {
			return []string{"bd show " + bead}
		}
	}
	return nil
}
