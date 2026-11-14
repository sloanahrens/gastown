package cmd

import (
	"fmt"
	"slices"
	"testing"
)

// sandboxSecretsCorpusRow is one command the property test judges, with the
// verdict this guard answers for it. The rows cover the environ-path shapes the
// parsed reading could flatten or drop: a path split by quotes, a command
// substitution in the path, a git commit message, a here-document body, a for
// header, a [[ ]] expression, and a path that follows a separator.
type sandboxSecretsCorpusRow struct {
	name    string
	command string
	blocked bool
}

func sandboxSecretsCorpusRows() []sandboxSecretsCorpusRow {
	return []sandboxSecretsCorpusRow{
		{"a proc environ read", "cat /proc/self/environ", true},
		{"a path split by quotes", `cat "/proc/self/env"iron`, true},
		{"a substitution in the path", "cat /proc/$(pgrep sh)/environ", true},
		{"a path decoded from an ANSI-C string", `cat /proc/self/env$'\x69ron'`, true},
		{"an ordinary command", "ls -l", false},
		{"a git commit message naming the path", "git commit -m /proc/self/environ", false},
		{"a path after a commit with no message", "git commit -m; /proc/self/environ", true},
		{"a here-document body naming the path", "cat > f <<'EOF'\n/proc/self/environ\nEOF", true},
		{"a for header naming the path", "for x in /proc/self/environ; do echo $x; done", true},
		{"a case word naming the path", "case /proc/self/environ in *) echo x;; esac", true},
		{"a test expression naming the path", "[[ -f /proc/self/environ ]]", true},
		{"an unreadable command naming the path", `cat /proc/self/environ "unterminated`, true},
		{"an unreadable command naming nothing", `echo "unterminated`, true},
	}
}

// sandboxSecretsCommands is the property's command set: the differential's
// command source (the guard corpus and the generated commands) and this guard's
// own rows.
func sandboxSecretsCommands(t *testing.T) []string {
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
	for _, row := range sandboxSecretsCorpusRows() {
		add(row.command)
	}
	return commands
}

// sandboxSecretsVerdictWith judges command as the guard does, through the
// production path with one reading swapped in.
func sandboxSecretsVerdictWith(command string, read sandboxSecretsReader) string {
	return sandboxSecretsReasonWith(command, read)
}

// sandboxSecretsLoosenings returns one entry for each command the parser reads
// whose verdict through read allows what shellTokenize's reading refuses. A
// command shellTokenize refuses and read refuses too is not listed; a command
// read refuses where shellTokenize allows is a tightening, which this move may
// add.
func sandboxSecretsLoosenings(commands []string, read sandboxSecretsReader) []string {
	var failures []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		if sandboxSecretsVerdictWith(c, shellTokenize) != "" && sandboxSecretsVerdictWith(c, read) == "" {
			failures = append(failures, c)
		}
	}
	return failures
}

// TestSandboxSecretsCorpusRows pins the verdict of this guard's own rows, so a
// reading change that flips one of them is named rather than buried in the
// property's aggregate.
func TestSandboxSecretsCorpusRows(t *testing.T) {
	t.Parallel()
	for _, row := range sandboxSecretsCorpusRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			reason := sandboxSecretsReason(row.command)
			if got := reason != ""; got != row.blocked {
				t.Errorf("sandboxSecretsReason(%q) blocked=%v (reason %q), want %v", row.command, got, reason, row.blocked)
			}
		})
	}
}

// TestSandboxSecretsReadsThroughParsedTokens pins the verdict of commands the
// guard judges through the production reader, including the ones it hands to
// shellTokenize: a command the parser cannot read, and a command
// sandboxSecretsParsedUnsafe names. Each verdict is also checked against the
// shellTokenize verdict in one direction: the move may tighten a verdict, never
// loosen it.
func TestSandboxSecretsReadsThroughParsedTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"a readable environ read is refused", "cat /proc/self/environ", true},
		{"a readable command that reads no environ stays allowed", "ls -l", false},
		{"an unreadable command keeps the text reading of its seek", `cat /proc/self/environ "unterminated`, true},
		{"an unreadable command is refused as before", `echo "unterminated`, true},
		{"a here-document is routed to the text reading", "cat > f <<'EOF'\n/proc/self/environ\nEOF", true},
		{"a for header is routed to the text reading", "for x in /proc/self/environ; do echo $x; done", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason := sandboxSecretsReason(tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("sandboxSecretsReason(%q) blocked=%v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
			want := sandboxSecretsVerdictWith(tt.command, shellTokenize)
			if want != "" && reason == "" {
				t.Errorf("reading through sandboxSecretsReadTokens allows %q where shellTokenize refuses it as %q; the move loosened a verdict",
					tt.command, want)
			}
		})
	}
}

