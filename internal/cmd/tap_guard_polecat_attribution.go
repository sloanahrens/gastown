package cmd

import (
	"path/filepath"
	"slices"
	"strings"
)

// The accepted-risk gate (internal/land/criteria.go) trusts a comment's
// free-text author, so the tool layer keeps a polecat from naming that author,
// setting the acting identity, or recording an accepted risk at all: the
// overseer or Sloan records one (gt-z71j0). A polecat's ordinary comment is
// untouched.
const (
	polecatAttributionReason = "polecat may not set a comment author or actor"
	polecatAttributionAlt    = "The accepted-risk gate trusts a comment's author name, so --author, -a and --actor " +
		"are refused for a polecat, and so is assigning BD_ACTOR or BEADS_ACTOR to another identity. " +
		"Post the comment without them."
	polecatAcceptedRiskReason = "polecat may not post an ACCEPTED-RISK: comment"
	polecatAcceptedRiskAlt    = "An accepted risk is recorded by the overseer or Sloan, not by a polecat. " +
		"Describe the finding in a plain bead comment and let them decide whether to accept it."
	polecatCommentInputReason = "polecat comment text must be inline"
	polecatCommentInputAlt    = "Text read from a file or stdin cannot be checked for an ACCEPTED-RISK: record, " +
		"so post the comment inline."
)

// polecatRule is the rule an invocation breaks; the zero value breaks none.
// The reason text is chosen from the rule rather than parsed back out of it.
type polecatRule int

const (
	polecatRuleOK polecatRule = iota
	polecatRuleAttribution
	polecatRuleAcceptedRisk
	polecatRuleCommentInput
)

// text returns the banner reason and the alternative printed beneath it. The
// block banner cuts the reason to one line, so the explanation rides in the
// alternative, which prints in full.
func (r polecatRule) text() (reason, alternative string) {
	switch r {
	case polecatRuleAttribution:
		return polecatAttributionReason, polecatAttributionAlt
	case polecatRuleAcceptedRisk:
		return polecatAcceptedRiskReason, polecatAcceptedRiskAlt
	case polecatRuleCommentInput:
		return polecatCommentInputReason, polecatCommentInputAlt
	}
	return "", ""
}

// bdReadOnlyVerbs are bd subcommands that write nothing, so --actor on them
// names no one. Any other verb, including one bd adds later, is a write verb.
var bdReadOnlyVerbs = map[string]bool{
	"show": true, "list": true, "ready": true, "blocked": true, "search": true,
	"count": true, "stats": true, "status": true, "query": true, "children": true,
	"version": true, "help": true, "prime": true, "info": true,
}

// bdCommentVerbs are the bd subcommands that add a bead comment or note from
// their text arguments; `comments` does so only as `comments add`.
var bdCommentVerbs = map[string]bool{"comment": true, "note": true}

// bdNoteFlags name the flags that carry a bead's notes; bd create and bd update
// both take them, and a bead's notes are the other place it records a decision.
var bdNoteFlags = []string{"--notes", "--append-notes"}

// matchesPolecatAttribution applies the rules to every bd and gt invocation in
// tokens when the session is a polecat's. tokens come from shellTokenize on the
// heredoc-stripped command, so quoted text is one token and a chain (&&, ;, |)
// is judged one segment at a time. polecatSession is passed in rather than read
// here so the matcher stays table-testable, as is ownIdentities, the values of
// the polecat's own identity that BD_ACTOR may be set to.
func matchesPolecatAttribution(tokens []string, polecatSession bool, ownIdentities []string) (reason, alternative string) {
	if !polecatSession {
		return "", ""
	}
	for _, segment := range splitShellSegments(tokens) {
		if assignsOtherActor(segment, ownIdentities) {
			return polecatRuleAttribution.text()
		}
		words := bdCloseCommandTokens(segment)
		if len(words) < 2 {
			continue
		}
		var rule polecatRule
		switch {
		case isBdCommandWord(words[0]):
			rule = polecatBdViolation(words[1:])
		case filepath.Base(words[0]) == "gt":
			rule = polecatGtViolation(words[1:])
		}
		if reason, alternative := rule.text(); reason != "" {
			return reason, alternative
		}
	}
	return "", ""
}

