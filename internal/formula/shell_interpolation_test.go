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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// The -s/-m test above only sees double-quoted mail arguments. The same
// injection exists on any command line in a shell block: `gh pr view {{pr_url}}`
// splits on whitespace, `--set problem="{{problem}}"` breaks on a quote. A
// template var may appear in a shell block only when it is machine-shaped, or
// on the body line of a quoted heredoc (<<'EOF'), which the shell never expands.
var (
	shellFenceOpen = regexp.MustCompile("^\\s*```(bash|sh|shell)\\s*$")
	fenceMarker    = regexp.MustCompile("^\\s*```")
	quotedHeredoc  = regexp.MustCompile(`<<-?\s*'([A-Za-z_][A-Za-z0-9_]*)'`)
)

// Allowed beyond systemGeneratedVar, all machine-shaped: review_id is a slug
// the agent picks (lowercase letters, digits, hyphens), convoy and
// resolved_issue are bead ids, and the rest are slugs and counts the operator
// configures. None can carry human prose.
var machineShapedShellVar = regexp.MustCompile(`\{\{(review_id|convoy|resolved_issue|repo|patrol_label|slice_docs|slice_go|max_open_beads|scan_interval_seconds)\}\}`)

// The *_command vars are the operator's own command lines: rendering one into a
// shell block is the formula's purpose, so they are exempt rather than bound.
var shellCommandVar = regexp.MustCompile(`\{\{(setup|build|typecheck|lint|test)_command\}\}`)

var shellLineAllowedVar = regexp.MustCompile(systemGeneratedVar.String() + `|` + machineShapedShellVar.String() + `|` + shellCommandVar.String())

// shellLineViolations returns "line: text" for each line inside a shell fence
// that carries a template var outside the allowlist and outside a quoted
// heredoc body. Unquoted heredoc bodies are not exempt: the shell expands them.
func shellLineViolations(content string) []string {
	var out []string
	inShell, quotedEnd := false, ""
	for _, line := range strings.Split(content, "\n") {
		if quotedEnd != "" {
			if line == quotedEnd {
				quotedEnd = ""
			}
			continue
		}
		if fenceMarker.MatchString(line) {
			inShell = !inShell && shellFenceOpen.MatchString(line)
			continue
		}
		if !inShell {
			continue
		}
		if m := quotedHeredoc.FindStringSubmatch(line); m != nil {
			quotedEnd = m[1]
		}
		for _, v := range anyVar.FindAllString(line, -1) {
			if !shellLineAllowedVar.MatchString(v) {
				out = append(out, strings.TrimSpace(line))
				break
			}
		}
	}
	return out
}

// Every shipped formula obeys the rule, so a new one is covered the moment it
// lands and no list has to be kept in step with the directory (gt-totbn).
func TestShippedFormulasBindFreeTextVarsOnlyInQuotedHeredocs(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(formulasFS, "formulas")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		checked++
		name := "formulas/" + e.Name()
		data, err := formulasFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range shellLineViolations(string(data)) {
			t.Errorf("%s: template var on a shell line outside a quoted heredoc: %s", e.Name(), l)
		}
	}
	if checked == 0 {
		t.Error("no formula checked; the sweep is looking at nothing")
	}
}

func TestShellLineViolationsCatchesUnquotedVars(t *testing.T) {
	t.Parallel()
	fence := func(body string) string { return "prose\n```bash\n" + body + "\n```\n" }
	bad := []string{
		`gt formula run shiny --set problem="{{problem}}"`,
		`ls -la {{scope}}`,
		`gh pr view {{pr_url}}`,
		`bd update {{issue}} --notes "{{focus}}"`,
		"cat <<EOF\n{{problem}}\nEOF",
		"X=$(cat <<'EOF'\nok\nEOF\n)\necho {{problem}}",
		// The allowlist matches the whole placeholder: a field on an allowed
		// id is free text and must still bind through a quoted heredoc.
		`echo "convoy: {{convoy.title}}"`,
	}
	for _, b := range bad {
		if len(shellLineViolations(fence(b))) == 0 {
			t.Errorf("not flagged: %q", b)
		}
	}
	good := []string{
		`bd show {{issue}}`,
		`cat .prd-reviews/{{review_id}}/prd-draft.md`,
		"P=$(cat <<'EOF'\n{{problem}}\nEOF\n)\ngh pr view \"$P\"",
		`bd show {{convoy}}`,
		`gh pr list --repo {{repo}}`,
		`{{build_command}}`,
		`sleep {{scan_interval_seconds}}`,
	}
	for _, g := range good {
		if v := shellLineViolations(fence(g)); len(v) != 0 {
			t.Errorf("flagged %q: %v", g, v)
		}
	}
	// Text outside a shell fence is prose or output for a human, not a command.
	if v := shellLineViolations("```\nTitle: {{problem}}\n```\nUse {{problem}}"); len(v) != 0 {
		t.Errorf("flagged prose: %v", v)
	}
}
