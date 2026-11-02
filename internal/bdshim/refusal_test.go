package bdshim

import (
	"path/filepath"
	"strings"
	"testing"
)

// refusedVerbs are the bd verbs a tracker rig has no handler for and does not
// hand to bd.real: Main refuses each by name and exits 1, so a command that
// routes to the tracker cannot fall through to bd.real. Every command bd 1.2.2
// documents in testdata/bd-flags.txt resolves to a verb here, in the handlers
// registry, or in passThroughVerbs; TestRefusesEveryBDCommandTheShimDoesNotAnswer
// fails when bd gains a command this table has no row for.
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
	"duplicate",
	"epic",
	"export",
	"find-duplicates",
	"flatten",
	"forget",
	"gate",
	"gc",
	"graph",
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
	"note",
	"orphans",
	"preflight",
	"prime",
	"priority",
	"promote",
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
// itself: every command testdata/bd-flags.txt records, and every name bd reads
// as one, resolves to a verb a tracker handler answers, a verb passed to
// bd.real by design, or a row of refusedVerbs. A bd upgrade that adds a command
// fails here until the shim decides what that command does on a flipped rig,
// and a row for a verb the shim answers fails as a refusal the runtime never
// makes. An alias is read as the command it names, as commandPath reads it.
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
	// An alias names a command too, and its target is one even when bd's help
	// records no flag row for it: status is reached here only as stats's
	// command.
	for name, command := range aliased {
		commands[name] = true
		commands[command] = true
	}

	refused := make(map[string]bool, len(refusedVerbs))
	for _, verb := range refusedVerbs {
		if refused[verb] {
			t.Errorf("refusedVerbs lists %s twice", verb)
		}
		refused[verb] = true
		if handlers[verb] != nil {
			t.Errorf("refusedVerbs lists %s, and the handler registry answers it", verb)
		}
		if passThroughVerbs[verb] {
			t.Errorf("refusedVerbs lists %s, and passThroughVerbs hands it to bd.real", verb)
		}
	}

	for command := range commands {
		canonical := command
		if to, ok := aliases[canonical]; ok {
			canonical = to
		}
		words := strings.Fields(canonical)
		if len(words) == 0 {
			t.Errorf("testdata/bd-flags.txt records %q, whose command path is empty", command)
			continue
		}
		verb := words[0]
		if handlers[verb] != nil || passThroughVerbs[verb] || refused[verb] {
			continue
		}
		t.Errorf("bd documents %q, and verb %s has no handler, no passthrough rule and no refusal row", command, verb)
	}
}