// bdActorEnvVars are the environment variables bd resolves a write's author
// from when no --actor flag is given.
var bdActorEnvVars = []string{"BD_ACTOR", "BEADS_ACTOR"}

// envAssignmentWords are the words that may sit before a NAME=value assignment
// and still apply it: export and its kin set it for the shell, command for the
// command that follows.
var envAssignmentWords = map[string]bool{
	"export": true, "declare": true, "typeset": true, "readonly": true, "local": true, "command": true,
}

// assignsOtherActor reports whether a segment assigns BD_ACTOR or BEADS_ACTOR
// to anything but the polecat's own identity, as a leading NAME=value word
// (`BD_ACTOR=x bd ...`, a bare `BD_ACTOR=x`) or behind export, declare, env and
// the like. An empty value counts as another identity: bd then falls back to
// the git user. Only the assignment words at the front of the segment are read,
// so comment text that mentions BD_ACTOR=x is left alone.
func assignsOtherActor(segment, ownIdentities []string) bool {
	tokens := segment
	for len(tokens) > 0 {
		tok := tokens[0]
		switch {
		case bdCloseLeadWords[tok] || envAssignmentWords[tok]:
			tokens = tokens[1:]
		case filepath.Base(tok) == "env":
			tokens = bdCloseSkipEnvFlags(tokens[1:])
		case strings.HasPrefix(tok, "-") && tok != "--":
			// declare -x, export -n: flags of the assignment word.
			tokens = tokens[1:]
		case isEnvAssignment(tok):
			name, value, _ := strings.Cut(tok, "=")
			if slices.Contains(bdActorEnvVars, name) && !isOwnIdentity(value, ownIdentities) {
				return true
			}
			tokens = tokens[1:]
		default:
			return false
		}
	}
	return false
}

// isOwnIdentity reports whether value is one of the polecat's own identities.
func isOwnIdentity(value string, own []string) bool {
	value = strings.TrimSpace(value)
	return value != "" && slices.Contains(own, value)
}

// polecatOwnIdentities are the values the session itself carries for who it is:
// its GT_ROLE and the BD_ACTOR or BEADS_ACTOR it was started with.
func polecatOwnIdentities(proc guardProcess) []string {
	var own []string
	for _, key := range append([]string{"GT_ROLE"}, bdActorEnvVars...) {
		if v := strings.TrimSpace(proc.getenv(key)); v != "" {
			own = append(own, v)
		}
	}
	return own
}

// polecatBdViolation judges one bd invocation, rest being the tokens after bd.
func polecatBdViolation(rest []string) polecatRule {
	sub := bdSubcommandIndex(rest)
	if sub < 0 {
		return polecatRuleOK
	}
	verb, args := rest[sub], rest[sub+1:]
	comment := bdWritesComment(verb, args)
	// --actor is a global flag, so it may sit before or after the verb.
	if bdVerbWrites(verb, comment) && bdFlagsNameActor(rest) {
		return polecatRuleAttribution
	}
	if comment {
		for _, tok := range bdFlagTokens(args) {
			switch bdCommentFlag(tok) {
			case bdAuthorFlag:
				return polecatRuleAttribution
			case bdCommentInputFlag:
				return polecatRuleCommentInput
			}
		}
		// The bead id, the flags and the text are all checked here: the text
		// may sit anywhere among them and a quoted one is a single token.
		if anyTokenOpensAcceptedRisk(args) {
			return polecatRuleAcceptedRisk
		}
	}
	// bd create and bd update take notes through a flag, in the `--notes=X` and
	// `--notes X` forms alike, and a note reads the same to a reviewer as a
	// comment does.
	flags := bdFlagTokens(args)
	for i, tok := range flags {
		for _, flag := range bdNoteFlags {
			switch {
			case tok == flag && i+1 < len(flags):
				if opensAcceptedRisk(flags[i+1]) {
					return polecatRuleAcceptedRisk
				}
			case strings.HasPrefix(tok, flag+"="):
				if opensAcceptedRisk(strings.TrimPrefix(tok, flag+"=")) {
					return polecatRuleAcceptedRisk
				}
			}
		}
	}
	return polecatRuleOK
}

