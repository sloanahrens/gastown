package cmd

import (
	"path/filepath"
	"strings"
)

// The accepted-risk gate (internal/land/criteria.go) trusts a bead comment's
// free-text author, and `bd comments add --author` takes any value. These two
// rules close a polecat's way to write such a comment at the tool layer where
// its commands are already vetted (gt-z71j0): it may not name a comment's
// author or the acting identity, and it may not post an ACCEPTED-RISK record
// at all. A prompt-injected or over-eager polecat is the threat; a deliberate
// bypass of a hook by the same OS user is out of scope.
//
// The block banner cuts a reason to one line, so each reason names the rule and
// the explanation rides in the alternative, which prints in full.
const (
	polecatAttributionReason = "polecat may not set a comment author or actor"
	polecatAttributionAlt    = "The accepted-risk gate trusts a comment's author name, so --author, -a and --actor " +
		"are refused for a polecat. Post the comment without them."
	polecatAcceptedRiskReason = "polecat may not post an ACCEPTED-RISK: comment"
	polecatAcceptedRiskAlt    = "An accepted risk is recorded by the overseer or Sloan, not by a polecat. " +
		"Describe the finding in a plain bead comment and let them decide whether to accept it."
)

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

// matchesPolecatAttribution applies the two rules to every bd and gt
// invocation in tokens when the session is a polecat's. tokens come from
// shellTokenize on the heredoc-stripped command, so quoted text is one token
// and a chain (&&, ;, |) is judged one segment at a time.
func matchesPolecatAttribution(tokens []string, polecatSession bool) (reason, alternative string) {
	if !polecatSession {
		return "", ""
	}
	for _, segment := range splitShellSegments(tokens) {
		words := bdCloseCommandTokens(segment)
		if len(words) < 2 {
			continue
		}
		var r string
		switch {
		case isBdCommandWord(words[0]):
			r = polecatBdViolation(words[1:])
		case filepath.Base(words[0]) == "gt":
			r = polecatGtViolation(words[1:])
		}
		switch r {
		case polecatAttributionReason:
			return r, polecatAttributionAlt
		case polecatAcceptedRiskReason:
			return r, polecatAcceptedRiskAlt
		}
	}
	return "", ""
}

// polecatBdViolation judges one bd invocation, rest being the tokens after bd.
func polecatBdViolation(rest []string) string {
	sub := bdSubcommandIndex(rest)
	if sub < 0 {
		return ""
	}
	verb, args := rest[sub], rest[sub+1:]
	adds := bdCommentVerbs[verb] || (verb == "comments" && len(args) > 0 && args[0] == "add")
	// --actor is a global flag, so it may sit before or after the verb.
	writes := !bdReadOnlyVerbs[verb] && (verb != "comments" || adds)
	if writes {
		for _, tok := range rest {
			if tok == "--actor" || strings.HasPrefix(tok, "--actor=") {
				return polecatAttributionReason
			}
		}
	}
	if adds {
		for _, tok := range args {
			if tok == "--author" || strings.HasPrefix(tok, "--author=") || (strings.HasPrefix(tok, "-a") && !strings.HasPrefix(tok, "--")) {
				return polecatAttributionReason
			}
		}
		if anyTokenOpensAcceptedRisk(args) {
			return polecatAcceptedRiskReason
		}
	}
	if verb == "update" {
		for i, tok := range args {
			for _, flag := range []string{"--append-notes", "--notes"} {
				value := ""
				switch {
				case tok == flag && i+1 < len(args):
					value = args[i+1]
				case strings.HasPrefix(tok, flag+"="):
					value = strings.TrimPrefix(tok, flag+"=")
				}
				if opensAcceptedRisk(value) {
					return polecatAcceptedRiskReason
				}
			}
		}
	}
	return ""
}

// polecatGtViolation judges one gt invocation, rest being the tokens after gt:
// `gt bead comment <id> <text>` and `gt bead note <id> <text>`.
func polecatGtViolation(rest []string) string {
	if len(rest) < 3 || rest[0] != "bead" || (rest[1] != "comment" && rest[1] != "note") {
		return ""
	}
	if anyTokenOpensAcceptedRisk(rest[2:]) {
		return polecatAcceptedRiskReason
	}
	return ""
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
