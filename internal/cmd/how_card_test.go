package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/taskmap"
)

// TestHowCardRulesReadTheFile drives the production loader over the embedded
// card. A blank line, a line over the limit, or a card over the byte budget in
// overseer-card.md makes this test fail with the validator's error, which names
// the file and the line or size.
func TestHowCardRulesReadTheFile(t *testing.T) {
	t.Parallel()

	lines, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("overseerCardRules returned no rule lines")
	}
}

// TestHowCardRulesPinTheFile pins the card's wording. A rule line changed in
// overseer-card.md without rewriting testdata/how/card.golden fails here, so a
// card edit is reviewed as a diff instead of slipping in through the embed.
func TestHowCardRulesPinTheFile(t *testing.T) {
	t.Parallel()

	lines, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	checkHowGolden(t, "card.golden", []byte(strings.Join(lines, "\n")+"\n"))
}

// TestHowCardRulesRefuseBadLines checks the shapes the card must never have and
// the line or size the error names for each, and that a card at each limit is
// accepted.
func TestHowCardRulesRefuseBadLines(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("x", overseerCardMaxLine)
	overLimit := strings.Repeat("x", overseerCardMaxLine+1)

	// atByteBudget is thirteen valid lines and exactly overseerCardMaxBytes, so
	// only the whole-file check decides it; overByteBudget is one byte more.
	// Twelve lines sit at the line limit, which the per-line check accepts.
	atByteBudget := strings.Repeat(atLimit+"\n", 12) + strings.Repeat("y", 88)
	overByteBudget := strings.Repeat(atLimit+"\n", 12) + strings.Repeat("y", 89)
	atByteBudgetLines := make([]string, 0, 13)
	for i := 0; i < 12; i++ {
		atByteBudgetLines = append(atByteBudgetLines, atLimit)
	}
	atByteBudgetLines = append(atByteBudgetLines, strings.Repeat("y", 88))

	tests := []struct {
		name    string
		text    string
		want    []string
		wantErr []string
	}{
		{
			name: "valid lines",
			text: "first\nsecond\n",
			want: []string{"first", "second"},
		},
		{
			name: "the last line needs no newline",
			text: "first\nsecond",
			want: []string{"first", "second"},
		},
		{
			name: "a line at the limit is accepted",
			text: atLimit,
			want: []string{atLimit},
		},
		{
			name: "a card at the byte budget is accepted",
			text: atByteBudget,
			want: atByteBudgetLines,
		},
		{
			name:    "a card over the byte budget",
			text:    overByteBudget,
			wantErr: []string{"2501 bytes", "more than the 2500-byte card budget"},
		},
		{
			name:    "an empty line",
			text:    "first\n\nthird\n",
			wantErr: []string{"line 2", "empty line"},
		},
		{
			name:    "a whitespace-only line",
			text:    "first\n  \nthird\n",
			wantErr: []string{"line 2", "empty line"},
		},
		{
			name:    "a blank line after the last newline",
			text:    "first\n\n",
			wantErr: []string{"line 2", "empty line"},
		},
		{
			name:    "an empty file",
			text:    "",
			wantErr: []string{"line 1", "empty line"},
		},
		{
			name:    "a line over the limit",
			text:    "first\nsecond\n" + overLimit + "\n",
			wantErr: []string{"line 3", "201 characters, more than 200"},
		},
		{
			name:    "the first bad line is the one named",
			text:    "first\n\n" + overLimit + "\n",
			wantErr: []string{"line 2", "empty line"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lines, err := parseOverseerCard(tc.text)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("parseOverseerCard accepted the card and returned %q", lines)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("parseOverseerCard error %q does not name %q", err, want)
					}
				}
				if lines != nil {
					t.Errorf("parseOverseerCard returned %q beside its error", lines)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOverseerCard: %v", err)
			}
			if len(lines) != len(tc.want) {
				t.Fatalf("parseOverseerCard returned %d lines, want %d: %q", len(lines), len(tc.want), lines)
			}
			for i := range tc.want {
				if lines[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i+1, lines[i], tc.want[i])
				}
			}
		})
	}
}

