package specdispatch

import (
	"fmt"
	"regexp"
	"strings"
)

// riskPathWarnMin is the number of risk paths a bead's text may name before
// the lint warns. Three or more is the share of beads the rework program
// measured at three times the median time; the rule is one risk path per bead
// (docs/design/rework-program.md, lever 2).
const riskPathWarnMin = 3

// sizeOkLabel is the overseer's waiver of a bead's size: a bead wearing it is
// one the overseer has looked at and accepted, so the risk-path warning is not
// repeated on it. The lint reads the same label in the story-point size gate,
// which passes a bead wearing it whatever its sp-minutes label says
// (gt-d4uh4f, gt-mtzx6s).
const sizeOkLabel = "size-ok"

// riskPathWarnText is the one WARN line a bead naming riskPathWarnMin or more
// risk paths gets: the count, the paths and the rule.
const riskPathWarnText = "%d risk paths named (one risk path per bead): %s"

// riskPathsUnreadText is the WARN line a bead whose risk list could not be read
// gets. The check did not run, and a check that did not run must not read as
// one that passed.
const riskPathsUnreadText = "the risk-path check did not run: %s"

// riskPathWarnings returns the WARN lines s earns: one when its text names
// riskPathWarnMin or more paths on the target branch's risk list, one when
// that list could not be read. A bead labeled size-ok is not warned. The
// lines change neither the route nor the exit code; risk paths are advisory
// because the risk list holds paths a landing should review, not paths it
// must split.
func riskPathWarnings(s Spec) []string {
	if s.RiskPathsErr != "" {
		return []string{fmt.Sprintf(riskPathsUnreadText, s.RiskPathsErr)}
	}
	if len(s.RiskPaths) < riskPathWarnMin || s.HasLabel(sizeOkLabel) {
		return nil
	}
	return []string{fmt.Sprintf(riskPathWarnText, len(s.RiskPaths), strings.Join(s.RiskPaths, ", "))}
}

// isPathTokenDelim reports whether r separates one path token from the next in
// a bead's prose: whitespace, the punctuation prose wraps a path in, and the
// list separators a Constraints bullet uses. A glob character is a separator
// too, so "internal/daemon/**" comes back as the package it names.
func isPathTokenDelim(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', ',', ';', '(', ')', '[', ']', '{', '}',
		'<', '>', '"', '\'', '`', '|', '*', '?', '=', '!':
		return true
	}
	return false
}

// pathLineRefRe matches the ":<line>" (or ":<line>:<col>") a citation appends
// to a path, so "spec.go:42" is read as spec.go.
var pathLineRefRe = regexp.MustCompile(`:\d+(?::\d+)?$`)

// pathTokenRe matches a token shaped like a repo file or package name: letters,
// digits, dots, slashes and dashes, with an optional leading dot. It is the
// whole-token check; whether the name is a real repo path is the risk list
// matcher's question.
var pathTokenRe = regexp.MustCompile(`^\.?[A-Za-z0-9_][A-Za-z0-9_./-]*$`)

// PathTokens returns the tokens of text that could name a repo file or
// package, in first-seen order and without duplicates. The tokens are read from
// prose, so an ordinary word comes back too: matching the tokens against the
// risk list is what tells the two apart, and the caller does that with
// internal/land's matcher, which this package cannot import (land imports
// specdispatch).
//
// A token is kept only when it carries no glob character, no URL or option
// punctuation, no absolute or parent path and no empty segment. A leading
// "./" and a trailing "/" are trimmed, since both name the same repo path a
// bare name does.
func PathTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(text, isPathTokenDelim) {
		tok := strings.TrimRight(raw, ".,;:")
		tok = strings.TrimRight(pathLineRefRe.ReplaceAllString(tok, ""), ".,;:")
		tok = strings.TrimPrefix(tok, "./")
		tok = strings.TrimSuffix(tok, "/")
		if tok == "" || seen[tok] || !pathTokenRe.MatchString(tok) || !pathSegmentsOK(tok) {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// pathSegmentsOK reports whether every segment of a path token is a real one:
// no empty segment, no "." and no "..".
func pathSegmentsOK(tok string) bool {
	for _, seg := range strings.Split(tok, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
