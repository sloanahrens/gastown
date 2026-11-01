package bdshim

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

// The first write handlers: create, update, close and reopen for a tracker rig
// (gt-p85pw.96). Each answers the flags its recorded goldens cover and refuses
// any other flag by name, so a caller never reads a wider write than it asked
// for. The store is opened through the backend seam, from the route the router
// chose, so this package holds no second tracker client.
//
// The exit codes are bd's, not the shim's: a guard that did not hold exits 13
// and every other refusal exits 1 (bd update --help), and a close that closed
// some of its ids but not all exits 0 (plan section 11, X4). A caller that
// scripts around bd reads the same code from the shim.

// The flag set each write verb answers: the flags its golden rows cover. A
// flag outside the set is refused by name, never ignored, so a write the shim
// cannot make in full never happens in part.
var (
	createFlags = readFlagSpec{"--json": {}, "--title": {}, "--type": {}, "--priority": {}}
	updateFlags = readFlagSpec{"--status": {}}
	closeFlags  = readFlagSpec{"--reason": {}}
	reopenFlags = readFlagSpec{}
)

// ownerEmailEnv is the environment name bd stamps a new bead's owner from, and
// a seat is spawned with it (config.GitIdentityEnv), so a flipped rig reports
// the same owner bd would.
const ownerEmailEnv = "GIT_AUTHOR_EMAIL"

// defaultCreatePriority is bd's priority for a create that names none.
const defaultCreatePriority = 2

// closeReasonDefault is what bd records, and reports, for a close that names no
// reason.
const closeReasonDefault = "Closed"

// bd's exit codes for a failed write, from `bd update --help`: 13 when every
// failure is an --if-assignee or --if-status guard that did not hold, so
// nothing was written and retrying the same guard is pointless; 1 otherwise.
const (
	bdGuardExit   = 13
	bdFailureExit = 1
)

// writeStore is the part of a rig's store the write handlers use. The handlers
// open the full backend.Store; the narrow interface lets a test drive each
// refusal without a tracker server.
type writeStore interface {
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	CloseWithReason(reason string, ids ...string) error
}

// isGuardFailure reports whether err is the tracker's guard_failed: an
// if_assignee or if_status guard did not hold and nothing was written
// (tracker/api/errors.go), which bd answers with its guard exit code.
func isGuardFailure(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.Kind == api.KindGuardFailed
}

// bdCreateAnswer is bd's answer to create --json: the new bead, then the schema
// version.
type bdCreateAnswer struct {
	Data          bdCreateIssue `json:"data"`
	SchemaVersion int           `json:"schema_version"`
}

