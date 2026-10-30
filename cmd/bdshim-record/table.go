package main

import "strings"

// fixturePrefix is the issue prefix every copy's database is initialized with, the one the fixture
// file's bead ids carry.
const fixturePrefix = "fx"

// The beads the fixture database must hold, since the table's rows name them. bd is asked for each
// one before any row runs, so a fixture that is missing one fails the recording instead of being
// recorded as bd's "not found".
const (
	fixtureID     = "fx-1"
	fixtureDepID  = "fx-2"
	fixtureParent = "fx-parent"
)

// fixtureIDs is every bead the preflight asks bd for.
var fixtureIDs = []string{fixtureID, fixtureDepID, fixtureParent}

// Row is one verb and flag set that bd is recorded for.
type Row struct {
	Verb     string   // the command words before any flag, e.g. "dep add"
	Flags    []string // flag tokens in the order bd takes them, e.g. "--status", "open"
	Operands []string // positional arguments after the flags
	Mutates  bool     // whether the row changes state; a mutating row gets a copy of the fixture of its own
}

// Args is the argument vector bd is run with: the verb words, the flags, then the operands.
func (r Row) Args() []string {
	args := append(strings.Fields(r.Verb), r.Flags...)
	return append(args, r.Operands...)
}

// GoldenName is the file stem for a row: the verb words, then each flag token, lower-cased, with
// every run of characters outside a-z and 0-9 turned into one dash. Operands are not in the name,
// so rows that differ only in an operand collide and Capture refuses the table.
func GoldenName(r Row) string {
	var parts []string
	for _, tok := range append(strings.Fields(r.Verb), r.Flags...) {
		if s := sanitize(tok); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "-")
}

// sanitize keeps a-z and 0-9 from tok and joins every other run with one dash, so the result
// holds no path separator and no space.
func sanitize(tok string) string {
	var b strings.Builder
	pendingDash := false
	for _, c := range strings.ToLower(tok) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(c)
			continue
		}
		pendingDash = true
	}
	return b.String()
}

// Table is the verb and flag set bd is recorded for, in the order the goldens are written. The
// read rows come from the bd read verbs gt calls (docs/research/tracker/R6-agent-bd-commands.md)
// and the core verbs in docs/plans/2026-10-25-town-tracker-design.md section 6.
func Table() []Row {
	return []Row{
		// Read rows: share one copy of the fixture.
		{Verb: "show", Flags: []string{"--json"}, Operands: []string{fixtureID}},
		{Verb: "list", Flags: []string{"--json"}},
		{Verb: "list", Flags: []string{"--json", "--status", "open"}},
		{Verb: "list", Flags: []string{"--json", "--all", "--limit", "0"}},
		{Verb: "list", Flags: []string{"--json", "--parent", fixtureParent}},
		{Verb: "list", Flags: []string{"--json", "--type", "task"}},
		{Verb: "list", Flags: []string{"--json", "--label", "gt:agent"}},
		{Verb: "list", Flags: []string{"--id", fixtureID, "--include-infra", "--all", "--json", "--flat", "--no-pager"}},
		{Verb: "ready", Flags: []string{"--json"}},
		{Verb: "ready", Flags: []string{"--json", "--limit", "5"}},
		{Verb: "ready", Flags: []string{"--json", "--unassigned"}},
		{Verb: "query", Flags: []string{"--json"}, Operands: []string{"status=open"}},
		{Verb: "query", Flags: []string{"--json", "--all", "--limit", "0"}, Operands: []string{"status=open"}},
		{Verb: "search", Flags: []string{"--json", "--limit", "10", "--status", "open"}, Operands: []string{"fixture"}},
		{Verb: "blocked", Flags: []string{"--json"}},
		{Verb: "comments", Flags: []string{"--json"}, Operands: []string{fixtureID}},
		{Verb: "children", Flags: []string{"--json"}, Operands: []string{fixtureParent}},
		{Verb: "count", Flags: []string{"--status", "open"}},
		{Verb: "count", Flags: []string{"--by-status"}},
		{Verb: "stats", Flags: []string{"--json"}},
		{Verb: "config list"},
		{Verb: "config get", Operands: []string{"export.auto"}},
		{Verb: "kv list"},
		{Verb: "kv get", Operands: []string{"fixture-key"}},
		{Verb: "dep list", Flags: []string{"--direction", "down", "--json"}, Operands: []string{fixtureID}},
		{Verb: "label list", Flags: []string{"--json"}, Operands: []string{fixtureID}},
		{Verb: "events tail", Operands: []string{"0"}},

		// Mutating rows: each runs in a copy of the fixture of its own.
		{Verb: "create", Flags: []string{"--title", "fixture-created", "--type", "task", "--priority", "3", "--json"}, Mutates: true},
		{Verb: "update", Flags: []string{"--status", "in_progress"}, Operands: []string{fixtureID}, Mutates: true},
		{Verb: "close", Flags: []string{"--reason", "fixture-close"}, Operands: []string{fixtureID}, Mutates: true},
		{Verb: "reopen", Operands: []string{fixtureID}, Mutates: true},
		{Verb: "delete", Flags: []string{"--force"}, Operands: []string{fixtureID}, Mutates: true},
		{Verb: "defer", Flags: []string{"--reason", "fixture-defer"}, Operands: []string{fixtureID}, Mutates: true},
		{Verb: "undefer", Operands: []string{fixtureID}, Mutates: true},
		{Verb: "unclaim", Operands: []string{fixtureID}, Mutates: true},
		{Verb: "dep add", Operands: []string{fixtureID, fixtureDepID}, Mutates: true},
		{Verb: "label add", Operands: []string{fixtureID, "fixture-label"}, Mutates: true},
		{Verb: "comment", Operands: []string{fixtureID, "fixture note"}, Mutates: true},
		{Verb: "kv set", Operands: []string{"fixture-key", "fixture-value"}, Mutates: true},
		{Verb: "config set", Operands: []string{"export.auto", "false"}, Mutates: true},
	}
}
