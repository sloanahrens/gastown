package cmd

import (
	"slices"
	"strings"
	"testing"
)

// parsedTokensSyntax are the words shellparse reads as the syntax of a
// compound command, which contributes no token of its own to parsedTokens.
var parsedTokensSyntax = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"for": true, "in": true, "do": true, "done": true, "while": true, "until": true,
	"case": true, "esac": true, "select": true, "function": true,
	"{": true, "}": true, "!": true, "[[": true, "]]": true, "(": true, ")": true,
}

// parsedTokensCloser are the words that end a compound command.
var parsedTokensCloser = map[string]bool{"done": true, "fi": true, "esac": true, "}": true, ")": true, "]]": true}

// parsedTokensRedirectOps are the redirection operators, with the file
// descriptor or {name} prefix taken off.
var parsedTokensRedirectOps = map[string]bool{
	"<": true, ">": true, ">>": true, "<<": true, "<<<": true, "<<-": true,
	"<&": true, ">&": true, "&>": true, "&>>": true, "<>": true, ">|": true,
}

// parsedTokensSeparators are the tokens shellTokenize and parsedTokens both
// emit between commands.
var parsedTokensSeparators = []string{";", "&", "&&", "||", "|"}

// isParsedTokensRedirectOp reports whether tok is a redirection operator, with
// an optional file descriptor or {name} in front.
func isParsedTokensRedirectOp(tok string) bool {
	if strings.HasPrefix(tok, "{") {
		if i := strings.Index(tok, "}"); i >= 0 {
			tok = tok[i+1:]
		}
	}
	return parsedTokensRedirectOps[strings.TrimLeft(tok, "0123456789")]
}

// isParsedTokensSyntaxRun reports whether every token of run is compound-command
// syntax: a syntax word, a case pattern or function header ("a)", "f()"), or a
// separator ";" or ";;", with at least one syntax word or closing parenthesis.
func isParsedTokensSyntaxRun(run []string) bool {
	syntax := false
	for _, tok := range run {
		switch {
		case parsedTokensSyntax[tok]:
			syntax = true
		case strings.HasSuffix(tok, ")"):
			syntax = true
		case tok == ";" || tok == ";;":
		default:
			return false
		}
	}
	return syntax
}

// parsedTokensHasLiteralSemicolon reports whether command writes a ';' as a word,
// escaped or quoted. Both readings then emit a ';' token for the word as well as
// for a separator, so their counts of ';' tokens can differ.
func parsedTokensHasLiteralSemicolon(command string) bool {
	return strings.Contains(command, `\;`) || strings.Contains(command, `";"`) || strings.Contains(command, `';'`)
}

// parsedTokensNorm is the text a run of tokens reads as once blanks, quote
// characters, backslashes, parentheses and ';' are taken away. Two runs with the
// same norm hold the same characters, split into tokens differently.
func parsedTokensNorm(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\'', '"', '\\', ';', '(', ')':
			return -1
		}
		return r
	}, s)
}

// parsedTokensDecodeANSI decodes the two $'...' escapes a token can keep from
// shellTokenize, which leaves them as written.
func parsedTokensDecodeANSI(s string) string {
	return strings.NewReplacer(`\t`, "\t", `\n`, "\n").Replace(s)
}

// parsedTokensSubstitutionOpeners are the words that open a substitution or a
// process substitution, which parsedTokens keeps as one word.
var parsedTokensSubstitutionOpeners = []string{"$(", "`", "<(", ">("}

// parsedTokensHeredocOp returns the index of the first here-document operator
// in command ("<<" or "<<-" not "<<<"), or -1.
func parsedTokensHeredocOp(command string) int {
	for i := 0; i+1 < len(command); i++ {
		if command[i:i+2] == "<<" && !strings.HasPrefix(command[i:], "<<<") && (i == 0 || command[i-1] != '<') {
			return i
		}
	}
	return -1
}

// parsedTokensGluedOps are the redirection operators, longest first, so the one
// a token holds is its whole operator.
var parsedTokensGluedOps = []string{"&>>", "<<<", "<<-", "<<", ">>", "&>", "<&", ">&", "<>", ">|", "<", ">"}

