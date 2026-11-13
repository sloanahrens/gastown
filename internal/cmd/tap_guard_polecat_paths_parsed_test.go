package cmd

import (
	"path/filepath"
	"slices"
	"testing"
)

// polecatPathsCorpusRow is one command the property test judges, with the
// verdict this guard answers for it in the fixture. The rows cover the shapes
// parsedTokens could flatten (a quoted path, an env prefix, a pipe, a heredoc,
// a subshell) and the comment shapes a `#` can start.
type polecatPathsCorpusRow struct {
	name      string
	command   string
	wantBlock bool
}

// polecatPathsCorpusRows builds this guard's own rows on p, so every path in a
// command is one the fixture really holds.
func polecatPathsCorpusRows(p polecatTestTown) []polecatPathsCorpusRow {
	sib := filepath.Join(p.sibling, "x.go")
	own := filepath.Join(p.worktree, "x.go")
	return []polecatPathsCorpusRow{
		{"a path in a quoted word", `rm -rf "` + sib + `"`, true},
		{"an env prefix", "X=" + sib + ` rm -rf "$X"`, true},
		{"a pipe", "echo hi | tee " + sib, true},
		{"a heredoc", "cat > " + sib + " <<'EOF'\ntext\nEOF", true},
		{"a subshell", "( rm -rf " + sib + " )", true},
		{"a loop assigning the name it deletes through", "for GT_POLECAT_PATH in " + p.sibling + `; do rm -rf "$GT_POLECAT_PATH"; done`, true},
		{"a comment after a write", "rm -rf " + sib + " # comment", true},
		{"a write after a comment is never run", "ls;#x rm -rf " + sib, false},
		{"echo followed by a write after a separator", "echo hi; rm -rf " + sib, true},
		{"a '#' inside a word is literal", "rm -rf " + filepath.Join(p.worktree, "a#b"), false},
		{"a quoted '#' is literal", "rm -rf " + own + " '#note'", false},
		{"$# is not a comment", "echo $#", false},
	}
}

// polecatPathsCommands is the property's command set: the differential's
// command source (the guard corpus and the generated commands) and this
// guard's own rows.
func polecatPathsCommands(t *testing.T, p polecatTestTown) []string {
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
	for _, row := range polecatPathsCorpusRows(p) {
		add(row.command)
	}
	return commands
}

// polecatPathsVerdict judges command as the Bash hook does, through the
// scope's reader.
func polecatPathsVerdict(scope polecatPathScope, command string) string {
	var hook polecatPathsInput
	hook.ToolName = "Bash"
	hook.Cwd = scope.cwd
	hook.ToolInput.Command = command
	return scope.evaluate(hook)
}

// polecatPathsVerdictWith judges command through the production path with one
// reading swapped in.
func polecatPathsVerdictWith(scope polecatPathScope, command string, read polecatPathsReader) string {
	scope.read = read
	return polecatPathsVerdict(scope, command)
}

// polecatPathsLoosenings returns one entry for each command the parser reads
// whose verdict under read allows what shellTokenize's reading refuses. A
// command shellTokenize allows and read refuses is a tightening, which this
// guard's move may add and which is not listed here.
func polecatPathsLoosenings(commands []string, scope polecatPathScope, read polecatPathsReader) []string {
	var failures []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		if want := polecatPathsVerdictWith(scope, c, shellTokenize); want != "" {
			if got := polecatPathsVerdictWith(scope, c, read); got == "" {
				failures = append(failures, c)
			}
		}
	}
	return failures
}

// TestPolecatPathsCorpusRows pins the verdict of this guard's own rows, so a
// reading change that flips one of them is named rather than buried in the
// property's aggregate.
func TestPolecatPathsCorpusRows(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	scope, ok := resolvePolecatPathScope(p.worktree, p.proc())
	if !ok {
		t.Fatal("the fixture is not recognised as a polecat session")
	}
	for _, row := range polecatPathsCorpusRows(p) {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			reason := polecatPathsVerdict(scope, row.command)
			if got := reason != ""; got != row.wantBlock {
				t.Errorf("checkBashCommand(%q) blocked=%v (reason %q), want %v", row.command, got, reason, row.wantBlock)
			}
		})
	}
}

