// Package townreboot runs, in the operations doc's order, the checks a person
// runs after a Mac reboot, and formats what each one found and the repair for it.
package townreboot

import (
	"context"
	"encoding/json"
	"strings"
)

// bootNoiseNote is the doc's step 9. A daemon started with Forgejo unreadable
// re-raises old post-land-unknown and deploy-pair-unreadable items for commits
// days old; a person who reads those as reboot damage chases work the boot did
// not cause (gt-g0adel).
const bootNoiseNote = "after a daemon start with Forgejo unreadable, older post-land-unknown and " +
	"deploy-pair-unreadable items for commits days old are raised again; " +
	"ack those as noise, not reboot damage"

// Check is one named read the engine performs.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// Result is one check's finding: whether the host is as it should be, what the
// check saw, and the repair to run when it is not.
type Result struct {
	OK     bool
	Detail string
	Fix    string
}

// Row is a check's name beside the result it returned, the unit both output
// forms are built from.
type Row struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// report is the JSON form: one array of the rows and the boot-noise note as its
// own field, so a reader can parse the rows without knowing the note.
type report struct {
	Rows []Row  `json:"rows"`
	Note string `json:"note"`
}

// RunAll runs every check in list order and returns one row each; a check that
// reports a problem does not stop the checks after it, because a reboot leaves
// several things broken at once and the person needs the whole list.
func RunAll(ctx context.Context, checks []Check) []Row {
	rows := make([]Row, 0, len(checks))
	for _, c := range checks {
		var res Result
		if c.Run == nil {
			// A check with no Run is a wiring bug. Calling it would panic and
			// hide every check after it, so the row reports the bug instead.
			res = Result{Detail: "check has no Run function"}
		} else {
			res = c.Run(ctx)
		}
		rows = append(rows, Row{Name: c.Name, OK: res.OK, Detail: res.Detail, Fix: res.Fix})
	}
	return rows
}

// Format renders the rows for the person: text is one line per row followed by
// the boot-noise note, and the JSON form is one object carrying the rows and
// the note.
func Format(rows []Row, json bool) string {
	if json {
		return formatJSON(rows)
	}
	return formatText(rows)
}

// formatText writes a line per row, then the note. The status word is a fixed
// four columns so the names line up and an eye finds the failures.
func formatText(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		status := "ok  "
		if !r.OK {
			status = "FAIL"
		}
		b.WriteString(status)
		b.WriteString(" ")
		b.WriteString(r.Name)
		if r.Detail != "" {
			b.WriteString(": ")
			b.WriteString(r.Detail)
		}
		if r.Fix != "" {
			b.WriteString(" (fix: ")
			b.WriteString(r.Fix)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("note: ")
	b.WriteString(bootNoiseNote)
	b.WriteString("\n")
	return b.String()
}

// formatJSON writes the rows and the note as one object. A caller that passes
// no rows gets an empty array, not null, so a reader can range the field.
func formatJSON(rows []Row) string {
	if rows == nil {
		rows = []Row{}
	}
	// Row holds only strings and a bool, so this encode cannot fail.
	data, _ := json.MarshalIndent(report{Rows: rows, Note: bootNoiseNote}, "", "  ")
	return string(data) + "\n"
}