// TestSandboxSecretsProductionReader pins which reading the guard hands its
// judge: the parsed one for a command the parser reads with no unsafe shape,
// and the text one for a command it cannot read or one
// sandboxSecretsParsedUnsafe names. Each row is a command whose two readings
// differ, so the assertion sees which one answered.
func TestSandboxSecretsProductionReader(t *testing.T) {
	t.Parallel()
	parsed := func(c string) []string {
		tokens, _ := parsedTokens(c)
		return tokens
	}
	tests := []struct {
		name    string
		command string
		reading sandboxSecretsReader
	}{
		{"a readable command with no unsafe shape is read by the parser", "cat /proc/self/environ 2>/dev/null", parsed},
		{"a plain environ read is read by the parser", "cat /proc/1/environ", parsed},
		{"an unreadable command falls back to the text reading", `echo "unterminated`, shellTokenize},
		{"a here-document falls back to the text reading", "cat > f <<EOF\nx\nEOF", shellTokenize},
		{"a [[ ]] expression falls back to the text reading", "[[ -f x ]]", shellTokenize},
		{"a for header falls back to the text reading", "for i in 1 2; do echo $i; done", shellTokenize},
		{"a case header falls back to the text reading", "case x in a) echo b;; esac", shellTokenize},
		{"a select header falls back to the text reading", "select x in 1; do echo $x; done", shellTokenize},
		{"a single '<' is not a here-document", "echo a<b", parsed},
		{"a here-string keeps the parsed reading", "cat <<< /proc/self/environ", parsed},
		{"a command substitution falls back to the text reading", "echo $(cat /proc/self/environ)", shellTokenize},
		{"a backtick substitution falls back to the text reading", "echo `cat /proc/self/environ`", shellTokenize},
		{"a process substitution falls back to the text reading", "cat <(cat /proc/self/environ)", shellTokenize},
		{"an output process substitution falls back to the text reading", "tee >(cat /proc/self/environ)", shellTokenize},
		{"an ordinary command with no substitution keeps the parsed reading", "echo hi", parsed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sandboxSecretsReadTokens(tt.command)
			if want := tt.reading(tt.command); !slices.Equal(got, want) {
				t.Errorf("sandboxSecretsReadTokens(%q) = %q, want %q", tt.command, got, want)
			}
		})
	}
}

// TestSandboxSecretsSubstitutionReadsPathAsText pins the verdict on the quoted
// and escaped environ spellings inside a substitution. parsedTokens keeps a
// substitution as one raw word, so these read an environment block only through
// the text reading sandboxSecretsParsedUnsafe routes them to (gt-p0y85u).
func TestSandboxSecretsSubstitutionReadsPathAsText(t *testing.T) {
	t.Parallel()
	const want = "it reads a process's environment block"
	tests := []struct {
		name    string
		command string
	}{
		{"a quoted directory in a substitution", "echo $(cat /pro''c/1/environ)"},
		{"a quoted directory in an assignment", "x=$(cat /pro''c/1/environ)"},
		{"a quoted directory in backticks", "echo `cat /pro''c/1/environ`"},
		{"a quoted name in a substitution", "echo $(cat /proc/1/envi''ron)"},
		{"an escaped name in a substitution", `echo $(cat /proc/1/en\viron)`},
		{"a quoted directory in a process substitution", "cat <(cat /pro''c/1/environ)"},
		{"a quoted directory in an output process substitution", "tee >(cat /pro''c/1/environ)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sandboxSecretsReason(tt.command); got != want {
				t.Errorf("sandboxSecretsReason(%q) = %q, want %q", tt.command, got, want)
			}
		})
	}
}