// TestPolecatPathsReadsThroughParsedTokens pins the verdict of commands the
// guard judges through the production reader, including the two it hands to
// shellTokenize: a command the parser cannot read, and a command
// polecatPathsParsedUnsafe names. Each verdict is also asserted equal to the
// shellTokenize verdict, so the move cannot flip a command toward allowing.
func TestPolecatPathsReadsThroughParsedTokens(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	scope, ok := resolvePolecatPathScope(p.worktree, p.proc())
	if !ok {
		t.Fatal("the fixture is not recognised as a polecat session")
	}
	sib := filepath.Join(p.sibling, "x.go")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"a readable write into the sibling worktree is refused", "rm -rf " + sib, true},
		{"a readable command that writes nothing stays allowed", "ls -l", false},
		{"an unreadable command keeps the text reading of its write", `rm -rf "` + sib + `" "unterminated`, true},
		{"an unreadable command that writes nothing stays allowed", `echo "unterminated`, false},
		{"a compound command is routed to the text reading", "for GT_POLECAT_PATH in " + p.sibling + `; do rm -rf "$GT_POLECAT_PATH"; done`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason := polecatPathsVerdictWith(scope, tt.command, polecatPathsReadTokens)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("checkBashCommand(%q) blocked=%v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
			want := polecatPathsVerdictWith(scope, tt.command, shellTokenize)
			if (reason != "") != (want != "") {
				t.Errorf("reading through polecatPathsReadTokens gives %q where shellTokenize gives %q for %q; the move changed a verdict",
					reason, want, tt.command)
			}
		})
	}
}

// TestPolecatPathsJudgesThroughItsReader proves both call sites read through
// the scope's seam: a scope whose reader loses the separators lets the write
// behind a separator through, and one whose reader hides a loop's variable
// name lets a write through that name. The same commands stay refused on the
// production reader, so a call site that went back to shellTokenize directly
// would keep them refused and this test would fail.
func TestPolecatPathsJudgesThroughItsReader(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	scope, ok := resolvePolecatPathScope(p.worktree, p.proc())
	if !ok {
		t.Fatal("the fixture is not recognised as a polecat session")
	}
	dropSeparators := func(c string) []string {
		return slices.DeleteFunc(slices.Clone(polecatPathsReadTokens(c)), func(s string) bool {
			return shellCommandSeparators[s]
		})
	}
	hideLoopName := func(c string) []string {
		return slices.DeleteFunc(slices.Clone(polecatPathsReadTokens(c)), func(s string) bool {
			return s == "GT_POLECAT_PATH"
		})
	}
	tests := []struct {
		name    string
		command string
		read    polecatPathsReader
	}{
		{
			name:    "the segment split reads through the scope's reader",
			command: "echo hi; rm -rf " + filepath.Join(p.sibling, "x.go"),
			read:    dropSeparators,
		},
		{
			name:    "the assignment scan reads through the scope's reader",
			command: "for GT_POLECAT_PATH in " + p.sibling + `; do rm -rf "$GT_POLECAT_PATH"; done`,
			read:    hideLoopName,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if reason := polecatPathsVerdictWith(scope, tt.command, polecatPathsReadTokens); reason == "" {
				t.Fatalf("production reading does not refuse %q", tt.command)
			}
			if reason := polecatPathsVerdictWith(scope, tt.command, tt.read); reason != "" {
				t.Errorf("reading through the scope's seam refuses %q as %q; the guard is not judging through it", tt.command, reason)
			}
		})
	}
}

// TestPolecatPathsProductionReader pins which reading the guard hands its
// judge: the parsed one for a command the parser reads with no unsafe shape,
// and the text one for a command it cannot read or one polecatPathsParsedUnsafe
// names. Each row is a command whose two readings differ, so the assertion
// sees which one answered.
func TestPolecatPathsProductionReader(t *testing.T) {
	t.Parallel()
	parsed := func(c string) []string {
		tokens, _ := parsedTokens(c)
		return tokens
	}
	tests := []struct {
		name    string
		command string
		reading polecatPathsReader
	}{
		{"a readable command with no unsafe shape is read by the parser", "echo >f", parsed},
		{"an unreadable command falls back to the text reading", `echo "unterminated`, shellTokenize},
		{"a compound command falls back to the text reading", "for i in 1 2; do rm a; done", shellTokenize},
		{"a compound marker falls back to the text reading", "(echo hi)", shellTokenize},
		{"a '#' that starts a word falls back to the text reading", "ls;#x rm", shellTokenize},
		{"a '#' inside a word is literal and keeps the parsed reading", "echo a#b >f", parsed},
		{"a quoted '#' is literal and keeps the parsed reading", "echo '# x' >f", parsed},
		{"a bare $# is not a comment and keeps the parsed reading", "echo $# >f", parsed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := polecatPathsReadTokens(tt.command)
			if want := tt.reading(tt.command); !slices.Equal(got, want) {
				t.Errorf("polecatPathsReadTokens(%q) = %q, want %q", tt.command, got, want)
			}
		})
	}
}

