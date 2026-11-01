package shelltier

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// repoRoot is the worktree this test file sits in, so the guards read the
// scripts the manifest lists rather than a copy of them.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not report this test file's path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// repoFiles returns the reader the guards use on the tree this test runs in.
// It is the one place a guard touches the filesystem: each rule takes a
// reader, so its table drives it with fixtures and only the manifest
// assertions read the real scripts.
func repoFiles(t *testing.T) func(string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	return func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// fixtureFiles returns a reader over m. A path m does not hold is unreadable,
// which is what a missing file reports.
func fixtureFiles(m map[string]string) func(string) (string, error) {
	return func(rel string) (string, error) {
		text, ok := m[rel]
		if !ok {
			return "", os.ErrNotExist
		}
		return text, nil
	}
}

// repoPathPrefixes are the repo trees and file a shell test names. A script
// that reads one of them must run when a path under it changes.
var repoPathPrefixes = []string{"scripts/", "deploy/", "plugins/", ".githooks/", "internal/", "Makefile"}

// scanRepoPaths returns the repo-relative paths text names, sorted and unique.
// A script writes them bare, in quotes, or after a variable such as $ROOT/ or
// $SCRIPT_DIR/, and every form is a dependency its triggers must cover.
func scanRepoPaths(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, prefix := range repoPathPrefixes {
		for i := 0; ; {
			j := strings.Index(text[i:], prefix)
			if j < 0 {
				break
			}
			start := i + j
			i = start + 1
			if start > 0 && isPathWordByte(text[start-1]) {
				continue // a prefix inside a longer name is not a repo path
			}
			end := start + len(prefix)
			for end < len(text) && isPathByte(text[end]) {
				end++
			}
			if p := text[start:end]; !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out
}

// isPathWordByte reports whether b continues a word. The separator / is not
// one, so $ROOT/scripts/x.sh and https://host/scripts/x.sh both name a path.
func isPathWordByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' ||
		('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// isPathByte reports whether b continues a repo-relative path.
func isPathByte(b byte) bool {
	return isPathWordByte(b) || b == '/'
}

// uncoveredRepoPaths returns the paths text names that the entry's triggers do
// not run it for. A path that is named but not covered would let a change to
// it land without running this script (test-engineering rule 6).
func uncoveredRepoPaths(e Entry, text string) []string {
	triggers := e.TriggersOrDefault()
	var out []string
	for _, p := range scanRepoPaths(text) {
		if !tiertrigger.Runs(triggers, []string{p}, false) {
			out = append(out, p)
		}
	}
	return out
}

// checkManifestPaths returns the problems the entries have with the repo paths
// their scripts name: one per path the entry's triggers do not cover, and one
// per script read cannot return. read is the seam and the rule's only file
// access.
func checkManifestPaths(entries []Entry, read func(string) (string, error)) []string {
	var out []string
	for _, e := range entries {
		text, err := read(e.Path)
		if err != nil {
			out = append(out, fmt.Sprintf("%s cannot be read: %v", e.Path, err))
			continue
		}
		for _, p := range uncoveredRepoPaths(e, text) {
			out = append(out, fmt.Sprintf("%s names %s, but its triggers %q do not run it when %s changes",
				e.Path, p, e.TriggersOrDefault(), p))
		}
	}
	return out
}

func TestEveryReferencedRepoPathIsATrigger(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []Entry
		files   map[string]string
		want    string
	}{
		{
			"a path under a default trigger is covered",
			[]Entry{{Path: "scripts/a_test.sh"}},
			map[string]string{"scripts/a_test.sh": "cp \"$SCRIPT_DIR/../scripts/lib/log.sh\" \"$D/\"\n"},
			"",
		},
		{
			"a path no default trigger covers is named with the script",
			[]Entry{{Path: "deploy/k8s/kind/kind_test.sh"}},
			map[string]string{"deploy/k8s/kind/kind_test.sh": "cp deploy/k8s/kind/values/kind.yaml \"$TMP/\"\n"},
			"deploy/k8s/kind/kind_test.sh names deploy/k8s/kind/values/kind.yaml",
		},
		{
			"a deploy path behind a variable is named",
			[]Entry{{Path: "scripts/floor_test.sh"}},
			map[string]string{"scripts/floor_test.sh": "\"$ROOT/deploy/polecat/go-floor.sh\" check\n"},
			"scripts/floor_test.sh names deploy/polecat/go-floor.sh",
		},
		{
			"a declared trigger covers a path the defaults do not",
			[]Entry{{Path: "scripts/floor_test.sh", Triggers: []string{"scripts/", "Makefile", "deploy/"}}},
			map[string]string{"scripts/floor_test.sh": "\"$ROOT/deploy/polecat/go-floor.sh\" check\n"},
			"",
		},
		{
			"a hook path behind a variable is named",
			[]Entry{{Path: ".githooks/pre-push_test.sh"}},
			map[string]string{".githooks/pre-push_test.sh": ". \"$REPO/.githooks/pre-push\"\n"},
			".githooks/pre-push_test.sh names .githooks/pre-push",
		},
		{
			"a script that cannot be read is a problem, not a pass",
			[]Entry{{Path: "scripts/absent_test.sh"}},
			map[string]string{},
			"scripts/absent_test.sh cannot be read",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkManifestPaths(tc.entries, fixtureFiles(tc.files))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("checkManifestPaths() = %q, want no problem", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("checkManifestPaths() = %q, want one problem naming %q", got, tc.want)
			}
		})
	}
	t.Run("the manifest", func(t *testing.T) {
		t.Parallel()
		for _, p := range checkManifestPaths(Manifest(), repoFiles(t)) {
			t.Error(p)
		}
	})
}

func TestScanFindsPathsInQuotesAndVariables(t *testing.T) {
	t.Parallel()
	text := `set -euo pipefail
SRC="$SCRIPT_DIR/../scripts/install-local.sh"
lib="$ROOT/scripts/lib/log.sh"
. "$REPO/.githooks/pre-push"
python3 "$ROOT/plugins/polecat-image/build.py"
cp deploy/k8s/kind/values/kind.yaml "$TMP/"
go test ./internal/shelltier
make -C "$ROOT" -f Makefile
echo 'not a path: myscripts/x.sh'
`
	want := []string{
		".githooks/pre-push",
		"Makefile",
		"deploy/k8s/kind/values/kind.yaml",
		"internal/shelltier",
		"plugins/polecat-image/build.py",
		"scripts/install-local.sh",
		"scripts/lib/log.sh",
	}
	if got := scanRepoPaths(text); !slices.Equal(got, want) {
		t.Fatalf("scanRepoPaths() = %q, want %q", got, want)
	}
}

// heredoc is a redirection whose body starts on the next line.
type heredoc struct {
	delim  string
	quoted bool // the delimiter is quoted, so the body is data the shell does not expand
	strip  bool // <<- strips leading tabs from the terminator
}

// ends reports whether line is this heredoc's terminator.
func (h heredoc) ends(line string) bool {
	body := strings.TrimSuffix(line, "\n")
	if h.strip {
		body = strings.TrimLeft(body, "\t")
	}
	return body == h.delim
}

// maskData returns s with everything but the line breaks blanked.
func maskData(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			b.WriteByte('\n')
			continue
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// codeOnly returns text with the parts the shell reads as data blanked: a
// comment, a single-quoted string, a double-quoted string (but not the
// expansions and substitutions inside one) and a quoted heredoc body. An
// unquoted heredoc body is scanned, since the shell expands it.
func codeOnly(text string) string {
	var out strings.Builder
	var pending []heredoc
	for _, line := range strings.SplitAfter(text, "\n") {
		if len(pending) > 0 {
			if pending[0].ends(line) {
				pending = pending[1:]
				out.WriteString(maskData(line))
				continue
			}
			if pending[0].quoted {
				out.WriteString(maskData(line))
				continue
			}
			// A heredoc opened inside a body is text the shell reads, not one
			// it runs, so the specs are dropped.
			masked, _ := maskLine(line)
			out.WriteString(masked)
			continue
		}
		masked, specs := maskLine(line)
		pending = specs
		out.WriteString(masked)
	}
	if len(pending) > 0 {
		// A heredoc with no terminator leaves the rest of the script ambiguous,
		// so the scan keeps every mention rather than mask the ones it cannot
		// place: a spurious run costs less than a missed one.
		return text
	}
	return out.String()
}

// maskLine returns line with its data contexts blanked, and the heredocs it
// opens.
func maskLine(line string) (string, []heredoc) {
	var b strings.Builder
	b.Grow(len(line))
	var specs []heredoc
	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == '\\':
			b.WriteByte(c)
			if i+1 < len(line) {
				b.WriteByte(line[i+1])
			}
			i += 2
		case c == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				b.WriteString(maskData(line[i:]))
				return b.String(), specs
			}
			b.WriteString(maskData(line[i : i+j+2]))
			i += j + 2
		case c == '"':
			masked, n := maskDoubleQuoted(line[i:])
			b.WriteString(masked)
			i += n
		case c == '`':
			j := strings.IndexByte(line[i+1:], '`')
			if j < 0 {
				b.WriteString(line[i:]) // an unterminated substitution is code
				return b.String(), specs
			}
			b.WriteString(line[i : i+j+2])
			i += j + 2
		case c == '$' && i+1 < len(line) && line[i+1] == '{':
			j := strings.IndexByte(line[i+2:], '}')
			if j < 0 {
				b.WriteString(line[i:])
				return b.String(), specs
			}
			b.WriteString(line[i : i+j+3])
			i += j + 3
		case c == '#' && atWordStart(line, i):
			b.WriteString(maskData(line[i:]))
			return b.String(), specs
		case c == '<' && strings.HasPrefix(line[i:], "<<") && !strings.HasPrefix(line[i:], "<<<"):
			spec, n := parseHeredoc(line[i:])
			if n == 0 {
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteString(line[i : i+n])
			specs = append(specs, spec)
			i += n
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), specs
}

// maskDoubleQuoted returns the masked form of s, which starts with a double
// quote, and how many bytes it covers. The shell expands what it finds inside
// the quotes and runs the substitutions, so those stay code; the literal text
// is data.
func maskDoubleQuoted(s string) (string, int) {
	var b strings.Builder
	b.WriteByte(' ')
	for i := 1; i < len(s); {
		switch c := s[i]; {
		case c == '"':
			b.WriteByte(' ')
			return b.String(), i + 1
		case c == '\\':
			end := min(i+2, len(s))
			b.WriteString(maskData(s[i:end]))
			i = end
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				b.WriteString(s[i:])
				return b.String(), len(s)
			}
			b.WriteString(s[i : i+j+2])
			i += j + 2
		case c == '$':
			// The shell expands every $ in double quotes, so the expansion is
			// code even though the literal text around it is data.
			if i+1 < len(s) && s[i+1] == '(' {
				seg, n := substitution(s[i:])
				b.WriteString(seg)
				i += n
				continue
			}
			if i+1 < len(s) && s[i+1] == '{' {
				j := strings.IndexByte(s[i+2:], '}')
				if j < 0 {
					b.WriteString(s[i:])
					return b.String(), len(s)
				}
				b.WriteString(s[i : i+j+3])
				i += j + 3
				continue
			}
			j := i + 1
			for j < len(s) && isDelimByte(s[j]) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
		default:
			b.WriteByte(' ')
			i++
		}
	}
	return b.String(), len(s)
}

// substitution returns s[:n], the $(...) it starts with, scanning to the
// matching close. An unterminated substitution is returned whole: a go command
// inside it is still code, so refusing to split it costs at most a spurious run.
func substitution(s string) (string, int) {
	depth := 0
	var quote byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[:i+1], i + 1
			}
		}
	}
	return s, len(s)
}

