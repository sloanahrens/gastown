package cmd

import (
	"slices"
	"testing"
)

// containerSuiteWrapRoleForTest is the holder the wrap tests print, so the
// expected lines below are the lines a polecat would copy.
const containerSuiteWrapRoleForTest = "gastown/polecats/rictus"

// TestContainerSuiteReadsThroughParsedTokens pins the wrap advice the guard
// prints for a refused command. A command the parsed reading can judge is
// judged on its parsed tokens. A command shellparse cannot read, or one the
// parsed reading could read as simpler than the shell does, is judged on
// shellTokenize, as it was before the move.
func TestContainerSuiteReadsThroughParsedTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "a simple command is wrapped bare",
			command: "go test ./internal/beads/...",
			want:    "gt slot run --role gastown/polecats/rictus -- go test ./internal/beads/...",
		},
		{
			name:    "a leading assignment is wrapped through env",
			command: "GT_TEST_DOCKER=1 go test ./internal/beads/...",
			want:    "gt slot run --role gastown/polecats/rictus -- env GT_TEST_DOCKER=1 go test ./internal/beads/...",
		},
		{
			name:    "a chained command goes to a shell",
			command: "go test ./internal/beads && echo ok",
			want:    "gt slot run --role gastown/polecats/rictus -- sh -c 'go test ./internal/beads && echo ok'",
		},
		{
			name:    "a command shellparse cannot read keeps the shell judgement",
			command: "echo \"unterminated && go test ./internal/beads/...",
			want:    "gt slot run --role gastown/polecats/rictus -- sh -c 'echo \"unterminated && go test ./internal/beads/...'",
		},
		{
			name:    "a heredoc keeps the shell judgement of its line ends",
			command: "bash <<EOF\ngo test ./internal/beads\nEOF",
			want:    "gt slot run --role gastown/polecats/rictus -- sh -c 'bash <<EOF\ngo test ./internal/beads\nEOF'",
		},
		{
			name:    "a brace group keeps the shell judgement of its separator",
			command: "{ go test ./internal/beads; }",
			want:    "gt slot run --role gastown/polecats/rictus -- sh -c '{ go test ./internal/beads; }'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containerSuiteWrap(tt.command, containerSuiteWrapRoleForTest); got != tt.want {
				t.Errorf("containerSuiteWrap(%q)\n got: %s\nwant: %s", tt.command, got, tt.want)
			}
		})
	}
}

// containerSuiteShapes are commands the differential set does not cover well:
// compound forms, substitutions and assignments whose separators or structure
// decide the advice. They are added to the property's command set.
var containerSuiteShapes = []string{
	"{ go test ./internal/beads; }",
	"FOO=1 go test ./internal/beads",
	"bash <<EOF\ngo test ./internal/beads\nEOF",
	"case a in a) make ;; b) go test ;; esac",
	"echo a && echo b",
	"for x in 1; do go test ./internal/beads; done",
	"while true; do go test ./internal/beads; done",
	"if true; then go test ./internal/beads; fi",
	"function f { go test ./internal/beads; }; f",
	"go test ./internal/beads && echo ok",
	"go test ./internal/beads & echo ok",
	"go test ./internal/beads\necho ok",
	"go test ./internal/beads |& cat",
	"GT_TEST_DOCKER=1 go test ./internal/beads/...; echo done",
	"x=1 y=$(echo a) go test ./internal/beads",
	"x=$(echo \")\" ; bash -c \"echo hi\")",
	"[[ -n x ]] && go test ./internal/beads",
	"((1)) && go test ./internal/beads",
	"echo `go test ./internal/beads` && ls",
	"echo <(go test ./internal/beads)",
	"go test ./internal/beads ;",
	"cd internal/beads && make test",
}

// containerSuiteCommands is the property's command set: the differential's
// command source (the guard corpus and the generated commands), this guard's
// own corpus rows, and containerSuiteShapes.
func containerSuiteCommands(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var commands []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			commands = append(commands, c)
		}
	}
	for _, c := range parsedTokensCommands(t) {
		add(c)
	}
	for _, row := range loadGuardCorpus(t) {
		if row.guard == corpusContainerSuite {
			add(row.command)
		}
	}
	for _, c := range containerSuiteShapes {
		add(c)
	}
	return commands
}

