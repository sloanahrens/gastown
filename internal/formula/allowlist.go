package formula

import (
	"fmt"
	"strings"
)

// This file implements command allowlist matching for formulas that declare
// command_allowlist (gt-9iv). The matcher is deliberately conservative: it is
// a guardrail against well-meaning agents drifting outside a formula's scope
// (the failure mode behind gt-61x), not a hardened sandbox against a
// deliberately adversarial shell user.
//
// Semantics: a shell command is allowed when every top-level segment
// (split on &&, ||, ;, |, &, newline — outside quotes) matches one of the
// allowlist entries by leading-token prefix, after stripping environment
// assignments and shell control keywords. Command substitutions ($(...)) are
// checked recursively; backticks are rejected outright.

// mergeAllowlist unions two allowlist entry slices, preserving order and
// dropping duplicates. Used when inheriting entries through extends.
func mergeAllowlist(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for _, e := range base {
		seen[e] = true
	}
	merged := base
	for _, e := range extra {
		if !seen[e] {
			seen[e] = true
			merged = append(merged, e)
		}
	}
	return merged
}

// shellControlKeywords are tokens stripped from the front of a segment before
// prefix matching. They introduce control flow but execute nothing themselves.
var shellControlKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true,
	"while": true, "until": true, "do": true, "time": true,
	"!": true, "{": true, "export": true, "local": true, "readonly": true,
}

// controlOnlyTokens are segments (or trailing tokens) that close control flow
// and are always harmless on their own.
var controlOnlyTokens = map[string]bool{
	"fi": true, "done": true, "esac": true, "}": true, ";;": true,
	"then": true, "else": true, "do": true,
}

// CheckCommandAllowed reports whether a shell command is permitted by the
// given allowlist entries. Each entry is a whitespace-tokenized command
// prefix ("gt reaper" allows "gt reaper scan --json"). Returns nil when the
// command is allowed, or an error naming the first disallowed segment.
func CheckCommandAllowed(command string, entries []string) error {
	entryTokens := make([][]string, 0, len(entries))
	for _, e := range entries {
		fields := strings.Fields(e)
		if len(fields) > 0 {
			entryTokens = append(entryTokens, fields)
		}
	}
	return checkCommand(command, entryTokens, 0)
}

// maxSubstitutionDepth bounds recursion into nested $(...) substitutions.
const maxSubstitutionDepth = 8

func checkCommand(command string, entries [][]string, depth int) error {
	if depth > maxSubstitutionDepth {
		return fmt.Errorf("command substitution nested too deeply")
	}

	outer, inners, err := extractSubstitutions(command)
	if err != nil {
		return err
	}
	for _, inner := range inners {
		if err := checkCommand(inner, entries, depth+1); err != nil {
			return err
		}
	}

	for _, segment := range splitTopLevel(outer) {
		if err := checkSegment(segment, entries); err != nil {
			return err
		}
	}
	return nil
}

// checkSegment verifies a single pipeline/command segment against the entries.
func checkSegment(segment string, entries [][]string) error {
	tokens := strings.Fields(segment)

	// Strip leading subshell parens: "(gt reaper scan" -> "gt reaper scan".
	for len(tokens) > 0 {
		tokens[0] = strings.TrimLeft(tokens[0], "(")
		if tokens[0] != "" {
			break
		}
		tokens = tokens[1:]
	}
	// Strip trailing close-parens from the last token so "gt convoy check)"
	// still prefix-matches. Only the leading tokens matter for matching, but
	// a bare ")" token should not survive as a command.
	for len(tokens) > 0 {
		last := strings.TrimRight(tokens[len(tokens)-1], ")")
		if last == "" {
			tokens = tokens[:len(tokens)-1]
			continue
		}
		tokens[len(tokens)-1] = last
		break
	}

	// Strip environment assignments (FOO=bar cmd ...) and control keywords.
	for len(tokens) > 0 {
		t := tokens[0]
		if isEnvAssignment(t) || shellControlKeywords[t] {
			tokens = tokens[1:]
			continue
		}
		break
	}

	if len(tokens) == 0 {
		return nil // pure control flow, redirection, or assignment — harmless
	}
	if controlOnlyTokens[tokens[0]] {
		return nil
	}
	// Loop and case headers execute nothing themselves; their bodies arrive
	// as separate segments (split on ; and newlines) and are checked there.
	if tokens[0] == "for" || tokens[0] == "case" || tokens[0] == "select" {
		return nil
	}

	for _, entry := range entries {
		if len(tokens) < len(entry) {
			continue
		}
		match := true
		for i, want := range entry {
			if tokens[i] != want {
				match = false
				break
			}
		}
		if match {
			return nil
		}
	}

	return fmt.Errorf("command %q is not in this formula's command allowlist", strings.Join(tokens, " "))
}

