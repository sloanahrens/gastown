package cmd

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/shellparse"
)

// parsedTokens is the shellparse-backed twin of shellTokenize. It returns the
// argv-like tokens of command that shellTokenize returns for it, read through the
// parser instead of shlex: a separator (";", "&&", "||", "|", "&") is a token of
// its own, quoting is resolved, a prefix assignment is a token, and a redirection
// is its operator (with its file descriptor) and then its target. A substitution
// with blanks in it stays one token, and an ANSI-C $'...' string is decoded.
// Where the two still differ, TestParsedTokensDifferential lists the class.
//
// readable is false when shellparse cannot read command. The tokens are then
// nil, and the caller keeps its own fail-closed reading. Nothing calls
// parsedTokens yet: the guards move to it one at a time, a bead per guard, and
// that retirement is planned in gt-crn9gu.
func parsedTokens(command string) (tokens []string, readable bool) {
	script, err := shellparse.Parse(command)
	if err != nil {
		return nil, false
	}
	for _, seg := range script.Segments {
		tokens = append(tokens, parsedSegmentTokens(command, seg)...)
		if seg.After != "" {
			tokens = append(tokens, seg.After)
		}
	}
	return tokens, true
}

// parsedSegmentTokens returns the tokens of one command site: the prefix
// assignments, then its words and redirections in source order, then the
// arguments of a declaration builtin. A redirection the parser marks Inherited
// belongs to an enclosing compound command, and is left out here (see the
// TestParsedTokensDifferential class compound-redirection).
//
// The split of prefix assignments from declaration arguments follows
// sandboxSecretsAssignSplit. It is copied here (gt-8sg8to) rather than called,
// because that walk belongs to the sandbox-secrets guard, which is retired on
// its own bead.
func parsedSegmentTokens(command string, seg shellparse.Segment) []string {
	prefix, args := parsedAssignSplit(command, seg)
	var tokens []string
	for _, a := range prefix {
		tokens = append(tokens, parsedAssignToken(a))
	}
	type item struct {
		at     int
		tokens []string
	}
	var items []item
	for _, w := range seg.Words {
		items = append(items, item{w.Src, []string{w.Text}})
	}
	for _, r := range seg.Redirs {
		if r.Inherited {
			continue
		}
		items = append(items, item{r.OpSrc, []string{r.N + r.Op, r.Word.Text}})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at < items[j].at })
	for _, it := range items {
		tokens = append(tokens, it.tokens...)
	}
	for _, a := range args {
		tokens = append(tokens, parsedAssignToken(a))
	}
	return tokens
}

// parsedDeclarationBuiltins are the builtins the parser reads as a declaration
// clause when they lead a command: their arguments come back as assignments.
var parsedDeclarationBuiltins = map[string]bool{
	"export": true, "declare": true, "typeset": true, "local": true, "readonly": true,
}

// parsedAssignSplit splits a segment's assignments into the prefix the shell
// sets for its command and the arguments of a declaration builtin. The parser
// gives an assignment no source position, so the text before the command word
// decides which is which. When that text holds anything else, every assignment
// is taken as a prefix.
func parsedAssignSplit(command string, seg shellparse.Segment) (prefix, args []shellparse.Word) {
	if len(seg.Words) == 0 || !parsedDeclarationBuiltins[strings.ToLower(filepath.Base(seg.Words[0].Text))] {
		return seg.Assigns, nil
	}
	at := seg.Words[0].Src
	if at < 0 || at > len(command) {
		return seg.Assigns, nil
	}
	head := parsedMaskSubs(command[:at], seg.Subs)
	if i := strings.LastIndexAny(head, ";&|\n()`"); i >= 0 {
		head = head[i+1:]
	}
	for k := len(seg.Assigns); k >= 0; k-- {
		if parsedHeadIs(head, seg.Assigns[:k], seg.Subs) {
			return seg.Assigns[:k], seg.Assigns[k:]
		}
	}
	return seg.Assigns, nil
}

// parsedHeadIs reports whether head is exactly the source text of the
// assignments, in order and separated by blanks.
func parsedHeadIs(head string, assigns []shellparse.Word, subs []shellparse.Script) bool {
	rest := head
	for _, a := range assigns {
		rest = strings.TrimLeft(rest, " \t")
		raw := parsedMaskSubs(a.Raw, subs)
		if !strings.HasPrefix(rest, raw) {
			return false
		}
		rest = rest[len(raw):]
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			return false
		}
	}
	return strings.TrimSpace(rest) == ""
}

// parsedMaskSubs replaces each copy of a substitution in subs, written as $(...)
// or `...`, with a placeholder that holds no separator or parenthesis, so a cut
// at one cannot land inside a substitution.
func parsedMaskSubs(text string, subs []shellparse.Script) string {
	for _, sub := range subs {
		text = strings.ReplaceAll(text, "$("+sub.Raw+")", parsedSubstitutionMask)
		text = strings.ReplaceAll(text, "`"+sub.Raw+"`", parsedSubstitutionMask)
	}
	return text
}

// parsedSubstitutionMask stands in for a substitution while a head is compared.
const parsedSubstitutionMask = "\x00"

// parsedAssignToken returns an assignment's token: its text, with the "=" the
// parser puts in front of an argument that has no name removed. The parser drops
// the "=" of an assignment with an empty value from its text ("X=" reads as "X"),
// so it is put back from the source.
func parsedAssignToken(a shellparse.Word) string {
	text := strings.TrimPrefix(a.Text, "=")
	if !strings.Contains(text, "=") && strings.HasSuffix(a.Raw, "=") {
		text += "="
	}
	return text
}