// atWordStart reports whether i begins a word in line, which is what makes a
// # a comment rather than part of a word.
func atWordStart(line string, i int) bool {
	if i == 0 {
		return true
	}
	switch line[i-1] {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '`':
		return true
	}
	return false
}

// parseHeredoc returns the heredoc that s, which starts with <<, opens and how
// many bytes it covers, or a zero length when s is not a heredoc (1<<2, <<<).
func parseHeredoc(s string) (heredoc, int) {
	i := 2
	strip := false
	if i < len(s) && s[i] == '-' {
		strip = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) {
		return heredoc{}, 0
	}
	h := heredoc{strip: strip}
	switch s[i] {
	case '\'', '"':
		q := s[i]
		j := strings.IndexByte(s[i+1:], q)
		if j < 0 {
			return heredoc{}, 0
		}
		h.delim = s[i+1 : i+1+j]
		h.quoted = true
		i += j + 2
	case '\\':
		start := i + 1
		j := start
		for j < len(s) && isDelimByte(s[j]) {
			j++
		}
		h.delim = s[start:j]
		h.quoted = true
		i = j
	default:
		start := i
		for i < len(s) && isDelimByte(s[i]) {
			i++
		}
		h.delim = s[start:i]
	}
	if !validDelim(h.delim) {
		return heredoc{}, 0
	}
	return h, i
}