// isEnvAssignment reports whether a token looks like a leading VAR=value
// environment assignment.
func isEnvAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq < 1 {
		return false
	}
	for i, r := range token[:eq] {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// extractSubstitutions scans a command for $(...) substitutions outside
// single quotes, returning the command with substitutions blanked out plus
// each inner command for recursive checking. Arithmetic expansion $((...))
// is treated as opaque and allowed. Backticks are rejected: they are archaic,
// unneeded for formula work, and complicate scanning.
func extractSubstitutions(command string) (outer string, inners []string, err error) {
	var out strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			out.WriteByte(c)
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			out.WriteByte(c)
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			out.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			out.WriteByte(c)
		case c == '`' && !inSingle:
			return "", nil, fmt.Errorf("backtick command substitution is not allowed under a formula command allowlist; use $(...) instead")
		case c == '$' && !inSingle && i+1 < len(command) && command[i+1] == '(':
			if i+2 < len(command) && command[i+2] == '(' {
				// Arithmetic expansion $((...)) — skip past the outer paren.
				end, ok := findClosing(command, i+1)
				if !ok {
					return "", nil, fmt.Errorf("unbalanced arithmetic expansion in command")
				}
				out.WriteByte(' ')
				i = end
				continue
			}
			end, ok := findClosing(command, i+1)
			if !ok {
				return "", nil, fmt.Errorf("unbalanced command substitution in command")
			}
			inners = append(inners, command[i+2:end])
			out.WriteByte(' ')
			i = end
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), inners, nil
}

// findClosing returns the index of the ')' matching the '(' at open,
// tracking nesting and quotes.
func findClosing(command string, open int) (int, bool) {
	depth := 0
	inSingle := false
	inDouble := false
	escaped := false
	for i := open; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '(' && !inSingle && !inDouble:
			depth++
		case c == ')' && !inSingle && !inDouble:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// splitTopLevel splits a command into segments at top-level shell operators
// (&&, ||, ;, |, &, newlines) outside quotes. Redirection operators like
// 2>&1 and |& are not treated as separators for the byte that belongs to
// the redirection.
func splitTopLevel(command string) []string {
	var segments []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			segments = append(segments, s)
		}
		current.Reset()
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			current.WriteByte(c)
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			current.WriteByte(c)
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			current.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			current.WriteByte(c)
		case inSingle || inDouble:
			current.WriteByte(c)
		case c == '\n' || c == ';':
			flush()
		case c == '|':
			// |& pipes stderr too; both split the same way.
			if i+1 < len(command) && (command[i+1] == '|' || command[i+1] == '&') {
				i++
			}
			flush()
		case c == '&':
			if i+1 < len(command) && command[i+1] == '&' {
				i++
				flush()
				continue
			}
			// >& and &> are redirections, not background/separator.
			if i > 0 && command[i-1] == '>' {
				current.WriteByte(c)
				continue
			}
			if i+1 < len(command) && command[i+1] == '>' {
				current.WriteByte(c)
				continue
			}
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return segments
}