// TestHowCardContent drives the card over the real map and the embedded rule
// file: the title line and the lookup rule come first, every group appears with
// its one-line description, every rule line appears, and the whole card is
// under the size limit.
func TestHowCardContent(t *testing.T) {
	t.Parallel()

	card, err := howCardText(howDefault{taskmap.Default()}, overseerCardRules)
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(card, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("howCardText returned %d lines, want at least the title and the lookup rule", len(lines))
	}
	if lines[0] != overseerCardTitle {
		t.Errorf("the card's first line = %q, want the title %q", lines[0], overseerCardTitle)
	}
	if lines[1] != overseerCardLookupRule {
		t.Errorf("the card's second line = %q, want the lookup rule %q", lines[1], overseerCardLookupRule)
	}

	have := make(map[string]bool, len(lines))
	for _, line := range lines {
		have[line] = true
	}

	var groupLines strings.Builder
	if err := writeHowGroups(&groupLines, howDefault{taskmap.Default()}); err != nil {
		t.Fatalf("writeHowGroups: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(groupLines.String(), "\n"), "\n") {
		if !have[line] {
			t.Errorf("the card does not carry group line %q", line)
		}
	}

	rules, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	for i, rule := range rules {
		if !have[rule] {
			t.Errorf("the card does not carry rule line %d %q", i+1, rule)
		}
	}

	if n := len(card); n >= overseerCardPrintMaxBytes {
		t.Errorf("the card is %d bytes, not under %d", n, overseerCardPrintMaxBytes)
	}
}

// TestHowCardCommandRunsTheCard drives runHowCmd with --card set on the
// command it reads, so deleting the flag branch in runHowCmd fails here even
// though runHowCard still builds a card. The test command carries its own flag
// set, so it runs beside the listing tests without touching howCmd's flag.
func TestHowCardCommandRunsTheCard(t *testing.T) {
	t.Parallel()

	if howCmd.Flags().Lookup("card") == nil {
		t.Fatal("gt how registers no --card flag")
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.Flags().Bool("card", true, "")
	cmd.Flags().Bool("hook-json", false, "")
	cmd.SetOut(&out)
	if err := runHowCmd(cmd, nil); err != nil {
		t.Fatalf("runHowCmd with --card: %v", err)
	}

	want, err := howCardText(howDefault{taskmap.Default()}, overseerCardRules)
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}
	if out.String() != want {
		t.Errorf("gt how --card wrote %q, want the card %q", out.String(), want)
	}
	if !strings.HasPrefix(out.String(), overseerCardTitle+"\n") {
		t.Errorf("gt how --card wrote %q, want it to start with the title line", out.String())
	}
}

// TestHowCardCommandRefusesACommandWithoutTheFlag checks runHowCmd fails closed
// when the command it is given carries no --card flag: an unreadable flag is an
// error, not a listing printed as if the card had not been asked for.
func TestHowCardCommandRefusesACommandWithoutTheFlag(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runHowCmd(cmd, nil)
	if err == nil {
		t.Fatal("runHowCmd ran without a --card flag, want an error")
	}
	if !strings.Contains(err.Error(), "card") {
		t.Errorf("runHowCmd error %q does not name the flag", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowCmd wrote %q before refusing", out.String())
	}
}

// TestHowCardRefusesAnArgument checks that --card refuses a group or word
// rather than printing the card as if the argument had chosen part of it.
func TestHowCardRefusesAnArgument(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := runHowCard(&out, howFixture(), []string{"roll"})
	if err == nil {
		t.Fatal("runHowCard accepted an argument, want an error")
	}
	if !strings.Contains(err.Error(), "roll") {
		t.Errorf("runHowCard error %q does not name the argument", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowCard wrote %q before refusing", out.String())
	}
}

// TestHowCardRefusesACardOverTheLimit checks the size bound: a card over
// overseerCardPrintMaxBytes is an error naming the size, and no partial card
// reaches the writer.
func TestHowCardRefusesACardOverTheLimit(t *testing.T) {
	t.Parallel()

	m := howFixture()
	m.groups = taskmap.Groups()
	long := strings.Repeat("x", 250)
	m.describe = func(taskmap.Group) string { return long }

	var out bytes.Buffer
	err := runHowCard(&out, m, nil)
	if err == nil {
		t.Fatal("runHowCard printed a card over the limit, want an error")
	}
	for _, want := range []string{fmt.Sprint(overseerCardPrintMaxBytes), "not under"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runHowCard error %q does not name %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("runHowCard wrote %q before refusing", out.String())
	}
}

// TestHowCardRefusesACardAtTheLimit pins the bound's edge: a card of exactly
// overseerCardPrintMaxBytes is refused, and one byte under it is printed and
// measured, so "under" keeps its strict meaning.
func TestHowCardRefusesACardAtTheLimit(t *testing.T) {
	t.Parallel()

	rules, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	// base is the card's size with a one-character group description: the title
	// and lookup rule lines, a blank line, the one group's line, a blank line,
	// then every rule line.
	const group, placeholder = "g", "x"
	base := len(overseerCardTitle) + 1 + len(overseerCardLookupRule) + 1 + 1 +
		len(group) + 2 + len(placeholder) + 1 + 1
	for _, rule := range rules {
		base += len(rule) + 1
	}

	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "one byte under the limit is printed", size: overseerCardPrintMaxBytes - 1},
		{name: "exactly at the limit is refused", size: overseerCardPrintMaxBytes, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := howFixture()
			m.groups = []taskmap.Group{group}
			desc := strings.Repeat("y", tc.size-base+1)
			m.describe = func(taskmap.Group) string { return desc }

			var out bytes.Buffer
			err := runHowCard(&out, m, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("runHowCard printed a %d-byte card, want an error", tc.size)
				}
				if out.Len() != 0 {
					t.Errorf("runHowCard wrote %q before refusing", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("runHowCard refused a %d-byte card: %v", tc.size, err)
			}
			if got := out.Len(); got != tc.size {
				t.Errorf("the card is %d bytes, want %d", got, tc.size)
			}
		})
	}
}

// TestHowCardRefusesAGroupWithNoDescription checks the card refuses a group
// description of "" (writeHowGroups' error) rather than print a group line with
// nothing after the name, and that no partial card reaches the writer.
func TestHowCardRefusesAGroupWithNoDescription(t *testing.T) {
	t.Parallel()

	m := howFixture()
	m.describe = func(taskmap.Group) string { return "" }

	var out bytes.Buffer
	err := runHowCard(&out, m, nil)
	if err == nil {
		t.Fatal("runHowCard printed a card for a group with no description, want an error")
	}
	if !strings.Contains(err.Error(), "no description") {
		t.Errorf("runHowCard error %q does not name the missing description", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowCard wrote %q before refusing", out.String())
	}
}

// TestHowCardReturnsALoaderError checks the card refuses to print when the rule
// loader fails, and returns the loader's error rather than an empty card.
func TestHowCardReturnsALoaderError(t *testing.T) {
	t.Parallel()

	loadErr := errors.New("the rule file could not be read")
	card, err := howCardText(howFixture(), func() ([]string, error) {
		return nil, loadErr
	})
	if !errors.Is(err, loadErr) {
		t.Fatalf("howCardText error = %v, want the loader's %v", err, loadErr)
	}
	if card != "" {
		t.Errorf("howCardText returned %q with the loader's error, want no card", card)
	}
}

// TestHowCardIsNotARoleDirective checks the card reaches an agent through gt
// how --card only: gt directive list, run here over a town root the test
// builds, reports a directives/overseer-card.md as UNUSED, because "overseer-card"
// is no role name, while a role-named file lists as ok. The card itself is
// embedded from howdata, not installed from directives/.
func TestHowCardIsNotARoleDirective(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	writeDirective(t, filepath.Join(townRoot, "directives", "polecat.md"), "polecat policy")
	writeDirective(t, filepath.Join(townRoot, "directives", "overseer-card.md"), "a card dropped into directives")

	files, err := listableDirectiveFiles(townRoot)
	if err != nil {
		t.Fatalf("listableDirectiveFiles: %v", err)
	}
	var out bytes.Buffer
	renderDirectiveList(&out, files)

	rowFor := func(name string) string {
		suffix := filepath.Join("directives", name+".md")
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.HasSuffix(strings.TrimSpace(line), suffix) {
				return line
			}
		}
		t.Fatalf("gt directive list has no row for %s:\n%s", suffix, out.String())
		return ""
	}
	if row := rowFor("overseer-card"); !strings.Contains(row, "UNUSED") {
		t.Errorf("gt directive list does not report the card as reaching no role: %q", row)
	}
	if row := rowFor("polecat"); strings.Contains(row, "UNUSED") {
		t.Errorf("gt directive list flags a role directive as unused: %q", row)
	}
}

// howHookEnv returns a getenv over env: a key the map does not name reads "",
// as an unset variable does.
func howHookEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// TestHowCardHookJSON drives the context decision: the overseer, named by either
// identity value, reads the card and the re-read line, while a bird, a polecat
// and a session with no identity read the re-read line alone.
func TestHowCardHookJSON(t *testing.T) {
	t.Parallel()

	card, err := howCardText(howDefault{taskmap.Default()}, overseerCardRules)
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}
	overseerText := card + howHookJSONRestoreText + "\n"
	otherText := howHookJSONRestoreText + "\n"

	tests := []struct {
		name      string
		actor     string
		agentName string
		want      string
		wantCard  bool
	}{
		{
			name:     "the actor names the overseer",
			actor:    overseerActor,
			want:     overseerText,
			wantCard: true,
		},
		{
			name:      "the agent name names the overseer",
			agentName: overseerAgentName,
			want:      overseerText,
			wantCard:  true,
		},
		{
			name:      "both identity values name the overseer",
			actor:     overseerActor,
			agentName: overseerAgentName,
			want:      overseerText,
			wantCard:  true,
		},
		{
			name:     "a padded actor still names the overseer",
			actor:    "  " + overseerActor + " ",
			want:     overseerText,
			wantCard: true,
		},
		{
			name:      "a bird reads the re-read line alone",
			actor:     "town/crew/eagle",
			agentName: "eagle",
			want:      otherText,
		},
		{
			name:  "a polecat reads the re-read line alone",
			actor: "gastown/polecats/valkyrie",
			want:  otherText,
		},
		{
			name: "no identity reads the re-read line alone",
			want: otherText,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := howHookJSONContext(tc.actor, tc.agentName, howDefault{taskmap.Default()}, overseerCardRules)
			if err != nil {
				t.Fatalf("howHookJSONContext(%q, %q): %v", tc.actor, tc.agentName, err)
			}
			if got != tc.want {
				t.Errorf("howHookJSONContext(%q, %q) = %q, want %q", tc.actor, tc.agentName, got, tc.want)
			}
			if hasCard := strings.HasPrefix(got, overseerCardTitle+"\n"); hasCard != tc.wantCard {
				t.Errorf("howHookJSONContext(%q, %q) carries the card = %v, want %v", tc.actor, tc.agentName, hasCard, tc.wantCard)
			}
			if tc.wantCard && !strings.HasSuffix(got, howHookJSONRestoreText+"\n") {
				t.Errorf("howHookJSONContext(%q, %q) does not end with the restore line: %q", tc.actor, tc.agentName, got)
			}
		})
	}
}

