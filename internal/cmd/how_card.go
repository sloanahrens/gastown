package cmd

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// how_card.go holds the overseer card: the rule file and its loader
// (gt-j669b4.8), the --card flag that prints it (gt-4n47gt), and the
// --hook-json flag that gives a session its card and the re-read line, or the
// re-read line alone, at session start (gt-ec569g).

// overseerCardFile holds the overseer card's rule lines, one per line and no
// headings: what a just-compacted overseer reads before it acts (gt-j669b4.8).
// The first lines are the design's first set (section 5), given verbatim by the
// overseer on that bead; the last three are the operator's standing rules
// (gt-0rix8g). TestHowCardRulesPinTheFile fails when a line changes, so an edit
// to the card is reviewed as a diff.
//
// It sits under howdata beside this package's code so the card is reviewed like
// code and deployed with the binary. It is not a file under directives/, which
// internal/config/directives.go lists as role directives and
// directives_deploy.go installs.
//
//go:embed howdata/overseer-card.md
var overseerCardFile string

const (
	// overseerCardPath names the embedded card in a validation error, so a
	// failing test points at the file to fix.
	overseerCardPath = "internal/cmd/howdata/overseer-card.md"
	// overseerCardMaxLine is the most characters one rule line may carry.
	overseerCardMaxLine = 200
	// overseerCardMaxBytes is the card's whole-file budget. The per-line
	// validator passes every line of a card far over this, so only a whole-file
	// check stops the card growing past what a compacted session will read
	// (gt-0rix8g).
	overseerCardMaxBytes = 2500
	// overseerCardTitle is the printed card's first line.
	overseerCardTitle = "Overseer card"
	// overseerCardLookupRule is the printed card's second line: how a task is
	// looked up before an operator step.
	overseerCardLookupRule = "before a roll, a resize, a release, a hand sling or any operator step: gt how <task> --history"
	// overseerCardPrintMaxBytes is the most the printed card may weigh. The
	// rule file has its own budget (overseerCardMaxBytes); this one bounds what
	// gt how --card writes, title, lookup rule and group lines included,
	// because that text is what reaches an agent's context.
	overseerCardPrintMaxBytes = 2500

	// overseerActor and overseerAgentName are the two identity values that mark
	// the overseer session: the actor beads records for it, and the agent name
	// gt bird start sets. Either one is enough (gt-ec569g).
	overseerActor     = "town/crew/overseer"
	overseerAgentName = "overseer"

	// howHookActorEnv and howHookAgentEnv are the environment the session's
	// identities are read from: BEADS_ACTOR is the identity bd attributes to,
	// BD_ACTOR is what a spawned crew session exports, and CLAUDE_AGENT_NAME is
	// the name gt bird start sets (internal/bird/plan.go).
	howHookActorEnv     = "BEADS_ACTOR"
	howHookCrewActorEnv = "BD_ACTOR"
	howHookAgentEnv     = "CLAUDE_AGENT_NAME"

	// howHookJSONEventName is the hook this output answers: a session start.
	howHookJSONEventName = "SessionStart"

	// howHookJSONRestoreText is the line a session start reads after the
	// overseer's card, or alone for every other session: re-read the town's
	// directives, then hand off. This repository owns the line; it is not a copy
	// of a file outside it, so a change to the instruction is reviewed as a diff
	// here and no outside file can drift from it.
	//
	// It makes no claim that the context was compacted. This hook answers every
	// SessionStart — startup, resume, clear and compact alike — so a line saying
	// the context was just compacted would be false for most of them
	// (gt-y1juz1). TestHowCardHookJSON pins what a session reads.
	howHookJSONRestoreText = "Re-read /gastown, then /handoff gt-overseer."
)

func init() {
	howCmd.Flags().Bool("card", false, "print the overseer card: the lookup rule, the task groups and the rule lines")
	howCmd.Flags().Bool("hook-json", false, "print the SessionStart hook JSON: the overseer's card and the re-read line, or the re-read line alone")
}

// overseerCardRules returns the card's rule lines, in file order. howCardText,
// which gt how --card prints, reads the card through it. A card that does not
// validate is an error, never an empty list.
func overseerCardRules() ([]string, error) {
	return parseOverseerCard(overseerCardFile)
}

