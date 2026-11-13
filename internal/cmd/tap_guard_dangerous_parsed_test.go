package cmd

import (
	"slices"
	"testing"
)

// dangerousCorpusRow is one of this guard's own corpus rows, with the verdict
// the corpus records for it. The rows are read from guard_corpus.tsv, so a
// command added to the corpus is judged here without a second list to edit.
type dangerousCorpusRow struct {
	name    string
	command string
	context string
	blocked bool
}

func dangerousCorpusRows(t *testing.T) []dangerousCorpusRow {
	t.Helper()
	var rows []dangerousCorpusRow
	for _, row := range loadGuardCorpus(t) {
		if row.guard != corpusDangerous {
			continue
		}
		rows = append(rows, dangerousCorpusRow{
			name:    row.name(),
			command: row.command,
			context: row.context,
			blocked: row.verdict == corpusRefuse,
		})
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no %s rows", guardCorpusPath, corpusDangerous)
	}
	return rows
}

// dangerousCommands is the property's command set: the differential's command
// source (the guard corpus and the generated commands) and this guard's own
// rows.
func dangerousCommands(t *testing.T) []string {
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
	for _, row := range dangerousCorpusRows(t) {
		add(row.command)
	}
	return commands
}

// dangerousVerdictWith judges command as the guard does, through the production
// path with one reading swapped in.
func dangerousVerdictWith(command string, read dangerousReader) string {
	reason, _ := evaluateDangerousCommandWith(command, 0, noTownSession, read)
	return reason
}

// dangerousLoosenings returns one entry for each command the parser reads whose
// verdict through read allows what shellTokenize's reading refuses. A command
// shellTokenize refuses and read refuses too is not listed; a command read
// refuses where shellTokenize allows is a tightening, which this move may add.
func dangerousLoosenings(commands []string, read dangerousReader) []string {
	var failures []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		if dangerousVerdictWith(c, shellTokenize) != "" && dangerousVerdictWith(c, read) == "" {
			failures = append(failures, c)
		}
	}
	return failures
}

// TestDangerousCorpusRows pins the verdict of this guard's own corpus rows, so
// a reading change that flips one of them is named rather than buried in the
// property's aggregate. Each row is judged in the session its context column
// names, exactly as TestGuardCorpusMatchesCurrentVerdicts judges it.
func TestDangerousCorpusRows(t *testing.T) {
	t.Parallel()
	fixture := newGuardCorpusFixture(t)
	for _, row := range dangerousCorpusRows(t) {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			sess := guardSession{proc: fixture.process(t, row.context), townRoot: fixture.town}
			reason, _ := evaluateDangerousCommand(fixture.expand(row.command), 0, sess)
			if got := reason != ""; got != row.blocked {
				t.Errorf("dangerous verdict on %q in %s blocked=%v (reason %q), want %v",
					row.command, row.context, got, reason, row.blocked)
			}
		})
	}
}

// TestDangerousReadsThroughParsedTokens pins the verdict of commands the guard
// judges through the production reader, and which reading answered each one: the
// parsed tokens for a plain command, and the text reading for a command the
// parser cannot read or one dangerousParsedUnsafe routes. Each verdict is also
// asserted equal to the shellTokenize verdict, so the move cannot flip a
// command toward allowing.
func TestDangerousReadsThroughParsedTokens(t *testing.T) {
	t.Parallel()
	parsed := func(c string) []string {
		tokens, _ := parsedTokens(c)
		return tokens
	}
	tests := []struct {
		name    string
		command string
		blocked bool
		reading dangerousReader
	}{
		{"a readable command is judged on its parsed tokens", "ls -l", false, parsed},
		{"a readable command whose token lines differ between the readings is judged on its parsed tokens", "echo >f", false, parsed},
		{"a loop is routed to the text reading and refused", "for i in 1 2 3; do gt done --status DEFERRED; done", true, shellTokenize},
		{"a loop inside a substitution is routed to the text reading and refused", "echo $(for i in 1; do gt done; done)", true, shellTokenize},
		{"an unreadable command keeps the text reading and is refused as before", `echo "unterminated`, true, shellTokenize},
		{"an unreadable command running the gate is refused as before", `for i in 1 2; do gt done; done "`, true, shellTokenize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason := dangerousVerdictWith(tt.command, dangerousReadTokens)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("dangerousReadTokens verdict on %q blocked=%v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
			if got, want := dangerousReadTokens(tt.command), tt.reading(tt.command); !slices.Equal(got, want) {
				t.Errorf("dangerousReadTokens(%q) = %q, want the reading %q", tt.command, got, want)
			}
			want := dangerousVerdictWith(tt.command, shellTokenize)
			if (reason != "") != (want != "") {
				t.Errorf("reading through dangerousReadTokens gives %q where shellTokenize gives %q for %q; the move changed a verdict",
					reason, want, tt.command)
			}
		})
	}
}