// TestHowCardHookJSONReturnsALoaderError checks a card that cannot be built
// fails the hook output rather than sending the session text without its card.
func TestHowCardHookJSONReturnsALoaderError(t *testing.T) {
	t.Parallel()

	loadErr := errors.New("the rule file could not be read")
	context, err := howHookJSONContext(overseerActor, "", howFixture(), func() ([]string, error) {
		return nil, loadErr
	})
	if !errors.Is(err, loadErr) {
		t.Fatalf("howHookJSONContext error = %v, want the loader's %v", err, loadErr)
	}
	if context != "" {
		t.Errorf("howHookJSONContext returned %q with the loader's error, want no context", context)
	}
}

// TestHowCardHookJSONIdentityFromEnv checks the two identity values read from
// the environment: BEADS_ACTOR, then BD_ACTOR for a spawned crew session, and
// CLAUDE_AGENT_NAME for a bird.
func TestHowCardHookJSONIdentityFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		env       map[string]string
		wantActor string
		wantAgent string
	}{
		{
			name:      "BEADS_ACTOR is the actor",
			env:       map[string]string{"BEADS_ACTOR": overseerActor},
			wantActor: overseerActor,
		},
		{
			name:      "BD_ACTOR is the actor when BEADS_ACTOR is unset",
			env:       map[string]string{"BD_ACTOR": "town/crew/overseer"},
			wantActor: "town/crew/overseer",
		},
		{
			name:      "BEADS_ACTOR leads BD_ACTOR",
			env:       map[string]string{"BEADS_ACTOR": "town/crew/dunlin", "BD_ACTOR": "town/crew/overseer"},
			wantActor: "town/crew/dunlin",
		},
		{
			name:      "a blank BEADS_ACTOR falls to BD_ACTOR",
			env:       map[string]string{"BEADS_ACTOR": "  ", "BD_ACTOR": "town/crew/overseer"},
			wantActor: "town/crew/overseer",
		},
		{
			name:      "the agent name comes from CLAUDE_AGENT_NAME",
			env:       map[string]string{"CLAUDE_AGENT_NAME": "eagle"},
			wantAgent: "eagle",
		},
		{
			name: "no variable set names no identity",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			actor, agent := howHookIdentity(howHookEnv(tc.env))
			if actor != tc.wantActor {
				t.Errorf("actor = %q, want %q", actor, tc.wantActor)
			}
			if agent != tc.wantAgent {
				t.Errorf("agent name = %q, want %q", agent, tc.wantAgent)
			}
		})
	}
}