// parsedTokensGluedSplit splits a token that holds a redirection operator into
// the pieces parsedTokens emits: the word before the operator, the operator with
// its file descriptor, and the target. It returns nil when tok holds no operator.
func parsedTokensGluedSplit(tok string) []string {
	at := strings.IndexAny(tok, "<>")
	if at < 0 {
		return nil
	}
	start := at
	if start > 0 && tok[start-1] == '&' {
		start--
	}
	head, fd := tok[:start], ""
	if head != "" && strings.Trim(head, "0123456789") == "" {
		head, fd = "", tok[:start]
	}
	rest := tok[start:]
	op := ""
	for _, candidate := range parsedTokensGluedOps {
		if strings.HasPrefix(rest, candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return nil
	}
	var pieces []string
	if head != "" {
		pieces = append(pieces, head)
	}
	pieces = append(pieces, fd+op)
	if target := rest[len(op):]; target != "" {
		if more := parsedTokensGluedSplit(target); more != nil {
			pieces = append(pieces, more...)
		} else {
			pieces = append(pieces, target)
		}
	}
	return pieces
}

// parsedTokensBlock is one run of tokens the two readings disagree on: shell
// is shellTokenize's run and parsed is parsedTokens', between two tokens they
// share.
type parsedTokensBlock struct {
	shell  []string
	parsed []string
}

// parsedTokensBlocks aligns the two token streams on their longest common
// subsequence and returns the runs between the shared tokens, in order.
func parsedTokensBlocks(shell, parsed []string) []parsedTokensBlock {
	n, m := len(shell), len(parsed)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case shell[i] == parsed[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var blocks []parsedTokensBlock
	var cur parsedTokensBlock
	flush := func() {
		if len(cur.shell) > 0 || len(cur.parsed) > 0 {
			blocks = append(blocks, cur)
		}
		cur = parsedTokensBlock{}
	}
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && shell[i] == parsed[j]:
			flush()
			i++
			j++
		case j >= m || (i < n && lcs[i+1][j] >= lcs[i][j+1]):
			cur.shell = append(cur.shell, shell[i])
			i++
		default:
			cur.parsed = append(cur.parsed, parsed[j])
			j++
		}
	}
	flush()
	return blocks
}

// parsedTokensClass is one listed class of difference between parsedTokens and
// shellTokenize. A class is either a rewrite, which removes from shellTokenize's
// stream a token parsedTokens never emits, or a block class, which covers a run
// of tokens still different after the rewrites. example is a command the class
// shows the difference on, and reason says why the readings differ.
type parsedTokensClass struct {
	name    string
	example string
	reason  string
	// rewrite returns the shell stream with the class's tokens removed, and
	// whether it removed any.
	rewrite func(command string, shell, parsed []string) ([]string, bool)
	// covers reports whether a block of command is this class.
	covers func(command string, b parsedTokensBlock) bool
}