// TestDangerousProductionReader pins which reading the guard's text reads are
// handed: the parsed one for a command the parser reads with no unsafe shape,
// and the text one for a command it cannot read or one dangerousParsedUnsafe
// names. Each row is a command whose two readings differ, so the assertion sees
// which one answered.
func TestDangerousProductionReader(t *testing.T) {
	t.Parallel()
	parsed := func(c string) []string {
		tokens, _ := parsedTokens(c)
		return tokens
	}
	tests := []struct {
		name    string
		command string
		reading dangerousReader
	}{
		{"a readable command with no unsafe shape is read by the parser", "echo >f", parsed},
		{"a glued file descriptor is read by the parser", "echo 2>&1", parsed},
		{"an unreadable command falls back to the text reading", `echo "unterminated`, shellTokenize},
		{"a for loop falls back to the text reading", "for i in 1 2; do gt done; done", shellTokenize},
		{"a while loop falls back to the text reading", "while true; do gt done; break; done", shellTokenize},
		{"a case statement falls back to the text reading", "case x in a) echo b;; esac", shellTokenize},
		{"an if block falls back to the text reading", "if true; then echo a; fi", shellTokenize},
		{"a test expression falls back to the text reading", "[[ -f x ]]", shellTokenize},
		{"a brace group falls back to the text reading", "{ echo hi; }", shellTokenize},
		{"a '#' that starts a word falls back to the text reading", "echo >f # note", shellTokenize},
		{"a '#' inside a word is literal and keeps the parsed reading", "echo a#b >f", parsed},
		{"a quoted '#' is literal and keeps the parsed reading", "echo '# note' >f", parsed},
		{"a parameter expansion's braces keep the parsed reading", "echo ${x:-y} >f", parsed},
		{"an ANSI-C string across a line continuation falls back to the text reading", "echo $\\\n'x'", shellTokenize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := dangerousReadTokens(tt.command)
			if want := tt.reading(tt.command); !slices.Equal(got, want) {
				t.Errorf("dangerousReadTokens(%q) = %q, want %q", tt.command, got, want)
			}
		})
	}
}

// TestDangerousIdleGateReadsThroughParsedTokens pins the idle gate's segment
// walk on the production reader: a gated suite start is still held, and an
// unreadable command is read as before.
func TestDangerousIdleGateReadsThroughParsedTokens(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "clone")
	proc := fakeGuardProcess(nil, goTree)
	tests := []struct {
		name    string
		command string
		held    bool
	}{
		{"a plain make test is held", "make test", true},
		{"a make test after an env prefix is held", "GOFLAGS=-p=8 make test", true},
		{"a make test behind a glued redirection is held", "2>/dev/null make test", true},
		{"a plain command is not held", "ls -l", false},
		{"a group whose cd does not carry is held", "(cd /tmp); make test", true},
		{"a group after a cd that carries is not held", "cd /tmp; (cd .); make test", false},
		{"an unreadable command is read as before", `make test "`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			held := idleGateHeldActionWith(proc, tt.command, dangerousReadTokens)
			if got := held != ""; got != tt.held {
				t.Errorf("idleGateHeldActionWith(%q) held=%v (%q), want %v", tt.command, got, held, tt.held)
			}
			want := idleGateHeldActionWith(proc, tt.command, shellTokenize)
			if (held != "") != (want != "") {
				t.Errorf("reading through dangerousReadTokens held %q where shellTokenize held %q for %q; the move changed a verdict",
					held, want, tt.command)
			}
		})
	}
}

// TestDangerousParsedVerdictNeverLoosens proves, over every command the parser
// reads, that the guard's verdict through parsedTokens is never less strict than
// the shellTokenize verdict. A command whose parsed reading would allow what
// shellTokenize refuses is routed to shellTokenize by dangerousParsedUnsafe, so
// the property holds for it too.
func TestDangerousParsedVerdictNeverLoosens(t *testing.T) {
	t.Parallel()
	commands := dangerousCommands(t)
	if failures := dangerousLoosenings(commands, dangerousReadTokens); len(failures) != 0 {
		for _, f := range failures {
			t.Errorf("parsed reading allows what shellTokenize refuses: %q", f)
		}
	}
	// Every verdict the move changes, so the closing note can list them.
	var changed []string
	parsedRead := 0
	for _, c := range commands {
		if toks, readable := parsedTokens(c); readable && slices.Equal(dangerousReadTokens(c), toks) {
			parsedRead++
		}
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		old := dangerousVerdictWith(c, shellTokenize)
		now := dangerousVerdictWith(c, dangerousReadTokens)
		if (old != "") != (now != "") {
			changed = append(changed, c)
		}
	}
	t.Logf("%d commands checked, %d read by the parser, %d verdicts changed", len(commands), parsedRead, len(changed))
	for _, c := range changed {
		t.Logf("verdict changed: %q (shellTokenize %q, parsed %q)",
			c, dangerousVerdictWith(c, shellTokenize), dangerousVerdictWith(c, dangerousReadTokens))
	}

	t.Run("the property fails without the fallback", func(t *testing.T) {
		t.Parallel()
		parsedOnly := func(c string) []string {
			tokens, readable := parsedTokens(c)
			if !readable {
				return shellTokenize(c)
			}
			return tokens
		}
		if len(dangerousLoosenings(commands, parsedOnly)) == 0 {
			t.Error("a reading with no fallback for unsafe shapes passed the property; the property does not hold the safety rule")
		}
	})

	t.Run("dropping the separator handling is caught", func(t *testing.T) {
		t.Parallel()
		dropSeparators := func(c string) []string {
			return slices.DeleteFunc(slices.Clone(dangerousReadTokens(c)), func(s string) bool {
				return shellCommandSeparators[s]
			})
		}
		if len(dangerousLoosenings(commands, dropSeparators)) == 0 {
			t.Error("a reading without separators passed the property; the property does not hold the separators")
		}
		// The same mutation through the production judge: with the pipe gone,
		// xargs no longer heads a segment, so the repeater rule does not fire
		// and the gate it repeats is not refused.
		const command = "seq 1 300 | xargs -I{} bash -c 'gt done'"
		if reason := dangerousVerdictWith(command, dangerousReadTokens); reason == "" {
			t.Errorf("production reading does not refuse %q", command)
		}
		if reason := dangerousVerdictWith(command, dropSeparators); reason != "" {
			t.Errorf("reading without separators refuses %q as %q; the mutation is not the one the property catches", command, reason)
		}
	})
}
