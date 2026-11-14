// Verdict-change record for the move to parsedTokens (gt-ppuk8j, reviewed at
// gt-x5gkqj). The guard's production reader and shellTokenize give the same
// verdict for every command in doneSlotLoopCommands, so the move changed no
// verdict on that set: no tightening and no loosening. TestDoneSlotLoopMoveChangesNoVerdict
// pins that equality over the set; TestDoneSlotLoopParsedVerdictNeverLoosens
// asserts the one-way property over the same set.
package cmd

import (
	"slices"
	"testing"
)

// doneSlotLoopShapes are commands this guard's own cases do not cover well:
// compound forms, substitutions and payloads whose structure the parsed reading
// flattens away. They are added to the property's command set.
var doneSlotLoopShapes = []string{
	`for i in $(seq 1 300); do if gt slot status | grep -q free; then gt done; fi; done`,
	"while true; do gt done; sleep 5; done",
	"until gt slot status | grep -q free; do sleep 5; done",
	"repeat 20 { gt done; sleep 60 }",
	"repeat 20 gt done",
	"foreach f (a b c); gt done; end",
	"for f (a b) { gt done }",
	"for f in a b; gt done",
	"for f in a b; { gt done }",
	"(while true; do gt slot status; sleep 5; done) &",
	"{ until gt done; do sleep 5; done; }",
	"case $x in a) true ;; b) for i in 1 2 3; do gt done; done ;; esac",
	"xargs -I{} sh -c 'for i in 1 2; do gt done; done'",
	"watch -n 5 bash -c 'while true; do gt done; done'",
	"eval 'for i in 1 2; do gt done; done'",
	"bash -c 'while true; do gt done; done'",
	"cat > /tmp/retry.sh <<'EOF'\nfor i in $(seq 1 300); do gt done; done\nEOF",
	"tee /tmp/retry.sh <<'EOF'\nwhile true; do gt done; sleep 5; done\nEOF",
	"gt done",
	"while read -r x; do gt done; done < /tmp/beads",
	`for f in $(git diff | head -3); gt done`,
	"gt done && gt done",
	"seq 1 300 | xargs -I{} bash -c 'gt done'",
	"go test ./internal/beads && echo ok",
	"echo ;",
	"echo &",
	// Marker-only shapes: no loop keyword, and the parsed reading alone would
	// miss the second command, so only the marker rules route them home.
	"echo $(seq 1 5 | xargs gt done)",
	"xargs -I{} bash -c 'echo $(seq 1 5 | xargs gt done)'",
	"xargs -I{} bash -c 'bash <<EOF\ngt done\nEOF'",
	"gt done |& xargs gt done",
}

// doneSlotLoopCommands is the property's command set: the differential's command
// source (the guard corpus and the generated commands), this guard's own cases,
// and doneSlotLoopShapes.
func doneSlotLoopCommands(t *testing.T) []string {
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
	for _, tt := range doneSlotLoopCases {
		add(tt.command)
	}
	for _, c := range doneSlotLoopShapes {
		add(c)
	}
	return commands
}

// doneSlotLoopLoosenings returns one entry for each command the parser reads
// whose verdict under read allows a command shellTokenize's reading refuses. A
// command shellTokenize allows and read refuses is a tightening, which this
// guard's move is allowed to add and which is not listed here.
func doneSlotLoopLoosenings(commands []string, read doneSlotLoopReader) []string {
	var failures []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		want, _ := doneSlotLoopVerdictWith(c, shellTokenize)
		got, _ := doneSlotLoopVerdictWith(c, read)
		if want != "" && got == "" {
			failures = append(failures, c)
		}
	}
	return failures
}

// TestDoneSlotLoopReadsThroughParsedTokens pins the verdict of commands the
// guard judges through the production reader, including the two it hands to
// shellTokenize: a command the parser cannot read, and a command whose
// structure the parsed reading flattens away. Each row's verdict is also
// asserted equal to the verdict shellTokenize gives, so the move cannot flip
// one command toward allowing.
func TestDoneSlotLoopReadsThroughParsedTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"a readable payload is judged on its parsed tokens", "seq 1 300 | xargs -I{} bash -c 'gt done'", true},
		{"a readable command that runs no gate stays allowed", "bd show gt-ppuk8j", false},
		{"an unreadable command keeps the text reading of its loop", `for i in 1 2; do gt done; done "`, true},
		{"an unreadable command that runs no gate stays allowed", `echo "unterminated`, false},
		{"an unreadable shell payload keeps the text reading", `seq 1 300 | xargs -I{} bash -c "gt done`, false},
		{"a compound command is routed to the text reading", "while true; do gt done; break; done", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tokens := doneSlotLoopReadTokens(tt.command)
			reason, _ := matchesDoneSlotLoop(tt.command, tokens, doneSlotLoopLower(tokens))
			if got := reason != ""; got != tt.blocked {
				t.Errorf("matchesDoneSlotLoop(%q) blocked=%v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
			shellTokens := shellTokenize(tt.command)
			want, _ := matchesDoneSlotLoop(tt.command, shellTokens, doneSlotLoopLower(shellTokens))
			if (reason != "") != (want != "") {
				t.Errorf("reading through doneSlotLoopReadTokens gives %q where shellTokenize gives %q for %q; the move changed a verdict",
					reason, want, tt.command)
			}
		})
	}
}

