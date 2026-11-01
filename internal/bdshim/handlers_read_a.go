package bdshim

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
)

// The first read handlers: show, list and children for a tracker rig
// (gt-p85pw.94). Each answers the flags its recorded goldens cover and refuses
// any other flag by name, so a caller never reads a filtered answer as an
// unfiltered one. The store is opened through the backend seam, from the route
// the router chose, so this package holds no second tracker client.

// readFlags is the flag set each read verb answers, the flags its golden rows
// cover. --flat and --no-pager choose how bd renders a list; the shim always
// prints the flat JSON shape the goldens hold, so they are accepted and change
// nothing.
var (
	showFlags     = readFlagSpec{"--json": {}}
	childrenFlags = readFlagSpec{"--json": {}}
	listFlags     = readFlagSpec{
		"--json": {}, "--flat": {}, "--no-pager": {}, "--include-infra": {}, "--all": {},
		"--status": {}, "--limit": {}, "--parent": {}, "--type": {}, "--label": {}, "--id": {},
	}
)

// readFlagSpec is the set of flags one read verb answers. A flag outside the
// set is refused, never ignored. Whether a flag takes a value is bd's own table
// (takesValue, flags.go), so parsing and this set cannot disagree about a
// flag's shape.
type readFlagSpec map[string]struct{}

// parsedRead is a read command's flags and operands.
type parsedRead struct {
	// flags holds the values given for each flag, in argv order; a boolean
	// flag's entry is empty.
	flags map[string][]string
	// order is the flags as they first appeared, so a refusal names the first
	// unrecognized one as the caller wrote it.
	order []string
	// operands are the tokens after the command's own words.
	operands []string
}

func (p parsedRead) has(name string) bool { return len(p.flags[name]) > 0 }

func (p parsedRead) values(name string) []string { return p.flags[name] }