// TestHowCardHookJSONRefusesAnArgument checks --hook-json refuses a group or
// word rather than answer as if the argument had chosen part of the context,
// and that nothing reaches the writer.
func TestHowCardHookJSONRefusesAnArgument(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := runHowHookJSON(&out, howFixture(), howHookEnv(nil), overseerCardRules, []string{"roll"})
	if err == nil {
		t.Fatal("runHowHookJSON accepted an argument, want an error")
	}
	if !strings.Contains(err.Error(), "roll") {
		t.Errorf("runHowHookJSON error %q does not name the argument", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowHookJSON wrote %q before refusing", out.String())
	}
}

// TestHowCardHookJSONReturnsAWriteError checks the writer's error reaches the
// caller, so a hook whose output could not be delivered does not report success.
func TestHowCardHookJSONReturnsAWriteError(t *testing.T) {
	t.Parallel()

	if err := runHowHookJSON(failingHowWriter{}, howFixture(), howHookEnv(nil), overseerCardRules, nil); err == nil {
		t.Fatal("runHowHookJSON swallowed the writer's error")
	}
}

// TestHowCardHookJSONWritesTheSessionStartOutput checks the wire form: one
// SessionStart JSON document whose additionalContext is the decided text.
func TestHowCardHookJSONWritesTheSessionStartOutput(t *testing.T) {
	t.Parallel()

	card, err := howCardText(howDefault{taskmap.Default()}, overseerCardRules)
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "the overseer",
			env:  map[string]string{"BEADS_ACTOR": overseerActor},
			want: card + howHookJSONRestoreText + "\n",
		},
		{
			name: "a bird",
			env:  map[string]string{"BEADS_ACTOR": "town/crew/eagle", "CLAUDE_AGENT_NAME": "eagle"},
			want: howHookJSONRestoreText + "\n",
		},
		{
			name: "no identity",
			want: howHookJSONRestoreText + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			if err := runHowHookJSON(&out, howDefault{taskmap.Default()}, howHookEnv(tc.env), overseerCardRules, nil); err != nil {
				t.Fatalf("runHowHookJSON: %v", err)
			}
			var got howHookJSONOutput
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("runHowHookJSON wrote %q, want one JSON document: %v", out.String(), err)
			}
			if got.HookSpecificOutput.HookEventName != howHookJSONEventName {
				t.Errorf("hookEventName = %q, want %q", got.HookSpecificOutput.HookEventName, howHookJSONEventName)
			}
			if got.HookSpecificOutput.AdditionalContext != tc.want {
				t.Errorf("additionalContext = %q, want %q", got.HookSpecificOutput.AdditionalContext, tc.want)
			}
		})
	}
}

