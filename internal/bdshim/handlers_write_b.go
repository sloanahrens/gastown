package bdshim

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The second write handlers: delete, defer, undefer and unclaim for a tracker
// rig (gt-p85pw.97). Each answers the flags its recorded goldens cover and
// refuses any other flag by name, so a caller never gets a wider write than it
// asked for. The store is opened through the backend seam, from the route the
// router chose.
//
// delete is bd's own three steps (bd delete --help): mark the text references
// to the batch in the surviving directly linked issues as "[deleted:<id>]", let
// the service remove the dependency links, and delete the batch all-or-nothing.
// The counts the answer prints are the ones the reads found and the writes made,
// so the line is never a claim the handler did not carry out.

// The flag set each write verb answers: the flags its golden rows cover.
var (
	deleteFlags  = readFlagSpec{"--force": {}, "-f": {}}
	deferFlags   = readFlagSpec{"--reason": {}}
	undeferFlags = readFlagSpec{}
	unclaimFlags = readFlagSpec{}
)

// The statuses these handlers move an issue between.
const (
	deferredStatus = "deferred"
	openStatus     = "open"
)

// deleteStore is the part of a rig's store the delete handler uses. The
// handlers open the full backend.Store; the narrow interface lets a test drive
// each refusal without a tracker server.
type deleteStore interface {
	Show(id string) (*beads.Issue, error)
	ShowMultiple(ids []string) (map[string]*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	DeleteIssues(ids ...string) error
}

// claimStore is the part of a rig's store the defer, undefer and unclaim
// handlers use, narrowed for the same reason.
type claimStore interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	Release(id string) error
}