// parsedTokensClasses is the table of known differences. The rewrites run first,
// in table order. Then a block belongs to the first block class that covers it.
// A block no class covers fails TestParsedTokensDifferential.
var parsedTokensClasses = []parsedTokensClass{
	{
		name:    "trailing-separator",
		example: "echo \"a b\" ;",
		reason:  "a separator that ends the command ends no command, so parsedTokens emits none after the last command",
		rewrite: func(_ string, shell, parsed []string) ([]string, bool) {
			out := shell
			for len(out) > 0 && slices.Contains(parsedTokensSeparators, out[len(out)-1]) {
				if len(parsed) > 0 && parsed[len(parsed)-1] == out[len(out)-1] {
					break
				}
				out = out[:len(out)-1]
			}
			return out, len(out) != len(shell)
		},
	},
	{
		name:    "empty-statement",
		example: "echo ;\necho b",
		reason:  "shellTokenize emits a ';' for a newline as well as for the ';' or '&&' before it, and for a comment line; a separator with no command before it is no command, so parsedTokens emits none. Skipped when the command writes a literal ';', which escaped-semicolon covers",
		rewrite: func(command string, shell, _ []string) ([]string, bool) {
			if parsedTokensHasLiteralSemicolon(command) {
				return shell, false
			}
			var out []string
			for _, tok := range shell {
				if tok == ";" && (len(out) == 0 || slices.Contains(parsedTokensSeparators, out[len(out)-1])) {
					continue
				}
				out = append(out, tok)
			}
			return out, len(out) != len(shell)
		},
	},
	{
		name:    "glued-redirection",
		example: "echo x>f",
		reason:  "shellTokenize leaves an operator glued to its word when no blank separates them; parsedTokens splits it into the word, the operator with its file descriptor, and the target",
		rewrite: func(_ string, shell, parsed []string) ([]string, bool) {
			known := map[string]bool{}
			for _, tok := range parsed {
				known[tok] = true
			}
			var out []string
			changed := false
			for _, tok := range shell {
				pieces := []string{tok}
				if !known[tok] {
					if split := parsedTokensGluedSplit(tok); split != nil {
						pieces = split
						changed = true
					}
				}
				out = append(out, pieces...)
			}
			return out, changed
		},
	},
	{
		name:    "compound-syntax",
		example: "if true; then echo y; fi",
		reason:  "a compound command's keywords and grouping parentheses are not tokens of parsedTokens; only its body's commands are",
		covers: func(_ string, b parsedTokensBlock) bool {
			return len(b.parsed) == 0 && isParsedTokensSyntaxRun(b.shell)
		},
	},
	{
		name:    "compound-header",
		example: "for i in 1 2; do echo $i; done",
		reason:  "the words of a for, select or case header are not commands, so parsedTokens drops them with the keyword",
		covers: func(command string, b parsedTokensBlock) bool {
			head := strings.Fields(command)
			if len(b.parsed) > 0 || len(head) == 0 || !slices.Contains([]string{"for", "select", "case"}, head[0]) {
				return false
			}
			for _, op := range []string{"&&", "||", "|", "&"} {
				if slices.Contains(b.shell, op) {
					return false
				}
			}
			return len(b.shell) > 0
		},
	},
	{
		name:    "test-expression",
		example: "[[ -n x ]] && echo y",
		reason:  "a [[ ]] conditional is not a command, so parsedTokens drops its words and the && that follows it",
		covers: func(_ string, b parsedTokensBlock) bool {
			return len(b.parsed) == 0 && len(b.shell) > 0 && b.shell[0] == "[[" && slices.Contains(b.shell, "]]")
		},
	},
	{
		name:    "compound-redirection",
		example: "while read l; do echo $l; done < file",
		reason:  "a redirection written on a compound command belongs to every command inside it, so parsedTokens gives it to none of them",
		covers: func(_ string, b parsedTokensBlock) bool {
			n := len(b.shell)
			if len(b.parsed) > 0 || n < 3 || !isParsedTokensRedirectOp(b.shell[n-2]) {
				return false
			}
			head := b.shell[:n-2]
			return slices.ContainsFunc(head, func(tok string) bool { return parsedTokensCloser[tok] }) &&
				isParsedTokensSyntaxRun(head)
		},
	},
	{
		name:    "heredoc-body",
		example: "cat <<EOF\nbody $x\nEOF",
		reason:  "a here-document is read from the parser's Heredocs, not from its redirections, so its operator, body and delimiter are no tokens; shellTokenize reads the body as commands",
		covers: func(command string, b parsedTokensBlock) bool {
			at := parsedTokensHeredocOp(command)
			if len(b.parsed) > 0 || at < 0 {
				return false
			}
			text := parsedTokensNorm(command[at:])
			for _, tok := range b.shell {
				if !strings.Contains(text, parsedTokensNorm(tok)) {
					return false
				}
			}
			return true
		},
	},
	{
		name:    "escaped-semicolon",
		example: "echo a;\n\\;",
		reason:  "an escaped or quoted ';' is a word, but both readings emit it as a ';' token like a separator, so where a newline sits beside it the two counts differ",
		covers: func(command string, b parsedTokensBlock) bool {
			if !parsedTokensHasLiteralSemicolon(command) {
				return false
			}
			all := append(slices.Clone(b.shell), b.parsed...)
			return len(all) > 0 && !slices.ContainsFunc(all, func(tok string) bool { return tok != ";" })
		},
	},
	{
		name:    "line-continuation",
		example: "echo $\\\n'x'; y",
		reason:  "a backslash-newline joins the lines as the shell does, but shellTokenize drops the character before the continuation, here the '$' of $x",
		covers: func(command string, b parsedTokensBlock) bool {
			if !strings.Contains(command, "\\\n") || len(b.shell) != len(b.parsed) || len(b.shell) == 0 {
				return false
			}
			for i := range b.shell {
				if !strings.HasSuffix(b.parsed[i], b.shell[i]) {
					return false
				}
			}
			return true
		},
	},
	{
		name:    "substitution-split",
		example: "echo $(ls -la)",
		reason:  "shellTokenize splits a $(...) or <(...) at its blanks, so its words arrive as separate tokens; parsedTokens keeps the whole substitution as one word. Separators and parentheses inside it are not compared",
		covers: func(_ string, b parsedTokensBlock) bool {
			if !slices.ContainsFunc(b.parsed, func(tok string) bool {
				return slices.ContainsFunc(parsedTokensSubstitutionOpeners, func(o string) bool { return strings.Contains(tok, o) })
			}) {
				return false
			}
			return parsedTokensNorm(strings.Join(b.shell, "")) == parsedTokensNorm(strings.Join(b.parsed, ""))
		},
	},
	{
		name:    "ansi-c-quote",
		example: "echo $'a\\tb'",
		reason:  "shellTokenize leaves the escapes of a $'...' string undecoded; parsedTokens decodes them as the shell does",
		covers: func(command string, b parsedTokensBlock) bool {
			return strings.Contains(command, "$'") &&
				parsedTokensNorm(parsedTokensDecodeANSI(strings.Join(b.shell, ""))) == parsedTokensNorm(strings.Join(b.parsed, ""))
		},
	},
	{
		name:    "parameter-expansion-comment",
		example: "echo ${v:-a #<(make test)}",
		reason:  "shellTokenize cuts a ${...} at a '#', which it reads as a comment; parsedTokens keeps the whole parameter expansion",
		covers: func(_ string, b parsedTokensBlock) bool {
			return len(b.parsed) == 1 && strings.Contains(b.parsed[0], "${") &&
				len(b.shell) > 0 && strings.HasPrefix(b.parsed[0], strings.Join(b.shell, " "))
		},
	},
}

