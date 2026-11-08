package bdshim

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// refusedVerbs are the verbs a tracker rig refuses by name: Main looks the
// handler registry up by the raw first word of the command (verbOf), so a name
// bd reads as an alias is listed here unless the registry answers that name
// itself. Every verb here exits 1 with Refusal's message and never reaches
// bd.real.
//
// The table is the per-verb decision record, and it does repeat the bd command
// list by hand: TestRefusesEveryUnimplementedVerb drives one subtest per row,
// and coverageError reads the row against every command testdata/bd-flags.txt
// records, so the next command bd gains fails the suite until this table, the
// handler registry or passThroughVerbs classifies it. The hand copy is what the
// coverage check keeps honest, not a claim that no copy exists.
//
// The verbs be-xu2.6 deleted from bd are no longer tabled: the doctor flags
// check compares the installed bd's flags with testdata/bd-flags.txt, so a bd
// that still had them fails that check, and a row would pin a command that no
// longer exists.
var refusedVerbs = []string{
	"backup",
	"branch",
	"create-form",
	"doctor",
	"dolt",
	"done",
	"epic",
	"export",
	"flatten",
	"forget",
	"gate",
	"gc",
	"hb",
	"heartbeat",
	"help",
	"history",
	"hooks",
	"human",
	"import",
	"init",
	"land-record",
	"mail",
	"memories",
	"merge-slot",
	"migrate",
	"mol",
	"new",
	"note",
	"prime",
	"priority",
	"protomolecule",
	"purge",
	"q",
	"recall",
	"reclaim",
	"remember",
	"rename",
	"rename-prefix",
	"repo",
	"set-state",
	"sql",
	"state",
	"status",
	"supersede",
	"todo",
	"vc",
	"view",
	"worktree",
}

// classification is what a tracker rig does with one bd command: refuse the
// verb by name, or answer it.
type classification struct {
	// verb is the word Main dispatches on: the raw first operand (verbOf), not
	// an alias resolved through the aliases table.
	verb string
	// refused says Main printed Refusal for verb instead of answering it.
	refused bool
}

// pinnedRefusal is the refusal message a bead fixes word for word, or "" when
// the verb is checked against the shape every refusal shares. The pinned message
// for sql is written out here rather than built by Refusal, so a change to
// Refusal cannot move both sides of that comparison together; the shared shape
// the other verbs are checked against is Refusal's own (classifyCommand compares
// against it). prefix is the tracker rig the fixture routes to.
func pinnedRefusal(verb, prefix string) string {
	if verb == "sql" {
		// R9 section 0 keeps bd sql refused for good: the direct-Dolt consumers
		// treat a database read as fatal, so a tracker rig has to fail the
		// command loudly instead of answering it.
		return "bdshim: verb sql is not implemented for tracker rig " + prefix
	}
	return ""
}

// trackerPrefix is the prefix of the tracker route rig routes to, read through
// Decide so the test never restates the prefix its fixture writes.
func trackerPrefix(t *testing.T, rig string) string {
	t.Helper()
	d, err := decide(t, rig, "")
	if err != nil {
		t.Fatalf("routing from %s: %v", rig, err)
	}
	if !d.Tracker {
		t.Fatalf("routing from %s found no tracker rig", rig)
	}
	return d.Route.Prefix
}

// classifyCommand runs the production entry point, Main, from rig with one bd
// command and reports what it did. The answered-or-refused decision is Main's
// own: a command it answers (a handler, or cook and formula passing to
// bd.real) leaves its own message or none on stderr, while a refused command
// leaves exactly Refusal's.
func classifyCommand(t *testing.T, rig, prefix, command string) classification {
	t.Helper()
	args := strings.Fields(command)
	proc := &fakeProcess{}
	e, stderr, _ := shimEnv(t, rig, "", proc, handlers, args...)
	Main(e)
	verb := verbOf(args)
	return classification{verb: verb, refused: verb != "" && stderr.String() == Refusal(verb, prefix)+"\n"}
}