func isDelimByte(b byte) bool {
	return b == '_' || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// validDelim reports whether s can be a heredoc delimiter rather than a shell
// operator: it starts with a letter or _ so that 1<<2 is not a heredoc.
func validDelim(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	if c != '_' && !('a' <= c && c <= 'z') && !('A' <= c && c <= 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isDelimByte(s[i]) {
			return false
		}
	}
	return true
}

// goCommandRE matches a go tool in code: the word go (bare, or at the end of a
// path such as /usr/local/go/bin/go) followed by a subcommand. It does not
// care what precedes the word, so command, exec, xargs and ! cannot hide a
// command behind their names; goCommand follows a variable assigned go or a
// whole go command separately. cargo build is not a go command.
var goCommandRE = regexp.MustCompile(`(?m)(?:[^\s;&|(){}<>'"]*/)?\bgo[ \t]+(build|run|test|vet|install)\b`)

// makeInstallRE matches make installing the tree in code, whatever flags it
// carries between.
var makeInstallRE = regexp.MustCompile(`(?m)\bmake\b[^\n;&|]*\binstall\b`)

// shellEvalRE matches a go command inside a string a shell is handed to run:
// eval, or sh/bash/zsh/dash/ksh -c. The string is quoted, so the masking above
// treats it as data, but the shell runs it.
var shellEvalRE = regexp.MustCompile(`\b(?:eval|sh|bash|zsh|dash|ksh)\b[^;&|\n]*[ \t]["']?go[ \t]+(build|run|test|vet|install)\b`)

// goAssignRE captures NAME=value at a word start.
var goAssignRE = regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_])([A-Za-z_][A-Za-z0-9_]*)=(?:"([^"\n]*)"|'([^'\n]*)'|([^\s;&|]+))`)

