package repoguard

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// execShimMaxLines caps the whole file, comments included: a shim's header is
// a pointer to plugin.md, not a second manual (gt-tpg0p.2).
const execShimMaxLines = 20

// PluginRunSh is what the guard reads for one plugins/<name>/run.sh: the
// script, and the binary its plugin.md [execution] block declares.
type PluginRunSh struct {
	Body   string
	Binary string
}

// PluginRunShLister returns the plugins/<name>/run.sh scripts under a tree,
// keyed by repo-relative path. Callers inject one so the check reads no
// working tree.
type PluginRunShLister func() (map[string]PluginRunSh, error)

// CheckPluginRunSh returns one finding per plugin run.sh that keeps its logic
// with no allowlist line, per allowlisted script that has become a shim or
// left the tree, and per allowlist line with no reason (gt-tpg0p.2).
//
// The comparison runs both ways on purpose: an allowlist that only answered
// "is this script known" would keep a line after the porting change turned its
// script into a shim, and the list would stop shrinking. A lister error is
// returned, never read as a clean tree.
func CheckPluginRunSh(list PluginRunShLister, allowed map[string]string) ([]string, error) {
	scripts, err := list()
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, path := range sortedPaths(scripts) {
		script := scripts[path]
		problem := execShimProblem(script.Body, script.Binary)
		reason, listed := allowed[path]
		switch {
		case listed && reason == "":
			findings = append(findings, fmt.Sprintf("%s: the plugin-run-sh.txt line has no reason; say why this run.sh keeps its logic", path))
		case listed && problem == "":
			findings = append(findings, fmt.Sprintf("%s: this run.sh is an exec shim now; delete its plugin-run-sh.txt line in the same change", path))
		case !listed && problem != "":
			findings = append(findings, fmt.Sprintf("%s: %s; make it one exec of the plugin's declared binary within %d lines, or give it a plugin-run-sh.txt line with its reason (scripts are debt: see .om.json, scripts-are-debt)", path, problem, execShimMaxLines))
		}
	}
	for _, path := range sortedPaths(allowed) {
		if _, ok := scripts[path]; !ok {
			findings = append(findings, fmt.Sprintf("%s: allowlisted plugin run.sh is not in the tree; the plugin-run-sh.txt line is stale", path))
		}
	}
	sort.Strings(findings)
	return findings, nil
}

// ReadPluginRunShAllowlist reads plugin-run-sh.txt: one repo-relative path and
// its one-line reason per line, tab-separated. Blank lines and lines starting
// with # are skipped. A duplicate path or an empty one is an error: the file
// has one line per script, so a second line is a merge artifact that would
// otherwise overwrite the first unseen (gt-tpg0p.2).
//
// A line with no reason keeps an empty one, so the check names the path whose
// line is short rather than failing the whole file.
func ReadPluginRunShAllowlist(name string) (map[string]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	allowed := map[string]string{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		p, reason, _ := strings.Cut(text, "\t")
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("%s:%d: no path before the tab", name, line)
		}
		if _, dup := allowed[p]; dup {
			return nil, fmt.Errorf("%s:%d: %s already has a line", name, line, p)
		}
		allowed[p] = strings.TrimSpace(reason)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return allowed, nil
}

// PluginRunShTree is the lister over a checkout: every plugins/<name>/run.sh
// under root, with the binary its plugin.md declares. A tree with no run.sh, a
// run.sh with no plugin.md beside it and an unreadable file are errors, never
// an empty tree: the guard reads an empty map as clean.
func PluginRunShTree(root string) PluginRunShLister {
	return func() (map[string]PluginRunSh, error) {
		paths, err := filepath.Glob(filepath.Join(root, "plugins", "*", "run.sh"))
		if err != nil {
			return nil, fmt.Errorf("globbing plugins/*/run.sh under %s: %w", root, err)
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("no plugins/*/run.sh under %s", root)
		}
		scripts := make(map[string]PluginRunSh, len(paths))
		for _, p := range paths {
			body, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("%s reads: %w", p, err)
			}
			manifest, err := os.ReadFile(filepath.Join(filepath.Dir(p), "plugin.md"))
			if err != nil {
				return nil, fmt.Errorf("%s declares its binary: %w", p, err)
			}
			// The glob matched <root>/plugins/<name>/run.sh, so the repo-relative
			// path is the plugin directory's base name, in slash form.
			scripts[path.Join("plugins", filepath.Base(filepath.Dir(p)), "run.sh")] = PluginRunSh{
				Body:   string(body),
				Binary: declaredBinary(string(manifest)),
			}
		}
		return scripts, nil
	}
}

