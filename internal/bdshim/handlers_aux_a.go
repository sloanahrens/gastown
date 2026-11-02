package bdshim

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// The first auxiliary handlers: comments, comment and label for a tracker rig
// (gt-p85pw.98). Each answers the flags its recorded goldens cover and refuses
// any other flag by name, so a caller never gets a wider read or write than it
// asked for. The store is opened through the backend seam, from the route the
// router chose, so this package holds no second tracker client.
//
// bd's comments (plural) reads an issue's comments and comment (singular) adds
// one, so the two verbs answer different commands. label is one verb with two
// subcommands, list and add, and the handler reads which one the argv names
// before it does anything.

// The flag set each auxiliary verb answers: the flags its golden rows cover.
var (
	commentsFlags  = readFlagSpec{"--json": {}}
	commentFlags   = readFlagSpec{}
	labelListFlags = readFlagSpec{"--json": {}}
	labelAddFlags  = readFlagSpec{}
)

// bdCommentsAnswer is bd's answer to comments --json: the comments oldest
// first, then the schema version.
type bdCommentsAnswer struct {
	Data          []bdComment `json:"data"`
	SchemaVersion int         `json:"schema_version"`
}

// bdComment is one comment as bd's comments prints it, in bd's member order.
type bdComment struct {
	ID        string `json:"id"`
	IssueID   string `json:"issue_id"`
	Author    string `json:"author"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// bdLabelAnswer is bd's answer to label list --json: the issue's labels, then
// the schema version.
type bdLabelAnswer struct {
	Data          []string `json:"data"`
	SchemaVersion int      `json:"schema_version"`
}

// auxCommentStore is the part of a rig's store the comment handlers use. The
// handlers open the full backend.Store; the narrow interface lets a test drive
// each refusal without a tracker server.
type auxCommentStore interface {
	Show(id string) (*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
	AddComment(id, comment string) error
}

// auxLabelStore is the part of a rig's store the label handler uses, narrowed
// for the same reason.
type auxLabelStore interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
}

// commentsHandler answers bd comments --json for a tracker rig.
func commentsHandler(env Env, d Decision) int {
	// comments add is bd's other comments command, and the shim answers only
	// the read; naming it here refuses it before its flags are read as the
	// read's.
	if command := commandPath(env.Args); command != "comments" {
		return shimError(env, fmt.Errorf("%s is not implemented for tracker rig %s: only comments is", command, d.Route.Prefix))
	}
	fl, err := parseReadArgs("comments", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, commentsFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	// The goldens record comments only as JSON, and bd's other shapes are not
	// recorded, so a command without --json is refused rather than printed in
	// a shape the shim cannot compare with bd.
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("comments without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("comments needs an issue id"))
	case 1:
	default:
		return shimError(env, fmt.Errorf("comments takes one issue id; %q is not implemented for tracker rig %s", fl.operands[1], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	comments, err := st.Comments(fl.operands[0])
	if err != nil {
		return shimError(env, fmt.Errorf("comments %s: %w", fl.operands[0], err))
	}
	answer := bdCommentsAnswer{Data: commentRows(comments), SchemaVersion: bdSchemaVersion}
	if err := printBDAnswer(env.Stdout, answer); err != nil {
		return shimError(env, err)
	}
	return 0
}

// commentRows renders the comments a comments read answered as bd's rows.
func commentRows(comments []beads.Comment) []bdComment {
	rows := make([]bdComment, 0, len(comments))
	for _, c := range comments {
		rows = append(rows, bdComment{
			ID: c.ID, IssueID: c.IssueID, Author: c.Author, Text: c.Text, CreatedAt: c.CreatedAt,
		})
	}
	return rows
}

// commentHandler answers bd comment (singular, which adds a comment) for a
// tracker rig.
func commentHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("comment", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, commentFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("comment needs an issue id"))
	}
	text := strings.TrimSpace(strings.Join(fl.operands[1:], " "))
	if text == "" {
		return shimError(env, errors.New("comment needs text"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return commentIssue(env, st, fl.operands[0], text)
}

// commentIssue adds text to id and prints bd's line, which names the issue's
// title. The title is read before the write, so a refused write prints no line
// and an id the store does not hold fails before anything is appended.
func commentIssue(env Env, st auxCommentStore, id, text string) int {
	is, err := st.Show(id)
	if err != nil {
		return shimError(env, fmt.Errorf("comment %s: %w", id, err))
	}
	if err := st.AddComment(id, text); err != nil {
		return shimError(env, fmt.Errorf("comment %s: %w", id, err))
	}
	if _, err := fmt.Fprintf(env.Stdout, "✓ Comment added to %s — %s\n", id, is.Title); err != nil {
		return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
	}
	return 0
}

// labelHandler answers bd label list and label add for a tracker rig. Any
// other subcommand is refused by name: the shim answers only the two the
// goldens cover.
func labelHandler(env Env, d Decision) int {
	switch command := commandPath(env.Args); command {
	case "label list":
		return labelList(env, d)
	case "label add":
		return labelAdd(env, d)
	default:
		return shimError(env, fmt.Errorf("%s is not implemented for tracker rig %s: only label list and label add are", command, d.Route.Prefix))
	}
}

// labelList answers bd label list --json for a tracker rig.
func labelList(env Env, d Decision) int {
	fl, err := parseReadArgs("label list", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, labelListFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("label list without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("label list needs an issue id"))
	case 1:
	default:
		return shimError(env, fmt.Errorf("label list takes one issue id; %q is not implemented for tracker rig %s", fl.operands[1], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	labels, err := labelRows(st, fl.operands[0])
	if err != nil {
		return shimError(env, err)
	}
	if err := printBDAnswer(env.Stdout, bdLabelAnswer{Data: labels, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// labelRows reads one issue's labels. An issue with none answers an empty
// array rather than null, which is the list bd prints.
func labelRows(st auxLabelStore, id string) ([]string, error) {
	is, err := st.Show(id)
	if err != nil {
		return nil, fmt.Errorf("label list %s: %w", id, err)
	}
	if is.Labels == nil {
		return []string{}, nil
	}
	return is.Labels, nil
}

// labelAdd answers bd label add for a tracker rig.
func labelAdd(env Env, d Decision) int {
	fl, err := parseReadArgs("label add", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, labelAddFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	ids, labels, err := labelAddOperands(fl.operands)
	if err != nil {
		return shimError(env, err)
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return addLabels(env, st, ids, labels)
}

// labelAddOperands reads the two operand slots bd's label add names: the
// issues the command writes (a variadic slot) and the labels it writes on them
// (the slot after it, one or more labels joined by commas). The read is the
// router's own (slotOf), so an operand the router does not read as a bead is
// not read as one here either. A command that names no issue, or an empty
// label, is refused rather than written as the other slot's value.
// An operand past the declared slots is read as an issue id, as the router
// reads it.
func labelAddOperands(ops []string) (ids, labels []string, err error) {
	slots := operandSlots["label add"]
	for i, op := range ops {
		slot, ok := slotOf(slots, len(ops), i)
		if !ok || slot.bead {
			ids = append(ids, op)
			continue
		}
		labels = append(labels, strings.Split(op, ",")...)
	}
	if len(ids) == 0 {
		return nil, nil, errors.New("label add needs an issue id")
	}
	for _, label := range labels {
		if label == "" {
			return nil, nil, errors.New("label add needs a label")
		}
	}
	return ids, labels, nil
}

// addLabels adds each label to each id and prints bd's line for every label it
// added. An id the store refuses is reported and the rest still run, as bd
// writes them one at a time; any refusal exits 1. A label the issue already
// carries still gets its line, as bd's own add does. A call naming no label is
// refused before any write: it would otherwise exit 0 having added nothing.
func addLabels(env Env, st auxLabelStore, ids, labels []string) int {
	if len(labels) == 0 {
		return shimError(env, errors.New("label add needs a label"))
	}
	failures := 0
	for _, id := range ids {
		if err := st.Update(id, beads.UpdateOptions{AddLabels: labels}); err != nil {
			failures++
			fmt.Fprintf(env.Stderr, "bdshim: label add %s: %v\n", id, err)
			continue
		}
		for _, label := range labels {
			if _, err := fmt.Fprintf(env.Stdout, "✓ Added label '%s' to %s\n", label, id); err != nil {
				return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
			}
		}
	}
	if failures == 0 {
		return 0
	}
	return bdFailureExit
}
