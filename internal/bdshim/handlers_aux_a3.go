package bdshim

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// The third auxiliary handler: dep for a tracker rig (gt-p85pw.120). A tracker
// keeps every link it writes in the town's one database, and a link to another
// rig is stored as external:<rig>:<id>, which has no target row here. The
// tracker lists such a link id-only, but bd's own dep list leaves it out and
// the shim answers as bd does, so dep list shows the rig's own edges only (see
// bdView). dep add refuses a target outside the rig until its bd answer is
// pinned (gt-p85pw.120), and every other dep subcommand is refused by name.
//
// dep is one verb with several subcommands, so the handler reads which one the
// argv names before it does anything: add and list are answered, and the rest
// are refused by name before their flags or operands are read as the answered
// commands'.

// depBlocks is the relation dep add records when it names no type. bd's --type
// is not covered by a golden, so it is refused rather than defaulted.
const depBlocks = "blocks"

// The flag set each dep subcommand answers: what its golden row covers. dep
// add's golden carries no flag, and dep list's carries --direction down and
// --json.
var (
	depAddFlags  = readFlagSpec{}
	depListFlags = readFlagSpec{"--direction": {}, "--json": {}}
)

// auxDepStore is the part of a rig's store the dep handlers use. The handlers
// open the full backend.Store; the narrow interface lets a test drive each
// refusal without a tracker server.
type auxDepStore interface {
	Show(id string) (*beads.Issue, error)
	ShowMultiple(ids []string) (map[string]*beads.Issue, error)
	AddDependency(issue, dependsOn string) error
}

// bdDepListAnswer is bd's answer to dep list --json: the target of each link
// with the relation between them, then the schema version.
type bdDepListAnswer struct {
	Data          []bdShowDep `json:"data"`
	SchemaVersion int         `json:"schema_version"`
}

// depHandler answers bd dep add and dep list for a tracker rig, and refuses
// every other dep subcommand by name.
func depHandler(env Env, d Decision) int {
	switch command := depCommand(env.Args); command {
	case "dep add":
		return depAddCommand(env, d)
	case "dep list":
		return depListCommand(env, d)
	default:
		return shimError(env, fmt.Errorf("%s is not implemented for tracker rig %s", command, d.Route.Prefix))
	}
}

// depCommand names the dep command the argv asks for: the verb and the word
// after it, through bd's alias table, so dep rm is named dep remove. A word bd
// does not document is still named as the caller's subcommand.
func depCommand(args []string) string {
	ops := operands("", args)
	if len(ops) == 0 {
		return "dep"
	}
	if len(ops) == 1 {
		return ops[0]
	}
	command := ops[0] + " " + ops[1]
	if canonical, ok := aliases[command]; ok {
		return canonical
	}
	return command
}

// depAddCommand answers bd dep add for a tracker rig.
func depAddCommand(env Env, d Decision) int {
	fl, err := parseReadArgs("dep add", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, depAddFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("dep add needs an issue id and a depends-on id"))
	case 1:
		return shimError(env, errors.New("dep add needs a depends-on id"))
	case 2:
	default:
		return shimError(env, fmt.Errorf("dep add takes two ids; %q is not implemented for tracker rig %s", fl.operands[2], d.Route.Prefix))
	}
	issue, target := fl.operands[0], fl.operands[1]
	// A target outside this rig, plain or already wrapped as external:<rig>:<id>,
	// is refused before the store is opened: the shim has no pinned bd answer
	// for writing a cross-rig link (gt-p85pw.120), so it does not write one.
	if strings.HasPrefix(target, "external:") || beads.ExtractPrefix(target) != d.Route.Prefix {
		return shimError(env, fmt.Errorf("dep add of a cross-rig target %s is not implemented for tracker rig %s", target, d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return addDep(env, st, issue, target)
}

// addDep records the link and answers as bd does: both beads' titles are read
// before the write, so a bead the store cannot answer is refused rather than
// printed with an empty title, and nothing is written for it.
func addDep(env Env, st auxDepStore, issue, target string) int {
	found, err := st.ShowMultiple([]string{issue, target})
	if err != nil {
		return shimError(env, fmt.Errorf("dep add %s %s: reading the beads: %w", issue, target, err))
	}
	for _, id := range []string{issue, target} {
		if _, ok := found[id]; !ok {
			return shimError(env, fmt.Errorf("dep add %s %s: %s: %w", issue, target, id, beads.ErrNotFound))
		}
	}
	if err := st.AddDependency(issue, target); err != nil {
		return shimError(env, fmt.Errorf("dep add %s %s: %w", issue, target, err))
	}
	answer := fmt.Sprintf("✓ Added dependency: %s (%s) depends on %s (%s) (%s)\n",
		issue, found[issue].Title, target, found[target].Title, depBlocks)
	if _, err := fmt.Fprint(env.Stdout, answer); err != nil {
		// The link is already in the store: the answer is printed only after
		// the write, so this failure must say the write landed rather than
		// read as a dep add a caller should retry.
		return shimError(env, fmt.Errorf("dep add %s %s: the link was written but bd's answer could not be printed: %w", issue, target, err))
	}
	return 0
}

// depListCommand answers bd dep list for a tracker rig.
func depListCommand(env Env, d Decision) int {
	fl, err := parseReadArgs("dep list", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, depListFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	// The goldens record only the JSON answer, and only the links an issue
	// depends on, so a command without either is refused rather than printed in
	// a shape bd did not record.
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("dep list without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	switch direction := fl.last("--direction"); direction {
	case "down":
	case "":
		return shimError(env, fmt.Errorf("dep list without --direction down is not implemented for tracker rig %s", d.Route.Prefix))
	default:
		return shimError(env, fmt.Errorf("dep list --direction %s is not implemented for tracker rig %s", direction, d.Route.Prefix))
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("dep list needs an issue id"))
	case 1:
	default:
		return shimError(env, fmt.Errorf("dep list takes one issue id; %q is not implemented for tracker rig %s", fl.operands[1], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return listDeps(env, st, fl.operands[0])
}

// listDeps prints the links the issue depends on, as bd lists them: the view
// opened by openTrackerStore leaves out a target outside this database
// (external:<rig>:<id>), so dep list answers this rig's own edges. bd prints
// each row's created_at and updated_at from the target bead, which the link
// does not carry, so the targets are read once more.
func listDeps(env Env, st auxDepStore, id string) int {
	is, err := st.Show(id)
	if err != nil {
		return shimError(env, fmt.Errorf("dep list %s: %w", id, err))
	}
	ids := make([]string, 0, len(is.Dependencies))
	for _, dep := range is.Dependencies {
		ids = append(ids, dep.ID)
	}
	targets, err := st.ShowMultiple(ids)
	if err != nil {
		return shimError(env, fmt.Errorf("dep list %s: reading the beads it depends on: %w", id, err))
	}
	rows := make([]bdShowDep, 0, len(is.Dependencies))
	for _, dep := range is.Dependencies {
		t, ok := targets[dep.ID]
		if !ok {
			return shimError(env, fmt.Errorf("dep list %s: the store did not answer the bead %s it depends on", id, dep.ID))
		}
		rows = append(rows, bdShowDep{
			ID: t.ID, Title: t.Title, Description: t.Description, Status: t.Status,
			Priority: t.Priority, Type: t.Type, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			DependencyType: dep.DependencyType,
		})
	}
	if err := printBDAnswer(env.Stdout, bdDepListAnswer{Data: rows, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}