// documentedCommands is every command bd's recorded help names: its value, bool
// and operand rows, and each alias name with the command it stands for. Both
// words are classified because both are words a caller can type, even when bd
// records no row for the target: status is reached here only as stats's alias
// target, and the registry answers stats rather than status.
func documentedCommands(t *testing.T) []string {
	t.Helper()
	value, boolean, _, aliased := bdDocumented(t)
	commands := map[string]bool{}
	for command := range value {
		commands[command] = true
	}
	for command := range boolean {
		commands[command] = true
	}
	for command := range bdOperands(t) {
		commands[command] = true
	}
	for name, command := range aliased {
		commands[name] = true
		commands[command] = true
	}
	// coverageError walks the list, so a recording that parsed to nothing would
	// pass it and leave the table looking complete.
	if len(commands) < 100 {
		t.Fatalf("testdata/bd-flags.txt names %d commands, want at least 100", len(commands))
	}
	sorted := make([]string, 0, len(commands))
	for command := range commands {
		sorted = append(sorted, command)
	}
	sort.Strings(sorted)
	return sorted
}

// coverageError reports every command in commands that a tracker rig neither
// answers nor refuses by a row in rows, and every row that no documented
// command reaches. The row table is a parameter so a test drives it with an
// invented name as well as with refusedVerbs, and classify is injected so the
// answered-or-refused decision is classifyCommand's Main in the real check
// rather than a second copy of its dispatch.
func coverageError(commands, rows []string, classify func(string) classification) error {
	refused := make(map[string]bool, len(rows))
	var problems []string
	for _, verb := range rows {
		if refused[verb] {
			problems = append(problems, fmt.Sprintf("the refusal table lists %s twice", verb))
			continue
		}
		refused[verb] = true
	}
	reached := make(map[string]bool, len(commands))
	for _, command := range commands {
		c := classify(command)
		if c.verb == "" {
			problems = append(problems, fmt.Sprintf("bd documents %q, and Main dispatches it to no verb", command))
			continue
		}
		reached[c.verb] = true
		switch {
		case c.refused && !refused[c.verb]:
			problems = append(problems, fmt.Sprintf("bd documents %q, and verb %s is refused by name with no refusal row", command, c.verb))
		case !c.refused && refused[c.verb]:
			problems = append(problems, fmt.Sprintf("bd documents %q, which a tracker rig answers, and the refusal table lists verb %s", command, c.verb))
		}
	}
	for _, verb := range rows {
		if !reached[verb] {
			problems = append(problems, fmt.Sprintf("the refusal table lists %s, and no command bd documents reaches that verb", verb))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// TestRefusesEveryUnimplementedVerb drives the production entry point from a
// tracker rig directory once per verb: each is refused by name, exits 1, and
// never runs bd.real in its place.
func TestRefusesEveryUnimplementedVerb(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	beta := filepath.Join(town, "beta", "mayor", "rig")
	prefix := trackerPrefix(t, beta)

	for _, verb := range refusedVerbs {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			proc := &fakeProcess{}
			e, stderr, _ := shimEnv(t, beta, "", proc, handlers, verb)

			if got := Main(e); got != 1 {
				t.Errorf("Main(%s) = %d, want 1", verb, got)
			}
			want := "bdshim: verb " + verb + " is not implemented for tracker rig " + prefix
			if pinned := pinnedRefusal(verb, prefix); pinned != "" {
				want = pinned
			}
			if got := stderr.String(); got != want+"\n" {
				t.Errorf("stderr = %q, want %q", got, want)
			}
			if proc.ran != nil {
				t.Errorf("Main(%s) ran bd.real with %v, want the verb refused before it", verb, proc.ran)
			}
		})
	}
}

// TestRefusesADoltRemoteSubverbAndItsOperand pins the invocation the namespace
// step in commandPath exists for: `remote` and `reset-data` are command words,
// not operands, so the subverb cannot read as a bead id and the command cannot
// reach bd.real from a tracker rig.
func TestRefusesADoltRemoteSubverbAndItsOperand(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	beta := filepath.Join(town, "beta", "mayor", "rig")
	prefix := trackerPrefix(t, beta)
	proc := &fakeProcess{}
	e, stderr, _ := shimEnv(t, beta, "", proc, handlers, "dolt", "remote", "reset-data", "origin")

	if got := Main(e); got != 1 {
		t.Errorf("Main(dolt remote reset-data origin) = %d, want 1", got)
	}
	want := "bdshim: verb dolt is not implemented for tracker rig " + prefix + "\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if proc.ran != nil {
		t.Errorf("Main ran bd.real with %v, want dolt refused before it", proc.ran)
	}
}

