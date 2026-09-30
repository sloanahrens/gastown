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

// The two polecat review formulas take a scope, a PR URL and a focus from the
// bead (gt-reafp). Those are attacker-shaped text, so the only place they may
// be rendered is the lone body line of a quoted heredoc that assigns a shell
// variable; every command reads the quoted variable.
var untrustedReviewVar = regexp.MustCompile(`\{\{(scope|pr_url|focus)\}\}`)
var quotedHeredocOpen = regexp.MustCompile(`<<-?\s*'([A-Za-z_][A-Za-z0-9_]*)'\s*$`)

func TestReviewFormulasBindUntrustedVarsThroughQuotedHeredoc(t *testing.T) {
	for _, f := range []string{"mol-polecat-code-review", "mol-polecat-review-pr"} {
		name := "formulas/" + f + ".formula.toml"
		data, err := formulasFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		seen := 0
		for i, line := range lines {
			m := untrustedReviewVar.FindString(line)
			if m == "" {
				continue
			}
			seen++
			if strings.TrimSpace(line) != m {
				t.Errorf("%s:%d renders %s among other text: %s", name, i+1, m, strings.TrimSpace(line))
				continue
			}
			open := quotedHeredocOpen.FindStringSubmatch(lines[i-1])
			if open == nil {
				t.Errorf("%s:%d renders %s outside a quoted heredoc body", name, i+1, m)
				continue
			}
			if strings.TrimSpace(lines[i+1]) != open[1] {
				t.Errorf("%s:%d %s must be followed by the heredoc terminator %s", name, i+1, m, open[1])
			}
			if !strings.Contains(lines[i-1], "$(cat <<") {
				t.Errorf("%s:%d %s heredoc must assign a shell variable via $(cat <<'...'), got: %s", name, i+1, m, strings.TrimSpace(lines[i-1]))
			}
		}
		if seen == 0 {
			t.Errorf("%s: no untrusted var found; the test is checking nothing", name)
		}
	}
}

// Path commands must take the validated, quoted variable, never a bare
// expansion that a leading dash could turn into a flag.
func TestCodeReviewFormulaValidatesScopeBeforePathCommands(t *testing.T) {
	data, err := formulasFS.ReadFile("formulas/mol-polecat-code-review.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `case "$SCOPE" in ''|-*|*[!A-Za-z0-9._/-]*) SCOPE_PATH=""`) {
		t.Error("scope allowlist (no leading dash, [A-Za-z0-9._/-] only) is missing")
	}
	bare := regexp.MustCompile(`(?m)^\s*(ls|head|find|tree|wc|cat|git log)\b[^\n]*\$SCOPE(\b|[^_])`)
	if m := bare.FindString(s); m != "" {
		t.Errorf("path command reads $SCOPE instead of the validated $SCOPE_PATH: %s", strings.TrimSpace(m))
	}
	code := regexp.MustCompile("`[^`\n]+`").ReplaceAllString(s, "")
	if regexp.MustCompile(`[^"]\$SCOPE_PATH`).MatchString(strings.ReplaceAll(code, `"$SCOPE_PATH"`, "")) {
		t.Error("$SCOPE_PATH must always be double-quoted")
	}
}

func TestReviewPRFormulaValidatesPRURL(t *testing.T) {
	data, err := formulasFS.ReadFile("formulas/mol-polecat-review-pr.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `https://github.com/*/*/pull/[0-9]*`) || !strings.Contains(s, `*[!A-Za-z0-9._/:-]*`) {
		t.Error("PR URL allowlist is missing")
	}
	if bare := regexp.MustCompile(`gh pr \w+ [^"\n]*\$PR_URL`).FindString(s); bare != "" {
		t.Errorf("gh reads an unquoted $PR_URL: %s", bare)
	}
}
