package formula

import (
	"bufio"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// A template variable inside a double-quoted shell argument is rendered by
// text/template with no escaping and then executed by an agent's shell
// (gt-ugcf, D7): a quote, backtick or $ in the value breaks or injects. Shipped
// formulas must pass such values through a shell variable or a quoted heredoc.
var shellInterpolatedVar = regexp.MustCompile(`-[sm] "[^"]*\{\{`)

// Variables whose values the system generates (bead ids, polecat and rig
// names, branch names, counts, dates, versions) never carry human prose, so a
// double-quoted use of them cannot inject. Everything else (titles, reports,
// summaries, scopes, URLs, problem statements) must go through a quoted
// heredoc or --stdin.
var systemGeneratedVar = regexp.MustCompile(`\{\{(issue|version|polecat|rig|base_branch|date|total_count|escalate_count|total_cleaned|orphan_count)\}\}`)

var anyVar = regexp.MustCompile(`\{\{[^}]+\}\}`)

// An unquoted heredoc delimiter (<<EOF rather than <<'EOF') lets the shell
// expand $, backticks and \ inside the body, so a rendered free-text value on
// a body line is the same injection with the variable one line further down.
var unquotedHeredoc = regexp.MustCompile(`<<-?\s*([A-Za-z_][A-Za-z0-9_]*)\b`)

// offendingVars reports whether the line carries a template variable that is
// not on the system-generated allowlist.
func offendingVars(line string) bool {
	for _, m := range anyVar.FindAllString(line, -1) {
		if !systemGeneratedVar.MatchString(m) {
			return true
		}
	}
	return false
}

func TestShippedFormulasDoNotInterpolateVarsIntoShellStrings(t *testing.T) {
	entries, err := fs.ReadDir(formulasFS, "formulas")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := "formulas/" + e.Name()
		data, err := formulasFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		n := 0
		heredocEnd := "" // terminator of an UNQUOTED heredoc we are inside, else ""
		for sc.Scan() {
			n++
			line := sc.Text()
			if heredocEnd != "" {
				if strings.TrimSpace(line) == heredocEnd {
					heredocEnd = ""
				} else if offendingVars(line) {
					t.Errorf("%s:%d interpolates a template var inside an unquoted heredoc (the shell expands it): %s", name, n, strings.TrimSpace(line))
				}
				continue
			}
			if m := unquotedHeredoc.FindStringSubmatch(line); m != nil {
				heredocEnd = m[1]
			}
			if shellInterpolatedVar.MatchString(line) && offendingVars(line) {
				t.Errorf("%s:%d interpolates a template var inside a quoted shell string: %s", name, n, strings.TrimSpace(line))
			}
		}
	}
}
