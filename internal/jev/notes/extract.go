package notes

import (
	"regexp"
	"sort"
	"strings"
)

// Command is one gt, bd or hm invocation a text names in backticks or in a
// fenced block: the binary, the words after it that could be subcommands, and
// the long flags it passes.
type Command struct {
	Bin string
	// Words are the leading arguments shaped like subcommand names, up to the
	// first one that is not (a quoted string, a placeholder, a flag).
	Words []string
	Flags []string
}

// Tokens is what a text names that the mechanical label can check.
type Tokens struct {
	// Paths are repo-relative paths, without a line suffix or a trailing slash.
	Paths    []string
	Commands []Command
	// Keys are config keys, dotted or not.
	Keys []string
}

// Empty reports whether the text names nothing to check.
func (t Tokens) Empty() bool {
	return len(t.Paths) == 0 && len(t.Commands) == 0 && len(t.Keys) == 0
}

var (
	// spanRe matches an inline code span on one line.
	spanRe = regexp.MustCompile("`([^`\n]+)`")

	// pathRe matches a relative path of at least two segments. A leading "~",
	// "/" or "$" does not match: those name the operator's machine, not the
	// repository.
	pathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*(/[A-Za-z0-9._-]+)+/?$`)

	// lineSuffixRe is a ":12" or ":12-30" after a path.
	lineSuffixRe = regexp.MustCompile(`:\d+(-\d+)?$`)

	// wordRe is a subcommand name.
	wordRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

	// flagRe is a long flag, with or without a value.
	flagRe = regexp.MustCompile(`^--([a-z][a-z0-9-]*)(=.*)?$`)

	// segmentRe is one part of a config key.
	segmentRe = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
)

// binaries are the commands whose subcommands and flags are checked against
// their --help.
var binaries = map[string]bool{"gt": true, "bd": true, "hm": true}

// fileExts end a dotted token that is a file name, not a config key.
var fileExts = map[string]bool{
	"go": true, "md": true, "json": true, "jsonl": true, "toml": true, "yaml": true,
	"yml": true, "sh": true, "mod": true, "sum": true, "sql": true, "txt": true, "log": true,
}

// shellBreaks end the words that belong to a command: what follows is another
// command or a redirect.
var shellBreaks = map[string]bool{"|": true, "||": true, "&&": true, ";": true, ">": true, ">>": true, "<": true, "2>&1": true}

// Extract reads the tokens a text names. Inline code spans are classified as a
// command (they start with gt, bd or hm), a path or a config key; every line of
// a fenced block that starts with a command counts as a command. Anything else
// in backticks is ignored.
func Extract(text string) Tokens {
	var spans, cmdLines []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			cmdLines = append(cmdLines, trimmed)
			continue
		}
		for _, m := range spanRe.FindAllStringSubmatch(line, -1) {
			spans = append(spans, strings.TrimSpace(m[1]))
		}
	}

	var out Tokens
	seenPath, seenKey, seenCmd := map[string]bool{}, map[string]bool{}, map[string]bool{}
	addCommand := func(s string) bool {
		c, ok := parseCommand(s)
		if !ok {
			return false
		}
		key := c.Bin + " " + strings.Join(c.Words, " ") + " " + strings.Join(c.Flags, " ")
		if !seenCmd[key] {
			seenCmd[key] = true
			out.Commands = append(out.Commands, c)
		}
		return true
	}
	for _, s := range spans {
		if addCommand(strings.TrimPrefix(s, "$ ")) {
			continue
		}
		if p, ok := parsePath(s); ok {
			if !seenPath[p] {
				seenPath[p] = true
				out.Paths = append(out.Paths, p)
			}
			continue
		}
		if isConfigKey(s) && !seenKey[s] {
			seenKey[s] = true
			out.Keys = append(out.Keys, s)
		}
	}
	for _, l := range cmdLines {
		addCommand(strings.TrimPrefix(l, "$ "))
	}
	sort.Strings(out.Paths)
	sort.Strings(out.Keys)
	return out
}

// parseCommand reads a span as a command: a binary from binaries and at least
// one argument.
func parseCommand(s string) (Command, bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 || !binaries[fields[0]] {
		return Command{}, false
	}
	for i, f := range fields {
		if shellBreaks[f] || strings.HasSuffix(f, ";") && len(f) > 1 {
			fields = fields[:i]
			break
		}
	}
	c := Command{Bin: fields[0]}
	words := true
	for _, f := range fields[1:] {
		if m := flagRe.FindStringSubmatch(f); m != nil {
			words = false
			if m[1] != "help" {
				c.Flags = append(c.Flags, "--"+m[1])
			}
			continue
		}
		if words && wordRe.MatchString(f) {
			c.Words = append(c.Words, f)
			continue
		}
		words = false
	}
	if len(c.Words) == 0 && len(c.Flags) == 0 {
		return Command{}, false
	}
	return c, true
}

// parsePath reads a span as a repo-relative path.
func parsePath(s string) (string, bool) {
	p := lineSuffixRe.ReplaceAllString(s, "")
	p = strings.TrimPrefix(p, "./")
	if !pathRe.MatchString(p) {
		return "", false
	}
	return strings.TrimSuffix(p, "/"), true
}

// isConfigKey reports whether a span is a snake_case config key or a dotted
// path of them, such as patrols.jev_stale.enabled. A dotted token ending in a
// file extension is a file name, not a key.
func isConfigKey(s string) bool {
	segs := strings.Split(s, ".")
	underscore := false
	for _, seg := range segs {
		if !segmentRe.MatchString(seg) {
			return false
		}
		underscore = underscore || strings.Contains(seg, "_")
	}
	if !underscore {
		return false
	}
	return len(segs) == 1 || !fileExts[segs[len(segs)-1]]
}