// TestHowCardHookJSONRestoreLineIsTheRepositorysOwn pins the exact line a
// session that is not the overseer reads at a session start: the re-read and
// hand-off instruction, with no claim that the context was compacted, because
// the same hook answers startup, resume and clear as well as compact. The line
// lives in this repository, so a change to it fails here and is reviewed as a
// diff (gt-y1juz1).
func TestHowCardHookJSONRestoreLineIsTheRepositorysOwn(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := runHowHookJSON(&out, howFixture(), howHookEnv(nil), overseerCardRules, nil); err != nil {
		t.Fatalf("runHowHookJSON: %v", err)
	}
	var got howHookJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("runHowHookJSON wrote %q, want one JSON document: %v", out.String(), err)
	}
	if want := "Re-read /gastown, then /handoff gt-overseer.\n"; got.HookSpecificOutput.AdditionalContext != want {
		t.Errorf("additionalContext = %q, want %q", got.HookSpecificOutput.AdditionalContext, want)
	}
}

// TestHowCardHookJSONCommandRunsTheHookJSON drives the command's flag branch
// with an environment of the test's own, so each row decides the branch itself
// and asserts the exact text: deleting the --hook-json branch fails here
// because the card listing it would print is not one JSON document (gt-y1juz1).
func TestHowCardHookJSONCommandRunsTheHookJSON(t *testing.T) {
	t.Parallel()

	if howCmd.Flags().Lookup("hook-json") == nil {
		t.Fatal("gt how registers no --hook-json flag")
	}

	card, err := howCardText(howDefault{taskmap.Default()}, overseerCardRules)
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "the overseer's actor",
			env:  map[string]string{"BEADS_ACTOR": overseerActor},
			want: card + howHookJSONRestoreText + "\n",
		},
		{
			name: "the overseer's agent name",
			env:  map[string]string{"CLAUDE_AGENT_NAME": overseerAgentName},
			want: card + howHookJSONRestoreText + "\n",
		},
		{
			name: "a bird",
			env:  map[string]string{"BEADS_ACTOR": "town/crew/eagle", "CLAUDE_AGENT_NAME": "eagle"},
			want: howHookJSONRestoreText + "\n",
		},
		{
			name: "a polecat",
			env:  map[string]string{"BEADS_ACTOR": "gastown/polecats/valkyrie"},
			want: howHookJSONRestoreText + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.Flags().Bool("card", true, "")
			cmd.Flags().Bool("hook-json", true, "")
			cmd.SetOut(&out)
			if err := runHowCmdWithEnv(cmd, nil, howHookEnv(tc.env)); err != nil {
				t.Fatalf("gt how --hook-json: %v", err)
			}

			var got howHookJSONOutput
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("gt how --hook-json wrote %q, want one JSON document: %v", out.String(), err)
			}
			if got.HookSpecificOutput.HookEventName != howHookJSONEventName {
				t.Errorf("hookEventName = %q, want %q", got.HookSpecificOutput.HookEventName, howHookJSONEventName)
			}
			if got.HookSpecificOutput.AdditionalContext != tc.want {
				t.Errorf("additionalContext = %q, want %q", got.HookSpecificOutput.AdditionalContext, tc.want)
			}
		})
	}
}