// bdCreateIssue is one created bead as bd's create prints it: the creation
// fields only, in bd's member order, with the members a golden omits left out.
type bdCreateIssue struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	Type      string `json:"issue_type"`
	Owner     string `json:"owner,omitempty"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
	UpdatedAt string `json:"updated_at"`
}

// bdCreateRow renders one created bead as bd's create prints it. owner is the
// identity bd stamps on a new bead, which the tracker does not store: the shim
// reads the same source bd does (ownerEmailEnv), and a seat without it answers
// without the member rather than with an owner the store never had.
func bdCreateRow(is *beads.Issue, owner string) bdCreateIssue {
	return bdCreateIssue{
		ID: is.ID, Title: is.Title, Status: is.Status, Priority: is.Priority, Type: is.Type,
		Owner: owner, CreatedAt: is.CreatedAt, CreatedBy: is.CreatedBy, UpdatedAt: is.UpdatedAt,
	}
}

// createHandler answers bd create --json for a tracker rig.
func createHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("create", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, createFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("create without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("create takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	title := fl.last("--title")
	if strings.TrimSpace(title) == "" {
		return shimError(env, errors.New("create needs --title"))
	}
	// The seam's create carries the type as bd's deprecated gt:<type> label,
	// not as the issue's type, so only the store's own default can be answered
	// exactly; any other type is refused rather than filed as a task.
	if typ := fl.last("--type"); typ != "" && typ != "task" {
		return shimError(env, fmt.Errorf("create --type %s is not implemented for tracker rig %s: the tracker files every created issue as task", typ, d.Route.Prefix))
	}
	priority, err := readPriority(fl)
	if err != nil {
		return shimError(env, err)
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	is, err := st.Create(beads.CreateOptions{Title: title, Priority: priority})
	if err != nil {
		return shimError(env, fmt.Errorf("create %q: %w", title, err))
	}
	answer := bdCreateAnswer{Data: bdCreateRow(is, env.Getenv(ownerEmailEnv)), SchemaVersion: bdSchemaVersion}
	if err := printBDAnswer(env.Stdout, answer); err != nil {
		return shimError(env, err)
	}
	return 0
}

// readPriority reads --priority, which bd spells as 0-4 or P0-P4. A value
// outside that range is refused rather than clamped: a bead filed at the wrong
// priority is work nobody sees.
func readPriority(fl parsedRead) (int, error) {
	if !fl.has("--priority") {
		return defaultCreatePriority, nil
	}
	text := fl.last("--priority")
	trimmed := strings.TrimPrefix(strings.TrimPrefix(text, "P"), "p")
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 || n > 4 {
		return 0, fmt.Errorf("--priority %q is not a priority from 0 to 4", text)
	}
	return n, nil
}

// updateHandler answers bd update --status for a tracker rig.
func updateHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("update", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, updateFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("update needs an issue id"))
	}
	status := fl.last("--status")
	if status == "" {
		return shimError(env, fmt.Errorf("update without --status is not implemented for tracker rig %s", d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return updateIssues(env, st, fl.operands, status)
}

// updateIssues sets status on ids one at a time, as bd does: a refused id is
// reported and the rest still run. bd exits 13 only when every failure was a
// guard that did not hold, and 1 when any failure was another kind.
func updateIssues(env Env, st writeStore, ids []string, status string) int {
	failures, guards := 0, 0
	for _, id := range ids {
		// bd's line names the bead, so the title is read before the write: a
		// row the shim cannot name is a row it would have to guess at.
		is, err := st.Show(id)
		if err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: update %s: %v\n", id, err)
			continue
		}
		if err := st.Update(id, beads.UpdateOptions{Status: &status}); err != nil {
			failures++
			if isGuardFailure(err) {
				guards++
			}
			fmt.Fprintf(env.Stderr, "bdshim: update %s: %v\n", id, err)
			continue
		}
		if _, err := fmt.Fprintf(env.Stdout, "✓ Updated issue: %s — %s\n", id, is.Title); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
	}
	switch {
	case failures == 0:
		return 0
	case guards == failures:
		return bdGuardExit
	}
	return bdFailureExit
}

// closeHandler answers bd close for a tracker rig.
func closeHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("close", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, closeFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("close needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return closeIssues(env, st, fl.operands, fl.last("--reason"))
}

// closeIssues closes ids the way bd does: every id the store accepts is
// reported, every id it refuses is reported and the rest still close, and a
// batch that closed some of its ids but not all exits 0.
func closeIssues(env Env, st writeStore, ids []string, reason string) int {
	if reason == "" {
		reason = closeReasonDefault
	}
	err := st.CloseWithReason(reason, ids...)
	var partial *beads.PartialCloseError
	switch {
	case err == nil:
		for _, id := range ids {
			if code := closeLine(env, id, reason); code != 0 {
				return code
			}
		}
		return 0
	case errors.As(err, &partial):
		for _, id := range partial.Closed {
			if code := closeLine(env, id, reason); code != 0 {
				return code
			}
		}
		refusal := partial.Err
		if refusal == nil {
			refusal = beads.ErrCloseRefused
		}
		fmt.Fprintf(env.Stderr, "bdshim: close %s: %v\n", strings.Join(partial.NotClosed, " "), refusal)
		// bd's 0 is for a batch that closed some of its ids; one that closed
		// none is the failure a single refused close is.
		if len(partial.Closed) > 0 {
			return 0
		}
		if isGuardFailure(refusal) {
			return bdGuardExit
		}
		return bdFailureExit
	}
	fmt.Fprintf(env.Stderr, "bdshim: close %s: %v\n", strings.Join(ids, " "), err)
	if isGuardFailure(err) {
		return bdGuardExit
	}
	return bdFailureExit
}

// closeLine writes bd's close line for one id. A stream that refuses the write
// is the shim's error, not a close the caller does not hear about.
func closeLine(env Env, id, reason string) int {
	if _, err := fmt.Fprintf(env.Stdout, "✓ Closed %s: %s\n", id, reason); err != nil {
		return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
	}
	return 0
}

// reopenHandler answers bd reopen for a tracker rig.
func reopenHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("reopen", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, reopenFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("reopen needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return reopenIssues(env, st, fl.operands)
}

// reopenIssues returns ids to open status. An id that is already open is a no-op
// bd reports nothing for (the recorded golden), and one it did reopen gets bd's
// line.
func reopenIssues(env Env, st writeStore, ids []string) int {
	open := "open"
	failures := 0
	for _, id := range ids {
		is, err := st.Show(id)
		if err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: reopen %s: %v\n", id, err)
			continue
		}
		if is.Status == open {
			continue
		}
		if err := st.Update(id, beads.UpdateOptions{Status: &open}); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: reopen %s: %v\n", id, err)
			continue
		}
		if _, err := fmt.Fprintf(env.Stdout, "✓ Reopened %s\n", id); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
	}
	if failures == 0 {
		return 0
	}
	return bdFailureExit
}