// TestRefusesEveryBDCommandTheShimDoesNotAnswer checks the table against bd
// itself, both ways: every command testdata/bd-flags.txt records, and every
// name bd reads as one, is answered by Main or has a refusal row, and every row
// names a verb one of those commands reaches. A bd upgrade that adds a command
// fails here until the shim decides what that command does on a flipped rig.
func TestRefusesEveryBDCommandTheShimDoesNotAnswer(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	beta := filepath.Join(town, "beta", "mayor", "rig")
	prefix := trackerPrefix(t, beta)

	err := coverageError(documentedCommands(t), refusedVerbs, func(command string) classification {
		return classifyCommand(t, beta, prefix, command)
	})
	if err != nil {
		t.Errorf("the shim leaves bd commands unclassified:\n%v", err)
	}
}

// TestCoverageCheckFailsOnAnUnclassifiedCommand drives the failing branch of
// coverageError, not only the passing one: the real command list passes, and
// the same list plus a name no handler, passthrough rule or refusal row
// mentions returns an error naming that name.
func TestCoverageCheckFailsOnAnUnclassifiedCommand(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	beta := filepath.Join(town, "beta", "mayor", "rig")
	prefix := trackerPrefix(t, beta)
	classify := func(command string) classification {
		return classifyCommand(t, beta, prefix, command)
	}
	commands := documentedCommands(t)

	if err := coverageError(commands, refusedVerbs, classify); err != nil {
		t.Fatalf("coverageError on the real command list = %v, want nil", err)
	}

	const invented = "shim-unclassified-probe"
	err := coverageError(append(append([]string{}, commands...), invented), refusedVerbs, classify)
	if err == nil {
		t.Fatalf("coverageError accepted %s, which no handler, passthrough rule or refusal row names", invented)
	}
	if !strings.Contains(err.Error(), invented) {
		t.Errorf("coverageError error = %q, want it to name %s", err, invented)
	}
}

// TestCoverageCheckReportsEveryMismatch drives the remaining branches of
// coverageError: an answered verb a row names, a row no command reaches, a row
// listed twice, and a documented command Main reads as no verb.
func TestCoverageCheckReportsEveryMismatch(t *testing.T) {
	t.Parallel()
	commands := []string{"list", "show"}
	answered := func(string) classification { return classification{verb: "list"} }
	refused := func(string) classification { return classification{verb: "show", refused: true} }

	cases := []struct {
		name     string
		commands []string
		rows     []string
		classify func(string) classification
		want     string
	}{
		{
			name:     "answered verb in the refusal table",
			commands: commands,
			rows:     []string{"list", "show"},
			classify: answered,
			want:     "which a tracker rig answers, and the refusal table lists verb list",
		},
		{
			name:     "refusal row no command reaches",
			commands: commands,
			rows:     []string{"show", "gate"},
			classify: refused,
			want:     "the refusal table lists gate, and no command bd documents reaches that verb",
		},
		{
			name:     "refusal row listed twice",
			commands: commands,
			rows:     []string{"show", "show"},
			classify: refused,
			want:     "the refusal table lists show twice",
		},
		{
			name:     "command Main names no verb for",
			commands: commands,
			rows:     []string{"show"},
			classify: func(command string) classification {
				if command == "show" {
					return classification{}
				}
				return classification{verb: "list"}
			},
			want: `bd documents "show", and Main dispatches it to no verb`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := coverageError(tc.commands, tc.rows, tc.classify)
			if err == nil {
				t.Fatalf("coverageError = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("coverageError error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