// TestHowCardHookJSONCommandRefusesACommandWithoutTheFlag checks runHowCmd fails
// closed when the command carries --card but no --hook-json: an unreadable flag
// must not fall through to a listing.
func TestHowCardHookJSONCommandRefusesACommandWithoutTheFlag(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.Flags().Bool("card", false, "")
	cmd.SetOut(&out)
	err := runHowCmd(cmd, nil)
	if err == nil {
		t.Fatal("runHowCmd ran without a --hook-json flag, want an error")
	}
	if !strings.Contains(err.Error(), "hook-json") {
		t.Errorf("runHowCmd error %q does not name the flag", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowCmd wrote %q before refusing", out.String())
	}
}

// TestHowCardHookJSONReturnsALoaderErrorThroughTheCommand checks the command's
// entry point hands a failed card loader back to the caller, so no text without
// the card reaches the hook.
func TestHowCardHookJSONReturnsALoaderErrorThroughTheCommand(t *testing.T) {
	t.Parallel()

	loadErr := errors.New("the rule file could not be read")
	var out bytes.Buffer
	err := runHowHookJSON(&out, howFixture(), howHookEnv(map[string]string{"BEADS_ACTOR": overseerActor}), func() ([]string, error) {
		return nil, loadErr
	}, nil)
	if !errors.Is(err, loadErr) {
		t.Fatalf("runHowHookJSON error = %v, want the loader's %v", err, loadErr)
	}
	if out.Len() != 0 {
		t.Errorf("runHowHookJSON wrote %q with the loader's error, want nothing", out.String())
	}
}