// parsedTokensClassOf returns the name of the block class that covers block b of
// command, or "" when no block class does.
func parsedTokensClassOf(command string, b parsedTokensBlock) string {
	for _, c := range parsedTokensClasses {
		if c.covers != nil && c.covers(command, b) {
			return c.name
		}
	}
	return ""
}

// parsedTokensApplyRewrites runs the rewrite classes over shell, in table order,
// and returns the stream they leave and the names of the classes that removed
// something.
func parsedTokensApplyRewrites(command string, shell, parsed []string) ([]string, []string) {
	var applied []string
	for _, c := range parsedTokensClasses {
		if c.rewrite == nil {
			continue
		}
		var changed bool
		shell, changed = c.rewrite(command, shell, parsed)
		if changed {
			applied = append(applied, c.name)
		}
	}
	return shell, applied
}

// parsedTokensFragments are the pieces the generated commands are built from:
// one or more of each kind the differential has to cover.
var parsedTokensFragments = []string{
	`"a b"`, `'a b'`, `"x\"y"`, `a\ b`, `\;`, `$'a\tb'`,
	`;`, `&&`, `||`, `|`, `&`, `2>&1`, `>f`, `&>f`, `>>f`, `< in`,
	`<<< "x y"`, `<<<x`, `$(ls -la)`, "`date`", `$(echo "a b")`, `<(ls)`,
	`# c`, `x #c ; rm`, `(x)`, `{ y; }`, `FOO=1`, `export Z=2`,
}

// parsedTokensGenerated returns a deterministic set of commands built from the
// fragments: each fragment alone after a command word, and each pair of
// fragments joined by a blank and by a newline.
func parsedTokensGenerated() []string {
	var out []string
	for _, f := range parsedTokensFragments {
		out = append(out, "echo "+f)
	}
	for _, f := range parsedTokensFragments {
		for _, g := range parsedTokensFragments {
			out = append(out, "echo "+f+" "+g, "echo "+f+"\n"+g)
		}
	}
	return out
}