// last returns the last value given for a flag, or "" when it was not given.
func (p parsedRead) last(name string) string {
	values := p.flags[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// parseReadArgs reads args for the command path, which is the first len(words)
// operands. Whether a flag takes a value is bd's own table, so a flag this
// verb refuses still keeps its value from being read as an operand.
func parseReadArgs(command string, args []string) (parsedRead, error) {
	out := parsedRead{flags: map[string][]string{}}
	words := len(strings.Fields(command))
	pastSeparator := false
	pending := "" // a value flag whose value is the next token
	for _, arg := range args {
		switch {
		case pending != "":
			out.flags[pending] = append(out.flags[pending], arg)
			pending = ""
		case pastSeparator:
			out.operands = append(out.operands, arg)
		case arg == "--":
			pastSeparator = true
		case isFlag(arg):
			name, value, inline := strings.Cut(arg, "=")
			takes := takesValue(command, name)
			if _, seen := out.flags[name]; !seen {
				out.order = append(out.order, name)
			}
			switch {
			case takes && inline:
				out.flags[name] = append(out.flags[name], value)
			case takes:
				pending = name
			case inline:
				return out, fmt.Errorf("flag %s takes no value", name)
			default:
				out.flags[name] = append(out.flags[name], "")
			}
		case words > 0:
			words--
		default:
			out.operands = append(out.operands, arg)
		}
	}
	if pending != "" {
		return out, fmt.Errorf("flag %s needs a value", pending)
	}
	return out, nil
}

// refuseUnknownFlags refuses the first flag the verb's goldens do not cover,
// naming it and the rig. A covered flag that takes a value may not be given
// without one; parseReadArgs already refused that.
func refuseUnknownFlags(fl parsedRead, spec readFlagSpec, prefix string) error {
	for _, name := range fl.order {
		if _, ok := spec[name]; !ok {
			return fmt.Errorf("flag %s is not implemented for tracker rig %s", name, prefix)
		}
	}
	return nil
}

// openTrackerStore opens the store the route names, through the backend seam.
// The town root is found the way the router found it: the nearest .beads
// directory holding the route table, above the working directory or BEADS_DIR.
func openTrackerStore(env Env, d Decision) (backend.Store, error) {
	cwd, err := env.Getwd()
	if err != nil {
		return nil, fmt.Errorf("reading the working directory: %w", err)
	}
	workdir := cwd
	if dir := env.Getenv(beadsDirEnv); dir != "" {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		workdir = dir
	}
	found, err := findTown(workdir)
	if err != nil {
		return nil, err
	}
	if found.table == "" {
		return nil, fmt.Errorf("no route table above %s, so tracker rig %s cannot be opened", workdir, d.Route.Prefix)
	}
	townRoot := filepath.Dir(found.table)
	return backend.Open(townRoot, routeBeadsDir(townRoot, d.Route), backend.FromEnv(env.Getenv))
}

// shimError prints one of the shim's own messages and returns its exit code.
func shimError(env Env, err error) int {
	fmt.Fprintf(env.Stderr, "bdshim: %v\n", err)
	return 1
}

// showHandler answers bd show --json for a tracker rig.
func showHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("show", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, showFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("show without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("show needs an issue id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	rows := make([]bdShowIssue, 0, len(fl.operands))
	for _, id := range fl.operands {
		row, err := showRow(st, id)
		if err != nil {
			return shimError(env, err)
		}
		rows = append(rows, row)
	}
	if err := printBDAnswer(env.Stdout, bdShowAnswer{Data: rows, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// readStore is the part of a rig's store the read rows use. The handlers open
// the full backend.Store; the narrow interface lets a test drive each read's
// failure without a tracker server.
type readStore interface {
	Show(id string) (*beads.Issue, error)
	ShowMultiple(ids []string) (map[string]*beads.Issue, error)
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Children(parentID string) ([]*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
}

// showRow reads one issue and everything bd's show row prints about it.
func showRow(st readStore, id string) (bdShowIssue, error) {
	is, err := st.Show(id)
	if err != nil {
		return bdShowIssue{}, fmt.Errorf("show %s: %w", id, err)
	}
	depIDs := make([]string, 0, len(is.Dependencies))
	for _, dep := range is.Dependencies {
		depIDs = append(depIDs, dep.ID)
	}
	targets, err := st.ShowMultiple(depIDs)
	if err != nil {
		return bdShowIssue{}, fmt.Errorf("show %s: reading the beads it depends on: %w", id, err)
	}
	comments, err := st.Comments(id)
	if err != nil {
		return bdShowIssue{}, fmt.Errorf("show %s: reading its comments: %w", id, err)
	}
	return bdShowRow(is, targets, len(comments))
}

// listHandler answers bd list --json for a tracker rig.
func listHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("list", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, listFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("list without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("list takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	opts, err := readListOptions(fl)
	if err != nil {
		return shimError(env, err)
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	issues, err := st.List(opts)
	if err != nil {
		return shimError(env, fmt.Errorf("list: %w", err))
	}
	rows, err := listRows(st, issues)
	if err != nil {
		return shimError(env, err)
	}
	if err := printBDAnswer(env.Stdout, bdListAnswer{Data: rows, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// childrenHandler answers bd children --json for a tracker rig.
func childrenHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("children", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, childrenFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("children without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) != 1 {
		return shimError(env, errors.New("children needs one parent id"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	kids, err := st.Children(fl.operands[0])
	if err != nil {
		return shimError(env, fmt.Errorf("children of %s: %w", fl.operands[0], err))
	}
	rows, err := listRows(st, kids)
	if err != nil {
		return shimError(env, err)
	}
	if err := printBDAnswer(env.Stdout, bdListAnswer{Data: rows, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// listRows renders listed issues as bd's rows. Each issue is read again for
// the dependencies and dependents its row counts, and its comments for the
// count bd prints.
func listRows(st readStore, issues []*beads.Issue) ([]bdListIssue, error) {
	rows := make([]bdListIssue, 0, len(issues))
	for _, is := range issues {
		full, err := st.Show(is.ID)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", is.ID, err)
		}
		comments, err := st.Comments(is.ID)
		if err != nil {
			return nil, fmt.Errorf("reading %s's comments: %w", is.ID, err)
		}
		rows = append(rows, bdListRow(full, len(comments)))
	}
	return rows, nil
}

// readListOptions maps the flags the handler answers to the store's list
// filter. A combination the goldens do not cover is refused rather than given
// one flag's meaning.
func readListOptions(fl parsedRead) (beads.ListOptions, error) {
	var opts beads.ListOptions
	if fl.has("--all") && fl.has("--status") {
		return opts, errors.New("list --all together with --status is not implemented: name one of them")
	}
	if fl.has("--all") {
		opts.Status = "all"
	}
	if status := fl.last("--status"); status != "" {
		opts.Status = status
	}
	opts.Parent = fl.last("--parent")
	opts.IssueType = fl.last("--type")
	opts.Labels = fl.values("--label")
	opts.IDs = fl.values("--id")
	opts.IncludeInfra = fl.has("--include-infra")
	if fl.has("--limit") {
		n, err := strconv.Atoi(fl.last("--limit"))
		if err != nil || n < 0 {
			return opts, fmt.Errorf("--limit %q is not a count of results", fl.last("--limit"))
		}
		opts.Limit = n
	}
	return opts, nil
}