// goVar is a variable a script assigns a go command to: the word go alone, or
// a whole command such as "go build ./...".
type goVar struct {
	name  string
	whole string // the go command it holds; empty when it holds only the word go
}

// goCommand returns the first go or make install command text runs, or "".
// The scan is a text guard and fails closed: a mention it cannot prove is
// data counts as a command, so a wrong classification costs a spurious run,
// never a missed one.
func goCommand(text string) string {
	code := codeOnly(text)
	if cmd := goCommandInCode(code); cmd != "" {
		return cmd
	}
	if m := shellEvalRE.FindStringSubmatch(text); m != nil {
		return "go " + m[1]
	}
	for _, v := range goVars(text) {
		if v.whole != "" {
			if regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(v.name) + `\}?[^\w]`).MatchString(code) {
				return v.whole
			}
			continue
		}
		use := `\$\{?` + regexp.QuoteMeta(v.name) + `\}?[ \t]+(build|run|test|vet|install)[^\w]`
		if m := regexp.MustCompile(use).FindStringSubmatch(code); m != nil {
			return "go " + m[1]
		}
	}
	return ""
}

// goCommandInCode returns the first go command in code, or "".
func goCommandInCode(code string) string {
	if m := goCommandRE.FindStringSubmatch(code); m != nil {
		return "go " + m[1]
	}
	if makeInstallRE.MatchString(code) {
		return "make install"
	}
	return ""
}

