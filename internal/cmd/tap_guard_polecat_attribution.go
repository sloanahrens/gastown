package cmd

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/cmdtree"
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
	polecatUnresolvedTextReason = "polecat comment text must not expand a variable or substitution"
	polecatUnresolvedTextAlt    = "The shell expands $VAR, $(...) and backticks before bd sees the text, so an " +
		"ACCEPTED-RISK: record could be built at run time and cannot be checked. Write the comment text out literally."
)

// acceptedRiskMarker is the opener the accepted-risk gate reads, lower case
// because the guard compares case-insensitively.
const acceptedRiskMarker = "accepted-risk:"

// polecatRule is the rule an invocation breaks; the zero value breaks none.
// The reason text is chosen from the rule rather than parsed back out of it.
type polecatRule int

const (
	polecatRuleOK polecatRule = iota
	polecatRuleAttribution
	polecatRuleAcceptedRisk
	polecatRuleCommentInput
	polecatRuleUnresolvedText
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
	case polecatRuleUnresolvedText:
		return polecatUnresolvedTextReason, polecatUnresolvedTextAlt
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
		words := polecatCommandWords(segment)
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

// execValueFlags names the flag that takes a value among exec's: -a NAME sets
// the argv[0] the command runs under.
var execValueFlags = map[string]bool{"-a": true}

// polecatCommandWords returns the command word and its arguments for a segment.
// bdCloseCommandTokens looks through the launchers, but not through `command`,
// `builtin` or `exec` with their flags, and it drops exec's -a NAME as a flag
// and leaves NAME as the command word. So those prefixes are peeled here first,
// and the walk repeats until neither pass moves, so `exec -a x command bd` is
// read through too. Without this, `command bd comments add --author X` was
// judged as a command named "command" and allowed (gt-v0u69).
func polecatCommandWords(segment []string) []string {
	words := segment
	for {
		next := bdCloseCommandTokens(skipShellPrefixes(words))
		if len(next) == len(words) {
			return next
		}
		words = next
	}
}

// skipShellPrefixes drops `command`, `builtin` and `exec` from the front of
// tokens along with their flags and, for exec, the value of -a, and drops the
// NAME=value words between them. The prefix word is tested before any env
// assignment is stripped, because stripLeadingEnvAssignments drops a bare
// `command` on its own and would leave its -p flag behind as the command word.
// Each pass removes a token or returns, so the loop ends.
func skipShellPrefixes(tokens []string) []string {
	for len(tokens) > 0 {
		switch filepath.Base(tokens[0]) {
		case "command", "builtin", "exec":
			launcher := filepath.Base(tokens[0])
			tokens = tokens[1:]
			for len(tokens) > 0 && strings.HasPrefix(tokens[0], "-") {
				flag := tokens[0]
				tokens = tokens[1:]
				if flag == "--" {
					break
				}
				if launcher == "exec" && execValueFlags[flag] && len(tokens) > 0 {
					tokens = tokens[1:]
				}
			}
			continue
		}
		if !isEnvAssignment(tokens[0]) {
			return tokens
		}
		tokens = tokens[1:]
	}
	return tokens
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
// the git user, and so does a variable the segment unsets or removes with env
// -u, or an env -i that launches bd or gt, so those count too. Only the assignment words at the front of the segment
// are read, so comment text that mentions BD_ACTOR=x is left alone.
func assignsOtherActor(segment, ownIdentities []string) bool {
	tokens := segment
	for len(tokens) > 0 {
		tok := tokens[0]
		switch {
		case bdCloseLeadWords[tok] || envAssignmentWords[tok]:
			tokens = tokens[1:]
		case tok == "unset":
			// unset launches nothing, so no word after its names is read.
			return unsetsActor(tokens[1:])
		case filepath.Base(tok) == "env":
			rest, eff := readEnvFlags(tokens[1:])
			if eff.unsetsActor || (eff.clearsAll && launchesWithoutActor(rest, ownIdentities)) {
				return true
			}
			tokens = rest
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

// unsetsActor reports whether the names given to unset include an actor
// variable. Its flags (-v, -f, -n) name no variable and are skipped.
func unsetsActor(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") && slices.Contains(bdActorEnvVars, arg) {
			return true
		}
	}
	return false
}

// envEffect is what env's own flags do to the environment of the command it
// launches: remove all of it (-i, -, --ignore-environment) or remove an actor
// variable by name (-u NAME). bd then runs with no actor set and falls back to
// the git user.
type envEffect struct{ clearsAll, unsetsActor bool }

// envShortValueFlags and envLongValueFlags are env's flags that take a value:
// bdCloseEnvValueFlags plus GNU env's -a and --argv0.
const envShortValueFlags = "uCSPa"

var envLongValueFlags = map[string]bool{
	"--unset": true, "--chdir": true, "--split-string": true, "--argv0": true,
}

// readEnvFlags reads env's flags off the front of args, the part of the command
// line after the word env, and returns what is left (the NAME=value words and
// the command) with the effect of the flags. A value is read in every form env
// takes it: -u NAME, -uNAME, --unset NAME and --unset=NAME, and in a cluster
// such as -iu NAME. -S STRING is a command line of its own, so its words are
// read in place, as env does.
func readEnvFlags(args []string) ([]string, envEffect) {
	var eff envEffect
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch {
		case arg == "--":
			return args, eff
		case arg == "-":
			eff.clearsAll = true
		case strings.HasPrefix(arg, "--"):
			name, value, attached := strings.Cut(arg, "=")
			if name == "--ignore-environment" {
				eff.clearsAll = true
			}
			if !envLongValueFlags[name] {
				continue
			}
			if !attached && len(args) > 0 {
				value, args = args[0], args[1:]
			}
			args = eff.applyValue(name == "--unset", name == "--split-string", value, args)
		case strings.HasPrefix(arg, "-"):
			for i := 1; i < len(arg); i++ {
				if arg[i] == 'i' {
					eff.clearsAll = true
				}
				if !strings.ContainsRune(envShortValueFlags, rune(arg[i])) {
					continue
				}
				value := arg[i+1:]
				if value == "" && len(args) > 0 {
					value, args = args[0], args[1:]
				}
				args = eff.applyValue(arg[i] == 'u', arg[i] == 'S', value, args)
				break
			}
		default:
			return append([]string{arg}, args...), eff
		}
	}
	return args, eff
}

// applyValue records the value of a value-taking env flag: the variable an
// unset names, or the command line a split-string carries, which is put back in
// front of args to be read as the flags and words it is.
func (e *envEffect) applyValue(isUnset, isSplit bool, value string, args []string) []string {
	switch {
	case isUnset && slices.Contains(bdActorEnvVars, value):
		e.unsetsActor = true
	case isSplit:
		return append(strings.Fields(value), args...)
	}
	return args
}

// launchesWithoutActor reports whether an env that cleared the environment
// launches bd or gt, which then find no actor and fall back to the git user,
// unless a NAME=value word before the command sets the polecat's own identity.
// rest is the words env was given after its flags.
func launchesWithoutActor(rest, ownIdentities []string) bool {
	for len(rest) > 0 && isEnvAssignment(rest[0]) {
		name, value, _ := strings.Cut(rest[0], "=")
		if slices.Contains(bdActorEnvVars, name) && isOwnIdentity(value, ownIdentities) {
			return false
		}
		rest = rest[1:]
	}
	words := polecatCommandWords(rest)
	return len(words) > 0 && (isBdCommandWord(words[0]) || filepath.Base(words[0]) == "gt")
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
		// The text may sit anywhere among the flags and a quoted one is a
		// single token, so a literal opener is looked for in every token; only
		// the bead id and a global flag's value are exempt from the expansion
		// test.
		commentArgs := args
		if verb == "comments" {
			// bdWritesComment found the add action, so it is there to drop.
			commentArgs = args[bdSubcommandIndex(args)+1:]
		}
		if rule := commentRule(commentArgs, true); rule != polecatRuleOK {
			return rule
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
				if rule := noteFlagRule(flags[i+1]); rule != polecatRuleOK {
					return rule
				}
			case strings.HasPrefix(tok, flag+"="):
				if rule := noteFlagRule(strings.TrimPrefix(tok, flag+"=")); rule != polecatRuleOK {
					return rule
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
	return commentRule(rest[2:], false)
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

// commentRule judges the arguments of a comment or note write, args being the
// tokens after the verb: the bead id, the flags and the text, in any order. A
// literal ACCEPTED-RISK: opener in any token is refused as such, since the text
// may be split into words and sit anywhere among the flags. A token whose
// expansion could build the opener is refused as unresolved, since the shell
// fills it in before bd sees the text; but only a token that can be the text is
// tested, because `bd comments add $ID "note"` is an ordinary comment. withFlags
// is whether the tokens can carry bd flags; gt bead comment and gt bead note
// take none.
func commentRule(args []string, withFlags bool) polecatRule {
	for _, tok := range args {
		if opensAcceptedRisk(tok) {
			return polecatRuleAcceptedRisk
		}
	}
	for _, tok := range commentTextTokens(args, withFlags) {
		if expansionMayOpenAcceptedRisk(tok) {
			return polecatRuleUnresolvedText
		}
	}
	return polecatRuleOK
}

// commentTextTokens returns the tokens of a comment's arguments that can be its
// text: everything but the flags, the value of a global flag that takes one
// (`--db $DB`) and the first positional, which is the bead id. Every token after
// a `--` is positional. A flag bd does not list as taking a value leaves the
// token behind it in, since that token may be the text.
func commentTextTokens(args []string, withFlags bool) []string {
	var text []string
	sawID, positional := false, false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case !positional && tok == "--":
			positional = true
		case withFlags && !positional && strings.HasPrefix(tok, "-"):
			if cmdtree.IsBdGlobalValueFlag(tok) {
				i++
			}
		case !sawID:
			sawID = true
		default:
			text = append(text, tok)
		}
	}
	return text
}

// noteFlagRule judges the value of a --notes or --append-notes flag, all of
// which is text.
func noteFlagRule(value string) polecatRule {
	switch {
	case opensAcceptedRisk(value):
		return polecatRuleAcceptedRisk
	case expansionMayOpenAcceptedRisk(value):
		return polecatRuleUnresolvedText
	}
	return polecatRuleOK
}

// expansionMayOpenAcceptedRisk reports whether the text of a token, once the
// shell has expanded its $VAR, ${VAR}, $(...) and backtick expansions, or its
// brace expansions ({a,b} and {a..b}), could begin with the ACCEPTED-RISK:
// marker. The literal text before the first expansion decides it: an expansion
// can supply anything, so only literal text that cannot grow into the marker
// rules the token out. Quoting is gone by the time a token is read, so a
// single-quoted $ or brace is refused too; that is the safe side of the same
// test. A brace group yields each of its alternatives, so the token is judged
// by each alternative's text in turn.
func expansionMayOpenAcceptedRisk(tok string) bool {
	at := strings.IndexAny(tok, "$`")
	if open, alts, ok := firstBraceExpansion(tok); ok && (at < 0 || open < at) {
		for _, alt := range alts {
			// An alternative may carry the whole marker, which is no prefix of it.
			if lit := tok[:open] + alt; markerCompatible(lit) || opensAcceptedRisk(lit) {
				return true
			}
		}
		return false
	}
	return at >= 0 && markerCompatible(tok[:at])
}

// markerCompatible reports whether literal text, read up to its first
// expansion, could grow into the marker. Text that is not a prefix of the
// marker can never open one.
func markerCompatible(lit string) bool {
	if i := strings.IndexAny(lit, "$`{"); i >= 0 {
		lit = lit[:i]
	}
	lead := strings.ToLower(strings.TrimLeft(lit, " \t\r\n"))
	return strings.HasPrefix(acceptedRiskMarker, lead)
}

// firstBraceExpansion finds the first brace group in tok that the shell
// expands: a {...} with a top-level comma or a {x..y} range. It returns the
// index of the group's opening brace and the group's alternatives, the
// comma-separated pieces. A range has no alternatives to list, so it returns
// the empty one, and the caller tests the text before it. Braces that enclose
// nothing the shell expands, such as {x} or a JSON object without a range, are
// left alone.
func firstBraceExpansion(tok string) (int, []string, bool) {
	for open := 0; open < len(tok); open++ {
		if tok[open] != '{' {
			continue
		}
		depth, last := 0, open+1
		var alts []string
		isList, isRange := false, false
		for j := open; j < len(tok); j++ {
			switch tok[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					if isList {
						return open, append(alts, tok[last:j]), true
					}
					if isRange {
						return open, []string{""}, true
					}
					j = len(tok)
				}
			case ',':
				if depth == 1 {
					isList = true
					alts = append(alts, tok[last:j])
					last = j + 1
				}
			case '.':
				if depth == 1 && j+1 < len(tok) && tok[j+1] == '.' {
					isRange = true
				}
			}
		}
	}
	return 0, nil, false
}

// opensAcceptedRisk reports whether text begins with the ACCEPTED-RISK: marker
// after trimming, in any case.
func opensAcceptedRisk(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "accepted-risk:")
}