// deleteHandler answers bd delete --force for a tracker rig.
func deleteHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("delete", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, deleteFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	// A delete without --force is bd's preview, and the shim does not print one:
	// answering with a real delete would be the write the caller did not ask for.
	if !fl.has("--force") && !fl.has("-f") {
		return shimError(env, fmt.Errorf("delete without --force is not implemented for tracker rig %s: the shim does not preview a delete", d.Route.Prefix))
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("delete needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return deleteIssues(env, st, fl.operands)
}

// deleteIssues deletes a batch the way bd does: every id is read first, so a
// batch naming an issue the store does not hold writes nothing; the text
// references to the batch in the surviving directly linked issues are planned;
// then the batch is deleted all-or-nothing, which is the service's rule, and
// only a delete that ran writes the marks. A refused delete therefore leaves
// every surviving issue exactly as it was, never marked for an id that is still
// there. The answer prints the link count the reads found and the issues the
// marking changed, so both lines report work this call did.
func deleteIssues(env Env, st deleteStore, ids []string) int {
	linked, links, err := linkedIssues(st, ids)
	if err != nil {
		return shimError(env, err)
	}
	marks, err := plannedMarks(st, ids, linked)
	if err != nil {
		return shimError(env, err)
	}
	if err := st.DeleteIssues(ids...); err != nil {
		return shimError(env, fmt.Errorf("delete %s: %w", strings.Join(ids, " "), err))
	}
	for _, id := range ids {
		if _, err := fmt.Fprintf(env.Stdout, "✓ Deleted %s\n", id); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
	}
	if _, err := fmt.Fprintf(env.Stdout, "  Removed %d dependency link(s)\n", links); err != nil {
		return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
	}
	updated, err := writeMarks(st, ids, marks)
	if err != nil {
		return shimError(env, err)
	}
	// bd says how many issues it marked, or that there was nothing to mark.
	if updated == 0 {
		if _, err := fmt.Fprintln(env.Stdout, "  (none have text references)"); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
		return 0
	}
	if _, err := fmt.Fprintf(env.Stdout, "  Updated text references in %d issue(s)\n", updated); err != nil {
		return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
	}
	return 0
}

// linkedIssues reads every id in the batch and returns the surviving issues a
// dependency link joins to it, sorted, plus the number of distinct links the
// delete removes. An id the store does not hold fails the read: the batch is
// all-or-nothing, so a batch that cannot run in full writes nothing.
func linkedIssues(st deleteStore, ids []string) ([]string, int, error) {
	deleted := make(map[string]bool, len(ids))
	for _, id := range ids {
		deleted[id] = true
	}
	linked := map[string]bool{}
	links := map[string]bool{}
	for _, id := range ids {
		is, err := st.Show(id)
		if err != nil {
			return nil, 0, fmt.Errorf("delete %s: %w", id, err)
		}
		for _, dep := range is.Dependencies {
			links[id+"\x00"+dep.ID] = true
			if !deleted[dep.ID] {
				linked[dep.ID] = true
			}
		}
		for _, dep := range is.Dependents {
			links[dep.ID+"\x00"+id] = true
			if !deleted[dep.ID] {
				linked[dep.ID] = true
			}
		}
	}
	out := make([]string, 0, len(linked))
	for id := range linked {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, len(links), nil
}

// deletedMark is what bd leaves in place of a deleted id in a text field
// (bd delete --help writes it as "[deleted:ID]").
func deletedMark(id string) string { return "[deleted:" + id + "]" }

// idByte reports whether c is one of the bytes beads.IsBeadIDToken accepts, so
// a byte that is not one ends a reference. '-' and '.' are id bytes, so "fx-1"
// in "fx-1.2" or "fx-1-x" is part of a longer id and is not the reference.
func idByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return c == '-' || c == '.' || c == '_'
}

// markDeletedReference replaces each whole-id occurrence of id in text with
// bd's mark. A boundary is a byte that cannot continue a bead id, so an id
// that is only part of a longer one stays as it is, wherever the text sits.
func markDeletedReference(text, id string) string {
	if text == "" || id == "" || !strings.Contains(text, id) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	rest := text
	for {
		i := strings.Index(rest, id)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := i + len(id)
		if (i == 0 || !idByte(rest[i-1])) && (end == len(rest) || !idByte(rest[end])) {
			b.WriteString(rest[:i])
			b.WriteString(deletedMark(id))
			rest = rest[end:]
			continue
		}
		// This occurrence is inside a longer id: keep the byte it starts on and
		// look again from the next one.
		b.WriteString(rest[:i+1])
		rest = rest[i+1:]
	}
}

// textMark is one surviving issue's marked text, planned from a read and
// written only after the batch delete has run.
type textMark struct {
	id   string
	opts beads.UpdateOptions
}

// plannedMarks computes the marks for the surviving issues a link joins to the
// batch and writes nothing: the batch delete runs first, so a refused delete
// leaves every issue as it was. The issues are read in one call, so a store
// that cannot answer leaves every issue as it was too.
func plannedMarks(st deleteStore, ids, linked []string) ([]textMark, error) {
	if len(linked) == 0 {
		return nil, nil
	}
	issues, err := st.ShowMultiple(linked)
	if err != nil {
		return nil, fmt.Errorf("reading the issues linked to %s: %w", strings.Join(ids, " "), err)
	}
	var marks []textMark
	for _, id := range linked {
		is, ok := issues[id]
		if !ok {
			// Gone between the link read and this read: there is no text left
			// to mark.
			continue
		}
		title, description, acceptance := is.Title, is.Description, is.AcceptanceCriteria
		for _, gone := range ids {
			title = markDeletedReference(title, gone)
			description = markDeletedReference(description, gone)
			acceptance = markDeletedReference(acceptance, gone)
		}
		opts := beads.UpdateOptions{}
		changed := false
		if title != is.Title {
			opts.Title = &title
			changed = true
		}
		if description != is.Description {
			opts.Description = &description
			changed = true
		}
		if acceptance != is.AcceptanceCriteria {
			opts.Acceptance = &acceptance
			changed = true
		}
		if changed {
			marks = append(marks, textMark{id: id, opts: opts})
		}
	}
	return marks, nil
}

// writeMarks writes the planned marks and returns how many issues it changed.
func writeMarks(st deleteStore, ids []string, marks []textMark) (int, error) {
	for _, mark := range marks {
		if err := st.Update(mark.id, mark.opts); err != nil {
			return 0, fmt.Errorf("marking the references to %s in %s: %w", strings.Join(ids, " "), mark.id, err)
		}
	}
	return len(marks), nil
}

// deferHandler answers bd defer for a tracker rig.
func deferHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("defer", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, deferFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("defer needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return deferIssues(env, st, fl.operands, fl.last("--reason"))
}

// deferIssues defers each id and appends the reason to its notes, bd's own
// --reason ("appended to notes"). An id the store refuses is reported and the
// rest of the batch still runs; any refusal exits 1.
func deferIssues(env Env, st claimStore, ids []string, reason string) int {
	failures := 0
	for _, id := range ids {
		status := deferredStatus
		if err := st.Update(id, beads.UpdateOptions{Status: &status, AppendNotes: reason}); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: defer %s: %v\n", id, err)
			continue
		}
		if _, err := fmt.Fprintf(env.Stdout, "* Deferred %s\n", id); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
	}
	if failures == 0 {
		return 0
	}
	return bdFailureExit
}

// undeferHandler answers bd undefer for a tracker rig.
func undeferHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("undefer", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, undeferFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("undefer needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return undeferIssues(env, st, fl.operands, clockOf(env))
}

// clockOf is the wall clock a handler dates its decision with: Env.Now when a
// test injects one, and the process's clock otherwise. undefer compares a
// bead's deferral to it, so a test fixes which side of the deferral it runs on
// rather than depending on the day it runs.
func clockOf(env Env) time.Time {
	if env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

// undeferIssues returns each deferred id to open. An id that is not deferred is
// bd's own refusal (bd exits 1 and prints nothing for it), and so is a bead the
// store has snoozed into the future: the seam's update cannot clear the
// deferral, so reopening it would read open and still stay off the ready board
// until its date.
func undeferIssues(env Env, st claimStore, ids []string, now time.Time) int {
	failures := 0
	for _, id := range ids {
		is, err := st.Show(id)
		if err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: undefer %s: %v\n", id, err)
			continue
		}
		if is.Status != deferredStatus {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: undefer %s: %s is not deferred (status: %s)\n", id, id, is.Status)
			continue
		}
		if err := snoozeBlocksReopen(is, now); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: undefer %s: %v\n", id, err)
			continue
		}
		status := openStatus
		if err := st.Update(id, beads.UpdateOptions{Status: &status}); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: undefer %s: %v\n", id, err)
			continue
		}
		if _, err := fmt.Fprintf(env.Stdout, "* Undeferred %s (now open)\n", id); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
	}
	if failures == 0 {
		return 0
	}
	return bdFailureExit
}

// snoozeBlocksReopen refuses the undefer of a bead whose deferral is still in
// the future, and of one whose deferral it cannot read: the seam's update
// cannot clear defer_until, and clearing the status alone would leave the bead
// off the ready board with no way for the caller to see why.
func snoozeBlocksReopen(is *beads.Issue, now time.Time) error {
	if is.DeferUntil == "" {
		return nil
	}
	until, err := time.Parse(time.RFC3339, is.DeferUntil)
	if err != nil {
		return fmt.Errorf("%s carries a deferral this shim cannot read (%q), and it cannot clear one", is.ID, is.DeferUntil)
	}
	if until.After(now) {
		return fmt.Errorf("%s is snoozed until %s, and the shim cannot clear a deferral: reopening it would leave it off the ready board", is.ID, is.DeferUntil)
	}
	return nil
}

// unclaimHandler answers bd unclaim for a tracker rig.
func unclaimHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("unclaim", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, unclaimFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("unclaim needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return unclaimIssues(env, st, fl.operands)
}

// unclaimIssues releases each id the caller holds. An unassigned id is bd's
// own refusal (bd exits 1 and prints nothing for it); a claim another actor
// holds is the store's guard, which the shim does not force past (--force is
// not a flag these goldens cover). Any refusal exits 1 and the rest still run.
func unclaimIssues(env Env, st claimStore, ids []string) int {
	failures := 0
	for _, id := range ids {
		is, err := st.Show(id)
		if err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: unclaim %s: %v\n", id, err)
			continue
		}
		if is.Assignee == "" {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: unclaim %s: issue %s is not assigned\n", id, id)
			continue
		}
		if err := st.Release(id); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: unclaim %s: %v\n", id, err)
			continue
		}
		if _, err := fmt.Fprintf(env.Stdout, "* Unclaimed %s\n", id); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
	}
	if failures == 0 {
		return 0
	}
	return bdFailureExit
}