// TestSandboxSecretsGrammarShapesRouteToText pins the construct rule behind the
// no-loosening property. Each construct sandboxSecretsParsedUnsafe names gets the
// shellTokenize verdict wherever it sits in a command, so the rule covers every
// position a construct can take, not only the commands the property samples.
func TestSandboxSecretsGrammarShapesRouteToText(t *testing.T) {
	t.Parallel()
	shapes := []string{
		"cat > f <<'EOF'\n/proc/self/environ\nEOF",
		"[[ -f /proc/self/environ ]]",
		"for x in /proc/self/environ; do echo $x; done",
		"case /proc/self/environ in *) echo x;; esac",
		"select x in /proc/self/environ; do echo $x; done",
		"echo $(cat /proc/self/environ)",
		"echo `cat /proc/self/environ`",
		"cat <(cat /proc/self/environ)",
		"cat >(cat /proc/self/environ)",
	}
	prefixes := []string{"", "ls; ", "ls && ", "ls || ", "ls | ", "ls\n"}
	for _, shape := range shapes {
		for _, prefix := range prefixes {
			command := prefix + shape
			t.Run(fmt.Sprintf("after %q: %s", prefix, shape), func(t *testing.T) {
				t.Parallel()
				want := sandboxSecretsVerdictWith(command, shellTokenize)
				if want == "" {
					t.Fatalf("shellTokenize allows %q; the row must name the environ path", command)
				}
				if got := sandboxSecretsReason(command); got != want {
					t.Errorf("sandboxSecretsReason(%q) = %q, want the text reading's %q; the construct is not routed to shellTokenize",
						command, got, want)
				}
			})
		}
	}
}

// TestSandboxSecretsParsedVerdictNeverLoosens checks, over the finite command set
// (the differential's commands and this guard's rows), that the guard's verdict
// through parsedTokens is never less strict than the shellTokenize verdict. The
// set samples the property; the grammar holds it for every other command, since
// sandboxSecretsParsedUnsafe routes each construct the parse drops or keeps raw to
// shellTokenize (TestSandboxSecretsGrammarShapesRouteToText).
func TestSandboxSecretsParsedVerdictNeverLoosens(t *testing.T) {
	t.Parallel()
	commands := sandboxSecretsCommands(t)
	if failures := sandboxSecretsLoosenings(commands, sandboxSecretsReadTokens); len(failures) != 0 {
		for _, f := range failures {
			t.Errorf("parsed reading allows what shellTokenize refuses: %q", f)
		}
	}
	// Every verdict the move changes. The list is pinned here, so a reading change
	// that flips another command fails by name instead of changing a log line.
	var changed []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		old := sandboxSecretsVerdictWith(c, shellTokenize)
		now := sandboxSecretsVerdictWith(c, sandboxSecretsReadTokens)
		if (old != "") != (now != "") {
			changed = append(changed, c)
		}
	}
	wantChanged := []string{`cat /proc/self/env$'\x69ron'`}
	if !slices.Equal(changed, wantChanged) {
		t.Errorf("verdicts changed = %q, want %q", changed, wantChanged)
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
		if len(sandboxSecretsLoosenings(commands, parsedOnly)) == 0 {
			t.Error("a reading with no fallback for unsafe shapes passed the property; the property does not hold the safety rule")
		}
	})

	t.Run("dropping the separator handling is caught", func(t *testing.T) {
		t.Parallel()
		dropSeparators := func(c string) []string {
			return slices.DeleteFunc(slices.Clone(sandboxSecretsReadTokens(c)), func(s string) bool {
				return shellCommandSeparators[s]
			})
		}
		if len(sandboxSecretsLoosenings(commands, dropSeparators)) == 0 {
			t.Error("a reading without separators passed the property; the property does not hold the separators")
		}
		// The same mutation through the production judge: with the separator
		// gone, the commit-message skip reads the next command's path as the
		// message and drops it.
		command := "git commit -m; /proc/self/environ"
		if reason := sandboxSecretsReason(command); reason == "" {
			t.Errorf("production reading does not refuse %q", command)
		}
		if reason := sandboxSecretsReasonWith(command, dropSeparators); reason != "" {
			t.Errorf("reading without separators refuses %q as %q; the mutation is not the one the property catches", command, reason)
		}
	})
}