// TestDoneSlotLoopProductionReader pins which reading the guard hands its
// matcher: the parsed one for a command the parser reads with no unsafe shape,
// and the text one for a command it cannot read or one whose structure it
// flattens away.
func TestDoneSlotLoopProductionReader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		reading doneSlotLoopReader
	}{
		{
			// A glued redirection: shellTokenize keeps ">f" in one token,
			// parsedTokens splits it. The two readings differ, so the assertion
			// says which one the guard's matcher was handed.
			name:    "a readable command with no unsafe shape is read by the parser",
			command: "echo >f",
			reading: func(c string) []string { toks, _ := parsedTokens(c); return toks },
		},
		{
			name:    "an unreadable command falls back to the text reading",
			command: `echo "unterminated`,
			reading: shellTokenize,
		},
		{
			name:    "a compound command falls back to the text reading",
			command: "for i in 1 2; do gt done; done",
			reading: shellTokenize,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := doneSlotLoopReadTokens(tt.command)
			if want := tt.reading(tt.command); !slices.Equal(got, want) {
				t.Errorf("doneSlotLoopReadTokens(%q) = %q, want %q", tt.command, got, want)
			}
		})
	}
}

// TestDoneSlotLoopMoveChangesNoVerdict pins the verdict-change record in this
// file's header: over doneSlotLoopCommands, the production reader gives every
// command the verdict shellTokenize gives it. A change to the reader that moves
// a verdict fails here, and the record in the header must then be updated.
func TestDoneSlotLoopMoveChangesNoVerdict(t *testing.T) {
	t.Parallel()
	for _, c := range doneSlotLoopCommands(t) {
		want, _ := doneSlotLoopVerdictWith(c, shellTokenize)
		got, _ := doneSlotLoopVerdictWith(c, doneSlotLoopReadTokens)
		if got != want {
			t.Errorf("verdict of %q is %q through the production reader and %q through shellTokenize", c, got, want)
		}
	}
}

// TestDoneSlotLoopParsedVerdictNeverLoosens checks, over the commands in
// doneSlotLoopCommands, that the guard's verdict through parsedTokens is never
// less strict than the shellTokenize verdict. That set is a sample of the
// commands the parser reads, so the check is not a proof over every readable
// command. A command whose parsed reading would allow what shellTokenize refuses
// is routed to shellTokenize by doneSlotLoopParsedUnsafe; that routing rule is
// the argument for the commands outside the sample, and the subtests below
// exercise it only on the sampled set.
func TestDoneSlotLoopParsedVerdictNeverLoosens(t *testing.T) {
	t.Parallel()
	commands := doneSlotLoopCommands(t)
	if failures := doneSlotLoopLoosenings(commands, doneSlotLoopReadTokens); len(failures) != 0 {
		for _, f := range failures {
			t.Errorf("parsed reading allows what shellTokenize refuses: %q", f)
		}
	}
	t.Logf("%d commands checked", len(commands))

	t.Run("the property fails without the fallback", func(t *testing.T) {
		t.Parallel()
		parsedOnly := func(c string) []string {
			toks, readable := parsedTokens(c)
			if !readable {
				return shellTokenize(c)
			}
			return toks
		}
		if len(doneSlotLoopLoosenings(commands, parsedOnly)) == 0 {
			t.Error("a reading with no fallback for unsafe shapes passed the property; the property does not hold the safety rule")
		}
	})

	t.Run("dropping the separator handling is caught", func(t *testing.T) {
		t.Parallel()
		dropSeparators := func(c string) []string {
			return slices.DeleteFunc(slices.Clone(doneSlotLoopReadTokens(c)), func(s string) bool {
				return shellCommandSeparators[s]
			})
		}
		if len(doneSlotLoopLoosenings(commands, dropSeparators)) == 0 {
			t.Error("a reading without separators passed the property; the property does not hold the separators")
		}
		// The same mutation through the production judge: the loop closes at
		// the `done` of `gt done` once its separators are gone, so the gate
		// behind an unclosed loop reads as a one-shot.
		const command = "while true; do gt done; done"
		if reason, _ := doneSlotLoopVerdictWith(command, doneSlotLoopReadTokens); reason == "" {
			t.Errorf("production reading does not refuse %q", command)
		}
		if reason, _ := doneSlotLoopVerdictWith(command, dropSeparators); reason != "" {
			t.Errorf("reading without separators refuses %q as %q; the mutation is not the one the property catches", command, reason)
		}
	})
}