// declaredBinary returns the binary a plugin.md [execution] block names, or
// "". The plugin runner honors this key by resolving it inside the plugin
// directory (internal/plugin), so a shim that names it runs the same file the
// runner would (gt-tpg0p.1).
func declaredBinary(pluginMD string) string {
	inFrontmatter, inExecution := false, false
	for _, line := range strings.Split(pluginMD, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "+++" {
			if inFrontmatter {
				return ""
			}
			inFrontmatter = true
			continue
		}
		if !inFrontmatter {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inExecution = trimmed == "[execution]"
			continue
		}
		if !inExecution {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "binary" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// execShimProblem returns "" when body is an exec shim of the plugin's
// declared binary and, when it is not, the one clause a finding adds after the
// path: why this run.sh is logic rather than a shim.
func execShimProblem(body, binary string) string {
	lines := fileLines(body)
	if len(lines) > execShimMaxLines {
		return fmt.Sprintf("it is %d lines, past the %d-line shim cap", len(lines), execShimMaxLines)
	}
	var commands []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		commands = append(commands, line)
	}
	switch len(commands) {
	case 0:
		return "it runs nothing"
	case 1:
		return execCommandProblem(commands[0], binary)
	default:
		return fmt.Sprintf("it holds %d commands and a shim holds one exec", len(commands))
	}
}

// fileLines splits body into the file's lines: the terminating newline adds no
// line, and a blank line before it still counts.
func fileLines(body string) []string {
	if body == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
}

// execCommandProblem returns why command is not the shim's one exec, or "".
func execCommandProblem(command, binary string) string {
	if command != "exec" && !strings.HasPrefix(command, "exec ") && !strings.HasPrefix(command, "exec\t") {
		return "its one command is not an exec"
	}
	target, rest, ok := firstShellWord(strings.TrimPrefix(command, "exec"))
	if !ok {
		return "its exec names no program"
	}
	if problem := execTargetProblem(target, binary); problem != "" {
		return problem
	}
	return execArgsProblem(rest)
}

// execTargetProblem returns why target is not a path to the plugin's declared
// binary, or "".
//
// The target must be one of scriptDirs plus a path inside the plugin
// directory, and that path must be the binary plugin.md declares. A sibling
// name alone proves nothing: an extensionless file beside the script is as
// likely to hold logic as to be a program, and a literal path names a file the
// guard never sees. The declaration is the evidence, because it is the same
// path the plugin runner executes (gt-tpg0p.1, gt-tpg0p.2).
func execTargetProblem(target, binary string) string {
	name, beside := scriptDirRest(target)
	if !beside {
		switch {
		case strings.ContainsAny(target, shellControl):
			return fmt.Sprintf("its exec target %q carries shell syntax", target)
		case strings.Contains(target, "$"):
			return fmt.Sprintf("its exec target %q is computed, not a path inside the plugin directory", target)
		case strings.ContainsAny(target, shellExpansion):
			return fmt.Sprintf("its exec target %q expands shell metacharacters instead of naming one path", target)
		}
		return fmt.Sprintf("its exec target %q is a literal path, and the guard can only vouch for the binary the plugin declares", target)
	}
	switch {
	case name == "":
		return "its exec target names no program"
	case strings.ContainsAny(name, shellControl):
		return fmt.Sprintf("its exec target %q carries shell syntax", target)
	case strings.Contains(name, "$"):
		return fmt.Sprintf("its exec target %q is computed, not a path inside the plugin directory", target)
	case strings.ContainsAny(name, shellExpansion):
		return fmt.Sprintf("its exec target %q expands shell metacharacters instead of naming one path", target)
	case path.Clean(name) != name, path.IsAbs(name), name == "..", strings.HasPrefix(name, "../"):
		// A shim names a file inside its own plugin directory: a name that
		// climbs out of it is not one the guard can hold to the declaration
		// plugin.md makes.
		return fmt.Sprintf("its exec target %q names %q, not a path inside the plugin directory", target, name)
	}
	base := name[strings.LastIndex(name, "/")+1:]
	if base == "" {
		return "its exec target names no program"
	}
	if isInterpreter(base) {
		return fmt.Sprintf("its exec target %q is an interpreter, so the logic stays in another file", target)
	}
	if suffix := scriptSuffix(base); suffix != "" {
		return fmt.Sprintf("its exec target %q is a %s script, so the logic stays in another file", target, suffix)
	}
	if binary == "" {
		return fmt.Sprintf("its exec target %q has no binary evidence: the plugin.md [execution] block names no binary, so the guard cannot tell it from a script", target)
	}
	if path.Clean(name) != path.Clean(binary) {
		return fmt.Sprintf("its exec target %q is not the binary plugin.md declares (%q)", target, binary)
	}
	return ""
}

// execArgsProblem returns why the words after an exec target are more than a
// shim may carry: a shim forwards "$@" and may end its line with a comment.
func execArgsProblem(rest string) string {
	rest = trimTrailingComment(rest)
	if strings.ContainsAny(rest, shellControl) {
		return fmt.Sprintf("its exec line holds shell syntax after the target: %q", rest)
	}
	switch rest {
	case "", `"$@"`, "$@":
		return ""
	}
	return fmt.Sprintf("its exec carries %q, and a shim forwards only \"$@\"", rest)
}

// trimTrailingComment drops a comment that starts a word: a # inside an
// argument is part of the argument.
func trimTrailingComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// scriptDirs are the directory expressions a shim may use to name a file
// inside its own plugin directory, written as they read once firstShellWord
// has removed the quotes.
var scriptDirs = []string{
	"$(dirname $0)",
	"$(dirname ${0})",
	"$(dirname ${BASH_SOURCE[0]})",
	"$(dirname ${BASH_SOURCE[0]:-$0})",
	"$(cd $(dirname $0) && pwd)",
	"$(cd $(dirname ${BASH_SOURCE[0]}) && pwd)",
	"$(cd $(dirname ${BASH_SOURCE[0]:-$0}) && pwd)",
	"${0%/*}",
	"${BASH_SOURCE[0]%/*}",
}

// scriptDirRest returns what target names after the first scriptDirs form and
// its slash, or false when target starts with none of them.
func scriptDirRest(target string) (string, bool) {
	for _, dir := range scriptDirs {
		rest, ok := strings.CutPrefix(target, dir)
		if !ok || !strings.HasPrefix(rest, "/") {
			continue
		}
		return rest[1:], true
	}
	return "", false
}

// shellControl are the characters that let one line hold a second command or a
// redirection, which a shim's single exec may not.
const shellControl = ";&|`<>()"

// shellExpansion are the characters the shell expands into a name the guard
// cannot see: a glob, a tilde or a brace list could name any file, so a target
// names one path in full (gt-tpg0p.2).
const shellExpansion = "~*?[]{}"

// interpreterStems are the programs a shim may not exec, matched on the name
// with its version stripped so python3.11 and node18 are caught. Exec'ing one
// leaves the logic it runs in another file, so the shim replaces nothing.
var interpreterStems = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "csh": true,
	"tcsh": true, "mksh": true, "ash": true, "fish": true, "yash": true, "xonsh": true,
	"python": true, "pypy": true, "perl": true, "ruby": true, "php": true,
	"node": true, "nodejs": true, "deno": true, "bun": true, "ts-node": true, "tsx": true,
	"lua": true, "luajit": true, "tclsh": true, "wish": true, "rscript": true,
	"julia": true, "sbcl": true, "clisp": true, "guile": true, "awk": true,
	"go": true, "env": true, "npx": true, "uv": true, "uvx": true, "pipx": true,
	"sudo": true, "doas": true, "xargs": true, "timeout": true, "nohup": true,
	"nice": true, "setsid": true, "chroot": true, "busybox": true, "toybox": true,
	"make": true,
}

// isInterpreter reports whether name, case-folded and with a trailing version
// stripped, is one of interpreterStems.
func isInterpreter(name string) bool {
	lower := strings.ToLower(name)
	if interpreterStems[lower] {
		return true
	}
	stem := strings.TrimRight(lower, "0123456789.")
	return stem != lower && stem != "" && interpreterStems[stem]
}

// scriptSuffixes are the file kinds a shim may not exec for the same reason as
// interpreterStems.
var scriptSuffixes = []string{
	".sh", ".bash", ".zsh", ".ksh", ".fish",
	".py", ".rb", ".pl", ".lua",
	".js", ".mjs", ".cjs", ".ts", ".ps1", ".bat", ".cmd",
}

// scriptSuffix returns the script extension name ends with, case-folded, or
// "".
func scriptSuffix(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range scriptSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return suffix
		}
	}
	return ""
}

// firstShellWord splits s into its first word, with quoting and escapes
// removed, and the rest after it. $(...) and ${...} nest and hold their own
// quotes, so neither a quote inside a substitution nor a space in one ends the
// word.
func firstShellWord(s string) (word, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	var b strings.Builder
	var closers []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '"' || c == '\'':
			// Quoting decides how the shell reads the word, not where it ends.
		case len(closers) == 0 && (c == ' ' || c == '\t'):
			return b.String(), s[i:], true
		case c == '$' && i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '{'):
			if s[i+1] == '(' {
				closers = append(closers, ')')
			} else {
				closers = append(closers, '}')
			}
			b.WriteByte(c)
			i++
			b.WriteByte(s[i])
		case len(closers) > 0 && c == closers[len(closers)-1]:
			closers = closers[:len(closers)-1]
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), "", true
}

// sortedPaths returns the keys of m sorted, so a finding list does not depend
// on map order.
func sortedPaths[V any](m map[string]V) []string {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