// polecatGtViolation judges one gt invocation, rest being the tokens after gt:
// `gt bead comment <id> <text>` and `gt bead note <id> <text>`, which take their
// text as arguments and no flags of their own.
func polecatGtViolation(rest []string) polecatRule {
	if len(rest) < 3 || rest[0] != "bead" || (rest[1] != "comment" && rest[1] != "note") {
		return polecatRuleOK
	}
	if anyTokenOpensAcceptedRisk(rest[2:]) {
		return polecatRuleAcceptedRisk
	}
	return polecatRuleOK
}

// bdWritesComment reports whether a bd invocation adds a comment or note to a
// bead: the singular shorthands, or `comments` followed by its `add` action.
func bdWritesComment(verb string, args []string) bool {
	if bdCommentVerbs[verb] {
		return true
	}
	if verb != "comments" {
		return false
	}
	action := bdSubcommandIndex(args)
	return action >= 0 && args[action] == "add"
}

// bdVerbWrites reports whether a bd invocation writes anything. A verb the
// lists do not name counts as a write, so one bd adds later is covered.
func bdVerbWrites(verb string, writesComment bool) bool {
	if bdReadOnlyVerbs[verb] {
		return false
	}
	if verb == "comments" {
		return writesComment
	}
	return true
}

// bdFlagsNameActor reports whether the invocation passes bd's --actor, which
// attributes a write, or --author, which names a comment's author. Only the
// long spellings are read here: -a is the assignee on most bd write verbs and
// the author only on the comment ones, where bdCommentFlag reads it.
func bdFlagsNameActor(rest []string) bool {
	for _, tok := range bdFlagTokens(rest) {
		switch {
		case tok == "--actor", strings.HasPrefix(tok, "--actor="):
			return true
		case tok == "--author", strings.HasPrefix(tok, "--author="):
			return true
		}
	}
	return false
}

// The kinds of comment flag the guard reads.
const (
	bdAuthorFlag       = "author"
	bdCommentInputFlag = "input"
)

// bdCommentFlag names the comment flag a token sets, or "" for none. Comments
// add's own shorthands are a (author) and f (file), and bd's global C
// (directory) takes a value too, so a letter after one of those is that flag's
// value and not a flag of its own: -aq is `-a q`, -qa and -qaV are the author
// flag bundled behind a boolean.
func bdCommentFlag(tok string) string {
	if strings.HasPrefix(tok, "--") {
		name, _, _ := strings.Cut(tok, "=")
		switch name {
		case "--author":
			return bdAuthorFlag
		case "--file", "--stdin":
			return bdCommentInputFlag
		}
		return ""
	}
	if len(tok) < 2 || tok[0] != '-' {
		return ""
	}
	for _, c := range tok[1:] {
		switch c {
		case 'a':
			return bdAuthorFlag
		case 'f':
			return bdCommentInputFlag
		case 'C', '=':
			// The rest of the token is this flag's value.
			return ""
		}
	}
	return ""
}

// bdFlagTokens drops everything from a `--` terminator on: those arguments are
// positional text, so a comment whose text is "-a note" is not the author flag.
func bdFlagTokens(tokens []string) []string {
	for i, tok := range tokens {
		if tok == "--" {
			return tokens[:i]
		}
	}
	return tokens
}

// anyTokenOpensAcceptedRisk reports whether any token is, or opens, an
// ACCEPTED-RISK: record. Comment text is the arguments that follow the bead id,
// quoted as one token or split into words, and a flag's value may sit among
// them, so each token is checked rather than guessing which one is the text.
func anyTokenOpensAcceptedRisk(tokens []string) bool {
	for _, tok := range tokens {
		if opensAcceptedRisk(tok) {
			return true
		}
	}
	return false
}

// opensAcceptedRisk reports whether text begins with the ACCEPTED-RISK: marker
// after trimming, in any case.
func opensAcceptedRisk(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "accepted-risk:")
}