// TestParsedTokens pins the token contract on commands the guards depend on.
// Each row is a command and the tokens parsedTokens returns for it, or that it
// is unreadable.
func TestParsedTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		command  string
		want     []string
		readable bool
	}{
		{"plain words", "rm -rf /tmp/x", []string{"rm", "-rf", "/tmp/x"}, true},
		{"separators are tokens", "echo a && b || c; d & e", []string{"echo", "a", "&&", "b", "||", "c", ";", "d", "&", "e"}, true},
		{"pipe is a token", "cat f | wc", []string{"cat", "f", "|", "wc"}, true},
		{"newline is a semicolon", "echo a\necho b", []string{"echo", "a", ";", "echo", "b"}, true},
		{"quoted separator stays in its word", `echo "a;b" && rm`, []string{"echo", "a;b", "&&", "rm"}, true},
		{"prefix assignments are tokens", "FOO=bar BAZ=1 env", []string{"FOO=bar", "BAZ=1", "env"}, true},
		{"empty assignment keeps its equals sign", "BD_ACTOR= bd x", []string{"BD_ACTOR=", "bd", "x"}, true},
		{"declaration argument follows its builtin", "export X=1", []string{"export", "X=1"}, true},
		{"redirection between prefix and declaration keeps source order", "x=1 >f export y=2", []string{"x=1", ">", "f", "export", "y=2"}, true},
		{"substitution with blanks is one word", "echo $(ls -la)", []string{"echo", "$(ls -la)"}, true},
		{"ansi-c quoting is decoded", `echo $'a\tb'`, []string{"echo", "a\tb"}, true},
		{"redirection is operator then target", "echo a > f", []string{"echo", "a", ">", "f"}, true},
		{"file descriptor stays with its operator", "echo a 2>f", []string{"echo", "a", "2>", "f"}, true},
		{"comment is not a token", "echo a # rm -rf /", []string{"echo", "a"}, true},
		{"unterminated quote is unreadable", `echo "unterminated`, nil, false},
		{"stray double semicolon is unreadable", "echo a;;", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, readable := parsedTokens(tt.command)
			if readable != tt.readable {
				t.Fatalf("parsedTokens(%q) readable = %v, want %v", tt.command, readable, tt.readable)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parsedTokens(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// TestParsedTokensDifferential compares parsedTokens with shellTokenize over the
// guard corpus and the generated commands. A command the two read the same, once
// the rewrite classes are applied, is fine. Each run of tokens they still read
// differently must be covered by a listed class. A run no class covers fails
// the test, and so does a listed class whose example does not show it.
func TestParsedTokensDifferential(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	var commands []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			commands = append(commands, c)
		}
	}
	for _, row := range loadGuardCorpus(t) {
		add(row.command)
	}
	for _, c := range parsedTokensGenerated() {
		add(c)
	}

	counts := map[string]int{}
	var identical, unreadable, rewritten int
	for _, command := range commands {
		parsed, readable := parsedTokens(command)
		if !readable {
			unreadable++
			continue
		}
		shell, applied := parsedTokensApplyRewrites(command, shellTokenize(command), parsed)
		for _, name := range applied {
			counts[name]++
		}
		if len(applied) > 0 {
			rewritten++
		}
		if slices.Equal(shell, parsed) {
			identical++
			continue
		}
		for _, b := range parsedTokensBlocks(shell, parsed) {
			class := parsedTokensClassOf(command, b)
			if class == "" {
				t.Errorf("unlisted difference in %q: shellTokenize run %q, parsedTokens run %q",
					command, b.shell, b.parsed)
				continue
			}
			counts[class]++
		}
	}
	t.Logf("%d commands: %d identical after rewrites, %d differ, %d unreadable",
		len(commands), identical, len(commands)-identical-unreadable, unreadable)
	t.Logf("%d commands need a rewrite", rewritten)
	for _, c := range parsedTokensClasses {
		t.Logf("class %s: %d", c.name, counts[c.name])
	}
	for _, c := range parsedTokensClasses {
		if !parsedTokensExampleShows(c) {
			t.Errorf("class %s: example %q does not show the difference", c.name, c.example)
		}
	}
}

// parsedTokensExampleShows reports whether the class's example command shows the
// class: a rewrite changes its shellTokenize stream, or a block is covered.
func parsedTokensExampleShows(c parsedTokensClass) bool {
	parsed, readable := parsedTokens(c.example)
	if !readable {
		return false
	}
	shell := shellTokenize(c.example)
	if c.rewrite != nil {
		_, changed := c.rewrite(c.example, shell, parsed)
		return changed
	}
	stream, _ := parsedTokensApplyRewrites(c.example, shell, parsed)
	for _, b := range parsedTokensBlocks(stream, parsed) {
		if c.covers(c.example, b) {
			return true
		}
	}
	return false
}