// containerSuiteVerdict judges command through the production refuse/allow
// path, in proc, with the session's container opt-in.
func containerSuiteVerdict(proc guardProcess, command string) string {
	reason, _ := evaluateContainerSuiteCommand(proc, command, proc.getenv(dockerTestsEnv))
	return reason
}

// containerSuiteLoosenings returns one failure for each readable command whose
// wrap decisions under read are less strict than shellTokenize's: a compound
// command read as simple, or an assignment prefix read as none. A failure is a
// command the advice would print bare, leaving a second command outside the slot.
func containerSuiteLoosenings(commands []string, read containerSuiteReader) []string {
	var failures []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		want := containerSuiteShapeOf(shellTokenize(c))
		got := containerSuiteShapeOf(read(c))
		if (want.compound && !got.compound) || (want.env && !got.env) {
			failures = append(failures, c)
		}
	}
	return failures
}

// TestContainerSuiteParsedVerdictNeverLoosens proves two halves: the guard's
// refuse/allow verdict on each of its corpus rows is the verdict the row
// records, and the wrap decisions read through parsedTokens are never less
// strict than the shellTokenize reading, over every command the parser reads.
// The first half ties the property to the guard's decision, since the advice is
// printed only after a refusal; the second holds the line an operator copies. A
// reading that drops the separator tokens must fail the second half, and must
// change the printed advice.
func TestContainerSuiteParsedVerdictNeverLoosens(t *testing.T) {
	t.Parallel()
	fixture := newGuardCorpusFixture(t)
	proc := fixture.process(t, corpusPolecat)
	commands := containerSuiteCommands(t)

	refused := 0
	for _, row := range loadGuardCorpus(t) {
		if row.guard != corpusContainerSuite {
			continue
		}
		verdict, reason := fixture.judge(t, row)
		if verdict != row.verdict {
			t.Errorf("container-suite verdict on %q is %s (reason %q), want %s",
				row.command, verdict, reason, row.verdict)
		}
		if verdict == corpusRefuse {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("no container_suite corpus row is refused; the property judged no verdict")
	}

	if failures := containerSuiteLoosenings(commands, containerSuiteReadTokens); len(failures) != 0 {
		for _, f := range failures {
			t.Errorf("parsed reading is less strict than shellTokenize for %q", f)
		}
	}
	t.Logf("%d commands checked, %d container_suite corpus rows refused", len(commands), refused)

	t.Run("dropping the separator handling is caught", func(t *testing.T) {
		t.Parallel()
		dropSeparators := func(c string) []string {
			return slices.DeleteFunc(slices.Clone(containerSuiteReadTokens(c)), func(s string) bool {
				return shellCommandSeparators[s]
			})
		}
		if len(containerSuiteLoosenings(commands, dropSeparators)) == 0 {
			t.Error("a reading without separators passed the property; the property does not hold the separators")
		}
		// The same mutation through the production wrap: the chained command
		// the guard refuses is printed bare instead of under sh -c.
		const command = "echo ok && GT_TEST_DOCKER=1 go test ./internal/beads/..."
		if reason := containerSuiteVerdict(proc, command); reason == "" {
			t.Fatalf("the guard allows %q; the mutation does not exercise a refusal", command)
		}
		const want = "gt slot run --role gastown/polecats/rictus -- sh -c 'echo ok && GT_TEST_DOCKER=1 go test ./internal/beads/...'"
		if got := containerSuiteWrapWith(command, containerSuiteWrapRoleForTest, containerSuiteReadTokens); got != want {
			t.Errorf("production wrap of %q = %s, want %s", command, got, want)
		}
		const bare = "gt slot run --role gastown/polecats/rictus -- echo ok && GT_TEST_DOCKER=1 go test ./internal/beads/..."
		if got := containerSuiteWrapWith(command, containerSuiteWrapRoleForTest, dropSeparators); got != bare {
			t.Errorf("mutated wrap of %q = %s, want the bare line %s", command, got, bare)
		}
	})
}
