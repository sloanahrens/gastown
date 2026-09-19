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
		for sc.Scan() {
			n++
			if shellInterpolatedVar.MatchString(sc.Text()) && offendingVars(sc.Text()) {
				t.Errorf("%s:%d interpolates a template var inside a quoted shell string: %s", name, n, strings.TrimSpace(sc.Text()))
			}
		}
	}
}
