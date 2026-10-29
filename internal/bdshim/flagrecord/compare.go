package flagrecord

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The row kinds a recorded flag file holds, in the order the file writes them.
const (
	rowPersistent = "persistent"
	rowAlias      = "alias"
	rowOperand    = "operand"
	rowValue      = "value"
	rowBool       = "bool"
)

// rowKinds is the order renderFlagFile writes rows in, which is the order
// Compare reports a difference in.
var rowKinds = []string{rowPersistent, rowAlias, rowOperand, rowValue, rowBool}

// versionLineRE matches the heading's first line, "# What bd 1.2.2 reads a
// value for, per command: ...".
var versionLineRE = regexp.MustCompile(`^# What bd (\S+) reads a value for`)

// Drift is the first row on which a recorded flag file and what bd's help
// records now disagree.
type Drift struct {
	Kind    string // the row kind: persistent, alias, operand, value or bool
	Command string // the command path the row is about; empty for the persistent row
	// Flag is the first token one side has and the other does not, spelled as
	// the file spells it. Empty means one side has no row at all, and
	// InRecorded says which.
	Flag       string
	InRecorded bool     // whether the recorded file holds Flag, or the row when Flag is empty
	Recorded   []string // the recorded file's tokens for the row
	Installed  []string // the tokens bd's help records for the row
}

// String names the disagreement in one line, the command and flag first.
func (d Drift) String() string {
	subject := "the persistent flags"
	if d.Command != "" {
		subject = "`bd " + d.Command + "`"
	}
	switch {
	case d.Kind == rowAlias && d.Flag != "":
		if d.InRecorded {
			return fmt.Sprintf("%s: the recorded file reads it as `bd %s`, which bd's help does not", subject, d.Flag)
		}
		return fmt.Sprintf("%s: bd's help reads it as `bd %s`, which the recorded file does not", subject, d.Flag)
	case d.Flag == "" && d.InRecorded:
		return fmt.Sprintf("%s: the recorded file has a %s row and bd's help has none", subject, kindName(d.Kind))
	case d.Flag == "":
		return fmt.Sprintf("%s: bd's help has a %s row and the recorded file has none", subject, kindName(d.Kind))
	case d.InRecorded:
		return fmt.Sprintf("%s: the recorded file has %s and bd's help does not", subject, d.Flag)
	default:
		return fmt.Sprintf("%s: bd's help has %s and the recorded file does not", subject, d.Flag)
	}
}

// kindName is how a report names one row kind.
func kindName(kind string) string {
	switch kind {
	case rowPersistent:
		return "persistent value flag"
	case rowAlias:
		return "alias"
	case rowOperand:
		return "operand"
	case rowValue:
		return "value flag"
	case rowBool:
		return "boolean flag"
	}
	return kind
}

// Compare returns the first row on which the recorded flag file and the file
// bd's help records now disagree, or nil when the rows agree. The heading's
// version is not a row; read it with Version.
func Compare(recorded, installed []byte) (*Drift, error) {
	have, err := parseRows(recorded)
	if err != nil {
		return nil, fmt.Errorf("the recorded file: %w", err)
	}
	want, err := parseRows(installed)
	if err != nil {
		return nil, fmt.Errorf("bd's help: %w", err)
	}
	for _, kind := range rowKinds {
		for _, command := range unionCommands(have[kind], want[kind]) {
			a, hasA := have[kind][command]
			b, hasB := want[kind][command]
			if hasA && hasB {
				if flag, inRecorded, differs := rowDrift(a, b); differs {
					return &Drift{Kind: kind, Command: command, Flag: flag, InRecorded: inRecorded, Recorded: a, Installed: b}, nil
				}
				continue
			}
			if hasA {
				return &Drift{Kind: kind, Command: command, InRecorded: true, Recorded: a}, nil
			}
			return &Drift{Kind: kind, Command: command, Installed: b}, nil
		}
	}
	return nil, nil
}

// rowDrift reports the first token one row has and the other does not,
// preferring a token only the installed bd has: a value flag the shim does not
// know is the failure this comparison exists for (gt-w8j7v). inRecorded says
// which side has it.
func rowDrift(recorded, installed []string) (flag string, inRecorded, differs bool) {
	have := tokenSet(recorded)
	want := tokenSet(installed)
	var added, dropped []string
	for _, tok := range installed {
		if !have[tok] {
			added = append(added, tok)
		}
	}
	for _, tok := range recorded {
		if !want[tok] {
			dropped = append(dropped, tok)
		}
	}
	if len(added) > 0 {
		sort.Strings(added)
		return added[0], false, true
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		return dropped[0], true, true
	}
	// The same tokens in another order: only an operand row can hold those,
	// since the flag rows are sorted.
	for i := range recorded {
		if recorded[i] != installed[i] {
			return installed[i], false, true
		}
	}
	return "", false, false
}

// tokenSet is the set of tokens a row holds.
func tokenSet(row []string) map[string]bool {
	set := make(map[string]bool, len(row))
	for _, tok := range row {
		set[tok] = true
	}
	return set
}

// unionCommands returns every command path either side has a row for, sorted
// so a report names the same row whatever order the two files wrote.
func unionCommands(a, b map[string][]string) []string {
	commands := make([]string, 0, len(a)+len(b))
	seen := make(map[string]bool, len(a)+len(b))
	for _, side := range []map[string][]string{a, b} {
		for command := range side {
			if !seen[command] {
				seen[command] = true
				commands = append(commands, command)
			}
		}
	}
	sort.Strings(commands)
	return commands
}

// parseRows reads a recorded flag file into one token list per row, keyed by
// row kind and then command path. A line that is not a row of a kind this
// package writes is an error: a file it cannot read is unverified, never read
// as agreeing.
func parseRows(file []byte) (map[string]map[string][]string, error) {
	rows := map[string]map[string][]string{}
	for n, line := range strings.Split(string(file), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		command, rest := "", ""
		switch {
		case fields[0] == rowPersistent && len(fields) == 2:
			rest = fields[1]
		case fields[0] != rowPersistent && isRowKind(fields[0]) && len(fields) == 3:
			command, rest = fields[1], fields[2]
		default:
			return nil, fmt.Errorf("line %d: %q is not a recorded flag row", n+1, line)
		}
		if rows[fields[0]] == nil {
			rows[fields[0]] = map[string][]string{}
		}
		rows[fields[0]][command] = strings.Fields(rest)
	}
	return rows, nil
}

// isRowKind reports whether kind is one of the row kinds a recorded file
// holds.
func isRowKind(kind string) bool {
	for _, k := range rowKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Version returns the bd version a recorded flag file's heading names, or ""
// when the heading names none.
func Version(file []byte) string {
	for _, line := range strings.Split(string(file), "\n") {
		if m := versionLineRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// Header returns the comment paragraph a recorded flag file opens with, in
// order and without the "#" markers, so a report can repeat the refresh steps
// the file documents. The paragraph ends at the blank comment line that
// separates it from the row legend.
func Header(file []byte) []string {
	var out []string
	for _, line := range strings.Split(string(file), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if text == "" {
			break
		}
		out = append(out, text)
	}
	return out
}
