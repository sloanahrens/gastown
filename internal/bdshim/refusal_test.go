package bdshim

import (
	"path/filepath"
	"strings"
	"testing"
)

// refusedVerbs are the verbs a tracker rig refuses by name: Main looks the
// handler registry up by the raw first word of the command (verbOf), so a name
// bd reads as an alias is listed here unless the registry answers that name
// itself. Every verb here exits 1 with Refusal's message and never reaches
// bd.real. This table is the per-verb decision record; the coverage test
// checks it against testdata/bd-flags.txt both ways, so the next command bd
// gains fails the suite until this table or the handler registry classifies it.
var refusedVerbs = []string{
	"admin",
	"assign",
	"audit",
	"backup",
	"batch",
	"branch",
	"compact",
	"conflicts",
	"create-form",
	"diff",
	"doctor",
	"dolt",
	"done",
	"duplicate",
	"epic",
	"export",
	"find-dups",
	"find-duplicates",
	"flatten",
	"forget",
	"gate",
	"gc",
	"graph",
	"hb",
	"heartbeat",
	"help",
	"history",
	"hooks",
	"human",
	"import",
	"init",
	"land-record",
	"link",
	"lint",
	"mail",
	"memories",
	"merge-slot",
	"migrate",
	"mol",
	"new",
	"note",
	"orphans",
	"preflight",
	"prime",
	"priority",
	"promote",
	"protomolecule",
	"provenance",
	"prune",
	"purge",
	"q",
	"recall",
	"reclaim",
	"remember",
	"rename",
	"rename-prefix",
	"repo",
	"restore",
	"rules",
	"serve",
	"set-state",
	"setup",
	"ship",
	"sql",
	"stale",
	"state",
	"status",
	"supersede",
	"swarm",
	"sync",
	"tag",
	"todo",
	"vc",
	"view",
	"worktree",
}

// pinnedRefusals are the refusal messages a bead fixes word for word. Every
// other row is checked against the shape every refusal shares, built by the
// test from the verb alone, so a change to Refusal cannot move both sides of
// the comparison together.
var pinnedRefusals = map[string]string{
	// R9 section 0 keeps bd sql refused for good: the direct-Dolt consumers
	// treat a database read as fatal, so a tracker rig has to fail the command
	// loudly instead of answering it.
	"sql": "bdshim: verb sql is not implemented for tracker rig bt-",
}

// dispatchVerb is the verb Main hands the handler registry for a command: the
// raw first operand, as verbOf reads it. An alias is not resolved here.
func dispatchVerb(command string) string {
	return verbOf(strings.Fields(command))
}

// answeredByShim reports whether a tracker rig answers command instead of
// refusing it, through the two checks Main makes: commandPath resolves aliases
// for the bd.real passthrough, and the registry is keyed on verbOf's raw word.
func answeredByShim(command string) bool {
	args := strings.Fields(command)
	return passesThrough(commandPath(args)) || handlers[verbOf(args)] != nil
}

// TestRefusesEveryUnimplementedVerb drives the production entry point from a
// tracker rig directory once per verb: each is refused by name, exits 1, and
// never runs bd.real in its place.
func TestRefusesEveryUnimplementedVerb(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	beta := filepath.Join(town, "beta", "mayor", "rig")

	for _, verb := range refusedVerbs {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			proc := &fakeProcess{}
			e, stderr, _ := shimEnv(t, beta, "", proc, handlers, verb)

			if got := Main(e); got != 1 {
				t.Errorf("Main(%s) = %d, want 1", verb, got)
			}
			want := "bdshim: verb " + verb + " is not implemented for tracker rig bt-\n"
			if pinned, ok := pinnedRefusals[verb]; ok {
				want = pinned + "\n"
			}
			if got := stderr.String(); got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
			if proc.ran != nil {
				t.Errorf("Main(%s) ran bd.real with %v, want the verb refused before it", verb, proc.ran)
			}
		})
	}
}

// TestRefusesEveryBDCommandTheShimDoesNotAnswer checks the table against bd
// itself, both ways: every command testdata/bd-flags.txt records, and every
// name bd reads as one, is answered or has a refusal row, and every row names
// a verb one of those commands reaches. A bd upgrade that adds a command fails
// here until the shim decides what that command does on a flipped rig.
func TestRefusesEveryBDCommandTheShimDoesNotAnswer(t *testing.T) {
	t.Parallel()
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
	// An alias name and the command it stands for are both words a caller can
	// type, so both need a classification even when bd's help records no flag
	// row for the target: status is reached here only as stats's alias target,
	// and the registry answers stats rather than status.
	for name, command := range aliased {
		commands[name] = true
		commands[command] = true
	}
	// The checks below walk maps, so a recording that parsed to nothing would
	// pass them and leave the table looking complete.
	if len(commands) < 100 {
		t.Fatalf("testdata/bd-flags.txt names %d commands, want at least 100", len(commands))
	}

	refused := make(map[string]bool, len(refusedVerbs))
	for _, verb := range refusedVerbs {
		if refused[verb] {
			t.Errorf("refusedVerbs lists %s twice", verb)
		}
		refused[verb] = true
	}

	documentedVerbs := map[string]bool{}
	for command := range commands {
		verb := dispatchVerb(command)
		if verb == "" {
			t.Errorf("testdata/bd-flags.txt records %q, whose command path is empty", command)
			continue
		}
		documentedVerbs[verb] = true
		answered := answeredByShim(command)
		if answered && refused[verb] {
			t.Errorf("bd documents %q, which a tracker rig answers, and refusedVerbs lists verb %s", command, verb)
		}
		if !answered && !refused[verb] {
			t.Errorf("bd documents %q, and verb %s has no handler, no passthrough rule and no refusal row", command, verb)
		}
	}

	for _, verb := range refusedVerbs {
		if !documentedVerbs[verb] {
			t.Errorf("refusedVerbs lists %s, and no command bd documents reaches that verb", verb)
		}
	}
}