// TestPolecatPathsCommentShapes pins where a shell comment starts, since that
// is the shape the '#' rule routes home: a word-start '#' is a comment, and a
// '#' inside a word, a quoted '#' and every $-form are literals.
func TestPolecatPathsCommentShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command string
		want    bool
	}{
		{"cmd # comment", true},
		{"cmd #comment", true},
		{"#comment", true},
		{"ls;#x rm", true},
		{"ls&&#x rm", true},
		{"ls|#x rm", true},
		{"cat a\n# note\ncat b", true},
		{`cmd \#escaped`, false},
		{`echo 'a' #b`, true},
		{`echo "a\" #b"`, false},
		{`a#b`, false},
		{`echo '# x' y`, false},
		{`echo "a # b"`, false},
		{`echo $#`, false},
		{`echo ${#x}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			t.Parallel()
			if got := startsShellComment(tt.command); got != tt.want {
				t.Errorf("startsShellComment(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestPolecatPathsParsedVerdictNeverLoosens proves, over every command the
// parser reads, that the guard's verdict through parsedTokens is never less
// strict than the shellTokenize verdict. A command whose parsed reading would
// allow what shellTokenize refuses is routed to shellTokenize by
// polecatPathsParsedUnsafe, so the property holds for it too.
func TestPolecatPathsParsedVerdictNeverLoosens(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	scope, ok := resolvePolecatPathScope(p.worktree, p.proc())
	if !ok {
		t.Fatal("the fixture is not recognised as a polecat session")
	}
	commands := polecatPathsCommands(t, p)
	if failures := polecatPathsLoosenings(commands, scope, polecatPathsReadTokens); len(failures) != 0 {
		for _, f := range failures {
			t.Errorf("parsed reading allows what shellTokenize refuses: %q", f)
		}
	}
	// Every verdict the move changes, so the closing note can list them.
	var changed []string
	for _, c := range commands {
		if _, readable := parsedTokens(c); !readable {
			continue
		}
		old := polecatPathsVerdictWith(scope, c, shellTokenize)
		now := polecatPathsVerdictWith(scope, c, polecatPathsReadTokens)
		if (old != "") != (now != "") {
			changed = append(changed, c)
		}
	}
	t.Logf("%d commands checked, %d verdicts changed", len(commands), len(changed))
	for _, c := range changed {
		t.Logf("verdict changed: %q", c)
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
		if len(polecatPathsLoosenings(commands, scope, parsedOnly)) == 0 {
			t.Error("a reading with no fallback for unsafe shapes passed the property; the property does not hold the safety rule")
		}
	})

	t.Run("dropping the separator handling is caught", func(t *testing.T) {
		t.Parallel()
		dropSeparators := func(c string) []string {
			return slices.DeleteFunc(slices.Clone(polecatPathsReadTokens(c)), func(s string) bool {
				return shellCommandSeparators[s]
			})
		}
		if len(polecatPathsLoosenings(commands, scope, dropSeparators)) == 0 {
			t.Error("a reading without separators passed the property; the property does not hold the separators")
		}
		// The same mutation through the production judge: the write behind the
		// separator reads as an argument of echo once the separator is gone.
		command := "echo hi; rm -rf " + filepath.Join(p.sibling, "x.go")
		if reason := polecatPathsVerdictWith(scope, command, polecatPathsReadTokens); reason == "" {
			t.Errorf("production reading does not refuse %q", command)
		}
		if reason := polecatPathsVerdictWith(scope, command, dropSeparators); reason != "" {
			t.Errorf("reading without separators refuses %q as %q; the mutation is not the one the property catches", command, reason)
		}
	})
}
