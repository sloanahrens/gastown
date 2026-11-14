package specdispatch

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/session"
)

// LiveSeatHold reports why beadID is not the dispatcher's to sling this tick,
// and false when no live sandbox record's branch names it (gt-712i5k): a
// record outlives a seat that deferred its bead, so a second sling would put
// two seats on one bead. The id is matched as one whole token of the Branch,
// so gt-c8rbn5 names neither gt-c8rbn5x, gt-c8rbn55 nor its child
// gt-c8rbn5.18; an empty Branch and an empty id name nothing, and the lowest
// seat is named when several records name the bead.
func LiveSeatHold(beadID string, records []session.SandboxRecord) (string, bool) {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return "", false
	}
	seat := ""
	for _, rec := range records {
		if !branchNamesBead(rec.Branch, beadID) {
			continue
		}
		if name := sandboxRecordSeat(rec); seat == "" || name < seat {
			seat = name
		}
	}
	if seat == "" {
		return "", false
	}
	return fmt.Sprintf("%s still names it; the hold lifts when its sandbox record is gone", seat), true
}

// branchNamesBead reports whether branch carries beadID as one whole token: a
// maximal run of the characters a bead id is made of. The delimiters a polecat
// branch uses ("/", "+", the legacy "@") end a token, and so does any
// character an id cannot hold, so a longer id that merely starts or ends with
// beadID is another token and names another bead.
func branchNamesBead(branch, beadID string) bool {
	for i := 0; i < len(branch); {
		if !beadIDByte(branch[i]) {
			i++
			continue
		}
		j := i
		for j < len(branch) && beadIDByte(branch[j]) {
			j++
		}
		if branch[i:j] == beadID {
			return true
		}
		i = j
	}
	return false
}

// beadIDByte reports whether c can sit in a bead id: letters, digits and the
// separators a beads prefix or a child suffix uses.
func beadIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.'
}

// sandboxRecordSeat names the seat a record stands for in the assignee form
// "<rig>/polecats/<name>". The fallbacks keep a record that names the bead
// from naming no seat in the reason.
func sandboxRecordSeat(rec session.SandboxRecord) string {
	if rec.Rig != "" && rec.Polecat != "" {
		return rec.Rig + "/polecats/" + rec.Polecat
	}
	if rec.Polecat != "" {
		return rec.Polecat
	}
	if rec.Session != "" {
		return rec.Session
	}
	if seat := branchSeat(rec.Branch); seat != "" {
		return seat
	}
	return "unknown seat"
}

// branchSeat is the seat a polecat branch names: the component after
// "polecat/". Empty for a branch that is not one.
func branchSeat(branch string) string {
	rest, ok := strings.CutPrefix(branch, "polecat/")
	if !ok {
		return ""
	}
	seat, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return seat
}
