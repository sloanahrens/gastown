package specdispatch

import (
	"fmt"
	"strings"
)

// Child is one direct child of a bead, reduced to what the container rule
// reads: the id for the report, and the status the open test decides on. The
// caller reads the child set (bd's parent-child edges, both tables) and fills
// Spec.Children; nothing here queries (gt-gektq).
type Child struct {
	ID     string
	Status string
}

// Open reports whether the child is still open work. Only a terminal status
// is not: every other status — deferred and blocked included — is work the
// parent waits on.
func (c Child) Open() bool {
	switch strings.ToLower(strings.TrimSpace(c.Status)) {
	case "closed", "tombstone":
		return false
	}
	return true
}

// maxNamedChildren bounds the child ids OpenChildHold names, so one wide
// convoy does not turn a skip line into a wall.
const maxNamedChildren = 3

// OpenChildHold reports why a bead is a container of work rather than a unit
// of it: at least one child is still open, so the bead's scope already belongs
// to a child a seat will take. Empty when every child is closed, or when there
// are none: the rule keys on OPEN children, so a container whose children have
// all landed is a plain candidate again (gt-gektq).
//
// The dispatcher skips the container instead of routing it to the planner: the
// plan already exists as the children, so naming a human to re-plan it queues
// work no one needs to do, while the skip clears itself when the last child
// closes.
func OpenChildHold(s Spec) string {
	var open []string
	for _, c := range s.Children {
		if c.Open() {
			open = append(open, c.ID)
		}
	}
	if len(open) == 0 {
		return ""
	}
	named, more := open, 0
	if len(named) > maxNamedChildren {
		more = len(named) - maxNamedChildren
		named = named[:maxNamedChildren]
	}
	var reason string
	if len(open) == 1 {
		reason = "container: open child " + named[0]
	} else {
		reason = fmt.Sprintf("container: %d open children: %s", len(open), strings.Join(named, ", "))
	}
	if more > 0 {
		reason += fmt.Sprintf(" (+%d more)", more)
	}
	return reason
}