// parseOverseerCard splits text into rule lines and refuses a card that would
// read as broken: an empty line, a line over overseerCardMaxLine characters, or
// a card over overseerCardMaxBytes. The error names the file and, for a line,
// the 1-based number, so a test that trips it names what to fix, and it returns
// no lines, so a caller cannot print a card that failed validation.
func parseOverseerCard(text string) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		n := i + 1
		if strings.TrimSpace(line) == "" {
			return nil, fmt.Errorf("%s: line %d: empty line", overseerCardPath, n)
		}
		if c := utf8.RuneCountInString(line); c > overseerCardMaxLine {
			return nil, fmt.Errorf("%s: line %d: %d characters, more than %d", overseerCardPath, n, c, overseerCardMaxLine)
		}
	}
	if n := len(text); n > overseerCardMaxBytes {
		return nil, fmt.Errorf("%s: %d bytes, more than the %d-byte card budget", overseerCardPath, n, overseerCardMaxBytes)
	}
	return lines, nil
}

// runHowCard writes the overseer card to out. It refuses an argument: the card
// is one fixed text, and a group or word after --card would read as if it chose
// part of the card.
func runHowCard(out io.Writer, m howMap, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("gt how --card takes no group or word, got %q", strings.Join(args, " "))
	}
	card, err := howCardText(m, overseerCardRules)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, card)
	return err
}

// howCardText builds the card in one string: the title line, the lookup rule,
// every task group with its one-line description, then the rule lines. loadRules
// supplies the rule lines, so the command passes overseerCardRules and a test
// can pass a loader that fails. A group with no description (writeHowGroups), a
// failed loader, or a card over overseerCardPrintMaxBytes is an error: a card
// that cannot be printed whole is not printed at all, so a failure writes
// nothing to out.
func howCardText(m howMap, loadRules func() ([]string, error)) (string, error) {
	rules, err := loadRules()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n", overseerCardTitle, overseerCardLookupRule)
	if err := writeHowGroups(&b, m); err != nil {
		return "", err
	}
	b.WriteString("\n")
	for _, line := range rules {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if n := b.Len(); n >= overseerCardPrintMaxBytes {
		return "", fmt.Errorf("the overseer card is %d bytes, not under %d: trim the rule file or a group description", n, overseerCardPrintMaxBytes)
	}
	return b.String(), nil
}

// howHookJSONOutput is the SessionStart hook output Claude Code reads: it adds
// hookSpecificOutput.additionalContext to the model's context.
type howHookJSONOutput struct {
	HookSpecificOutput howHookJSONSpecificOutput `json:"hookSpecificOutput"`
}

type howHookJSONSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// runHowHookJSON writes the SessionStart hook JSON for the calling session: the
// card and the re-read line for the overseer, the re-read line alone for any
// other session. getenv is the process environment, so a test passes its own
// and the branch is decided by the arguments, not from a hidden read of the
// live environment.
func runHowHookJSON(out io.Writer, m howMap, getenv func(string) string, loadRules func() ([]string, error), args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("gt how --hook-json takes no group or word, got %q", strings.Join(args, " "))
	}
	actor, agentName := howHookIdentity(getenv)
	context, err := howHookJSONContext(actor, agentName, m, loadRules)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(howHookJSONOutput{
		HookSpecificOutput: howHookJSONSpecificOutput{
			HookEventName:     howHookJSONEventName,
			AdditionalContext: context,
		},
	})
}

// howHookIdentity reads the two identity values a session may carry: the actor
// beads records (BEADS_ACTOR, then BD_ACTOR, which is set instead of it on a
// spawned crew session), and the agent name gt bird start sets. A value is
// returned as read; isOverseer trims it before comparing.
func howHookIdentity(getenv func(string) string) (actor, agentName string) {
	actor = getenv(howHookActorEnv)
	if strings.TrimSpace(actor) == "" {
		actor = getenv(howHookCrewActorEnv)
	}
	return actor, getenv(howHookAgentEnv)
}

// isOverseer reports whether either identity value names the overseer session.
// The values are trimmed first: the environment is the session's, not this
// command's, and a padded value must not name a different role.
func isOverseer(actor, agentName string) bool {
	return strings.TrimSpace(actor) == overseerActor || strings.TrimSpace(agentName) == overseerAgentName
}

// howHookJSONContext returns the text the hook adds to a session's context. The
// overseer gets the card and then the re-read line; any other session gets the
// re-read line alone, so it is told to re-read and hand off and nothing more. A
// card that cannot be built is the card's error, never text without the card.
func howHookJSONContext(actor, agentName string, m howMap, loadRules func() ([]string, error)) (string, error) {
	if !isOverseer(actor, agentName) {
		return howHookJSONRestoreText + "\n", nil
	}
	card, err := howCardText(m, loadRules)
	if err != nil {
		return "", err
	}
	return card + howHookJSONRestoreText + "\n", nil
}