// goVars returns the variables text assigns a go command to.
func goVars(text string) []goVar {
	var out []goVar
	for _, m := range goAssignRE.FindAllStringSubmatch(text, -1) {
		value := m[2]
		if value == "" {
			value = m[3]
		}
		if value == "" {
			value = m[4]
		}
		if value == "go" {
			out = append(out, goVar{name: m[1]})
			continue
		}
		if value != "" {
			if cmd := goCommandInCode(codeOnly(value)); cmd != "" {
				out = append(out, goVar{name: m[1], whole: cmd})
			}
		}
	}
	return out
}

// stubCitationRE matches the file a stub: reason cites, with an optional line.
var stubCitationRE = regexp.MustCompile(`^([A-Za-z0-9._/-]+)(?::([0-9]+))?$`)

// stubCitation returns the file a stub: reason cites and its line, if any.
func stubCitation(reason string) (file string, line int, ok bool) {
	rest, found := strings.CutPrefix(reason, "stub:")
	if !found {
		return "", 0, false
	}
	for _, word := range strings.Fields(rest) {
		m := stubCitationRE.FindStringSubmatch(strings.Trim(word, "(),;."))
		if m == nil || !strings.Contains(m[1], "/") {
			continue
		}
		if m[2] == "" {
			return m[1], 0, true
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		return m[1], n, true
	}
	return "", 0, false
}

// fakeGoRE matches a fake go a script defines: a go (or go-1.27) executable
// written under a temporary bin directory, or a go() shell function.
var fakeGoRE = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*/go(?:[.-][0-9][0-9.]*)?\b|\bgo[[:space:]]*\([[:space:]]*\)`)

// goExemptionProblem returns why the entry's StubsGo exemption is not
// supported by its script text and the files the reason cites, or "".
// A none: exemption claims the script runs no go, a go: declaration claims it
// runs the real go, and a stub: exemption names where the script's fake go is
// defined; a claim the text does not support makes the script run on Go
// changes for the wrong reason or not at all, so the guard refuses it.
func goExemptionProblem(e Entry, text string, files func(string) (string, error)) string {
	switch {
	case e.StubsGo == "":
		return "" // no exemption: the entry already runs on every Go change
	case strings.HasPrefix(e.StubsGo, "none:"):
		if cmd := goCommand(text); cmd != "" {
			return fmt.Sprintf("%s carries %q but runs %q", e.Path, e.StubsGo, cmd)
		}
		return ""
	case strings.HasPrefix(e.StubsGo, "go:") && hasGoDeclaration(e.StubsGo):
		if goCommand(text) == "" {
			return fmt.Sprintf("%s carries %q but its script runs no go command, so it should not run on every Go change", e.Path, e.StubsGo)
		}
		return ""
	case strings.HasPrefix(e.StubsGo, "stub:"):
	default:
		return fmt.Sprintf("%s carries %q, which is no exemption the guard can check", e.Path, e.StubsGo)
	}
	file, line, ok := stubCitation(e.StubsGo)
	if !ok {
		return fmt.Sprintf("%s carries %q, and the reason cites no file:line", e.Path, e.StubsGo)
	}
	if clean := path.Clean(file); path.IsAbs(file) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Sprintf("%s carries %q, and %s is not a path under the repo root", e.Path, e.StubsGo, file)
	}
	evidence, err := files(file)
	if err != nil {
		return fmt.Sprintf("%s carries %q, but %s cannot be read: %v", e.Path, e.StubsGo, file, err)
	}
	if line > 0 {
		lines := strings.Split(evidence, "\n")
		if line > len(lines) {
			return fmt.Sprintf("%s carries %q, but %s has only %d lines", e.Path, e.StubsGo, file, len(lines))
		}
		evidence = lines[line-1]
	}
	if !fakeGoRE.MatchString(evidence) {
		return fmt.Sprintf("%s carries %q, but %s does not define a fake go", e.Path, e.StubsGo, file)
	}
	return ""
}

// checkManifestGo returns the problems the entries' StubsGo exemptions have
// against the text read returns and the files their reasons cite. read is the
// seam and the rule's only file access.
func checkManifestGo(entries []Entry, read func(string) (string, error)) []string {
	var out []string
	for _, e := range entries {
		if e.StubsGo == "" {
			continue
		}
		text, err := read(e.Path)
		if err != nil {
			out = append(out, fmt.Sprintf("%s cannot be read: %v", e.Path, err))
			continue
		}
		if p := goExemptionProblem(e, text, read); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestEveryStubsGoExemptionIsSupportedByTheScript(t *testing.T) {
	t.Parallel()
	const stubScript = "set -euo pipefail\nprintf 'echo go\\n' > \"$BIN/go\"\n"
	tests := []struct {
		name  string
		entry Entry
		files map[string]string
		want  string
	}{
		{
			"a none: script that mentions go only in a grep pattern passes",
			Entry{Path: "scripts/plain_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/plain_test.sh": "set -euo pipefail\ngrep -q 'go build' README.md\n"},
			"",
		},
		{
			"a none: script that runs go build is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "set -euo pipefail\ngo build ./...\n"},
			"scripts/build_test.sh",
		},
		{
			"a none: script that runs go behind command is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "set -euo pipefail\ncommand go build ./...\n"},
			"runs \"go build\"",
		},
		{
			"a none: script that runs go behind exec is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "set -euo pipefail\nexec go test ./...\n"},
			"runs \"go test\"",
		},
		{
			"a none: script that runs go behind xargs is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "find . -name '*.go' -print0 | xargs -0 go vet\n"},
			"runs \"go vet\"",
		},
		{
			"a none: script that runs go behind ! is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "if ! go test ./...; then exit 1; fi\n"},
			"runs \"go test\"",
		},
		{
			"a none: script that runs go through a variable is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "GO=go\n\"$GO\" build ./...\n"},
			"runs \"go build\"",
		},
		{
			"a none: script that runs a command held in a variable is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "GOCMD=\"go build ./...\"\n$GOCMD\n"},
			"runs \"go build\"",
		},
		{
			"a none: script that runs go in sh -c is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "sh -c 'go test ./...'\n"},
			"runs \"go test\"",
		},
		{
			"a none: script that runs go in eval is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "eval \"go run ./cmd/gt\"\n"},
			"runs \"go run\"",
		},
		{
			"a none: script that runs go through a path is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "/usr/local/go/bin/go test ./...\n"},
			"runs \"go test\"",
		},
		{
			"a none: script that runs go in a $(...) inside quotes is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/build_test.sh": "echo \"$(go test ./...)\"\n"},
			"runs \"go test\"",
		},
		{
			"a none: script that runs make install after a separator is named",
			Entry{Path: "scripts/install_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/install_test.sh": "set -euo pipefail\nmake lint && make install\n"},
			"make install",
		},
		{
			"a none: script that only echoes go is named, since the guard cannot prove it is data",
			Entry{Path: "scripts/echo_test.sh", StubsGo: "none: names no go command"},
			map[string]string{"scripts/echo_test.sh": "echo go build ./...\n"},
			"runs \"go build\"",
		},
		{
			"the go: declaration names the go command the script runs",
			Entry{Path: "scripts/gate_test.sh", StubsGo: "go: runs the real shelltier"},
			map[string]string{"scripts/gate_test.sh": "(cd \"$ROOT\" && go run ./internal/shelltier/cmd/shelltier --all)\n"},
			"",
		},
		{
			"a go: declaration its script does not support is named",
			Entry{Path: "scripts/gate_test.sh", StubsGo: "go: runs the real shelltier"},
			map[string]string{"scripts/gate_test.sh": "grep -q gt internal/shelltier/manifest.go\n"},
			"runs no go command",
		},
		{
			"a stub: reason citing the line that defines the fake go passes",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/stub_test.sh:2 puts a fake go on PATH"},
			map[string]string{"scripts/stub_test.sh": stubScript}, "",
		},
		{
			"a stub: reason citing a line with no fake go fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/stub_test.sh:1 puts a fake go on PATH"},
			map[string]string{"scripts/stub_test.sh": stubScript}, "scripts/stub_test.sh",
		},
		{
			"a stub: reason citing a line past the end of the file fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/stub_test.sh:9 puts a fake go on PATH"},
			map[string]string{"scripts/stub_test.sh": stubScript}, "scripts/stub_test.sh has only 3 lines",
		},
		{
			"a stub: reason citing a file that does not exist fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/absent_test.sh puts a fake go on PATH"},
			map[string]string{"scripts/stub_test.sh": ""}, "scripts/absent_test.sh",
		},
		{
			"a stub: reason that cites no file fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: puts a fake go on PATH"},
			map[string]string{"scripts/stub_test.sh": ""}, "cites no file",
		},
		{
			"a stub: reason citing a path outside the repo fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: /etc/passwd puts a fake go on PATH"},
			map[string]string{"scripts/stub_test.sh": ""}, "not a path under the repo root",
		},
		{
			"an exemption the guard cannot read fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "bogus"},
			map[string]string{"scripts/stub_test.sh": ""}, "no exemption the guard can check",
		},
		{
			"a script that cannot be read is a problem, not a pass",
			Entry{Path: "scripts/absent_test.sh", StubsGo: "none: names no go command"},
			map[string]string{}, "scripts/absent_test.sh cannot be read",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkManifestGo([]Entry{tc.entry}, fixtureFiles(tc.files))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("checkManifestGo() = %q, want no problem", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("checkManifestGo() = %q, want one problem naming %q", got, tc.want)
			}
		})
	}
	t.Run("the manifest", func(t *testing.T) {
		t.Parallel()
		for _, p := range checkManifestGo(Manifest(), repoFiles(t)) {
			t.Error(p)
		}
	})
}

func TestScanFindsGoCommandsTheShellRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{"a go command at the start of a line", "go build ./...\n", "go build"},
		{"a go command after a separator", "make lint && go test ./...\n", "go test"},
		{"a go command after a separator with no space", "go build;go test\n", "go build"},
		{"a go command behind command", "command go build ./...\n", "go build"},
		{"a go command behind exec", "exec go install ./cmd/gt\n", "go install"},
		{"a go command behind xargs", "find . -name go | xargs go vet\n", "go vet"},
		{"a go command behind !", "! go test ./...\n", "go test"},
		{"a go command behind a path", "/usr/local/go/bin/go test ./...\n", "go test"},
		{"a go command in a substitution", "echo \"$(go test ./...)\"\n", "go test"},
		{"a go command in a backtick substitution", "echo `go run ./cmd/gt`\n", "go run"},
		{"a go command held in a variable", "GO=go\n\"$GO\" build ./...\n", "go build"},
		{"a command held in a variable", "GOCMD='go test ./...'\n$GOCMD\n", "go test"},
		{"a go command in sh -c", "sh -c 'go test ./...'\n", "go test"},
		{"a go command in eval", "eval \"go build ./...\"\n", "go build"},
		{"make install after a separator", "make lint && make install\n", "make install"},
		{"make install with flags", "make -C deploy install\n", "make install"},
		{"a go command in an unquoted heredoc body", "cat <<EOF > f\n$(go build ./...)\nEOF\n", "go build"},
		{"a go command after a # in a parameter expansion", "name=\"${f#x}\" && go build ./...\n", "go build"},
		{"a go command after an unterminated quoted heredoc", "cat > f <<'MD'\ngo build ./...\n", "go build"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := goCommand(tc.text); got != tc.want {
				t.Fatalf("goCommand(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestScanFindsNoGoCommandInData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
	}{
		{"a single-quoted pattern", "grep -q 'go build' README.md\n"},
		{"another tool whose name ends in go", "cargo build --release\n"},
		{"a double-quoted argument", "echo \"go test ./...\"\n"},
		{"a comment", "# go build is slow\nset -euo pipefail\n"},
		{"a trailing comment", "echo hi # go run ./cmd/gt\n"},
		{"a quoted heredoc body", "cat > f <<'MD'\ngo test ./...\nMD\n"},
		{"a tab-stripped quoted heredoc body", "cat > f <<-'MD'\n\tgo test ./...\n\tMD\n"},
		{"a quoted string ending the line", "fail \"the gate ran\" 'go build'\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := goCommand(tc.text); got != "" {
				t.Fatalf("goCommand(%q) = %q, want no go command", tc.text, got)
			}
		})
	}
}
