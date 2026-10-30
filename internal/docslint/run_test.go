package docslint

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// fence is a code fence, spelled out because a raw string cannot hold backticks.
const fence = "```"

// fixtureTree is an in-memory checkout with one violation per rule, one clean
// file per tier and a plugin of 2102 words, over the ceiling.
func fixtureTree() memFS {
	return memFS{
		"AGENTS.md":           "# Agents\n\nShort and fine.\n",
		"README.md":           "# Readme\n\nSee [the guide](docs/guides/guide.md).\n",
		"Makefile":            "build:\n\t@true\n\ntest:\n\t@true\n",
		"docs/HOOKS.md":       "# Hooks\n\nRun `make build` first.\n",
		"docs/design/live.md": "# Live architecture\n\nStill true.\n",
		"docs/design/old.md":  "> Status: historical (2026-07). Abandoned: gt-0000. Not maintained.\n\n# Old design\n",
		"docs/guides/guide.md": "# Guide\n\nGood link: [hooks](../HOOKS.md).\nBad link: [missing](../missing.md).\n" +
			"Good target: `make build`.\nBad target: `make nope`.\n" +
			"Prose that must not match: we make decisions here.\n" +
			"External: [site](https://example.com) and [anchor](#guide).\n",
		"docs/guides/marked.md": "# Marked\n\nA line with raw tool-call markup:\n\n<tool_call>\n<function=Edit>\n<parameter=file_path>\n",
		"docs/plans/conflict.md": "> Status: historical (2026-07). Abandoned: gt-0000. Not maintained.\n\n" +
			"> Status: historical (2026-08). Abandoned: gt-0000. Not maintained.\n\n# Old plan with two status lines\n",
		"docs/plans/old-plan.md":                       "# Old plan\n\nNo header here.\n",
		"docs/research/survey.md":                      "> Status: historical (2026-07). Merged in: abc123. Not maintained.\n\n# Research\n",
		"internal/pkg/a.go":                            "package pkg\n",
		"internal/pkg/a_test.go":                       "package pkg\n",
		"internal/pkg/b.go":                            "package pkg\n",
		"internal/templates/polecat-CLAUDE.md":         "# Polecat\n\nfine\n",
		"internal/formula/formulas/mol-x.formula.toml": "description = \"\"\"\nA formula. See [guide](../../../docs/guides/guide.md).\n\"\"\"\nformula = \"mol-x\"\nversion = 1\n",
		"internal/formula/formulas/mol-release.formula.toml": "description = \"\"\"\nA release formula.\n\n## Usage\n\n" +
			fence + "bash\ngt mol wisp create mol-release --var version=1.0.0\n" + fence + "\n\nOr assign to a polecat:\n" +
			fence + "bash\ngt sling rig/polecats/p1 --formula mol-release --var version=1.0.0\n" + fence + "\n\"\"\"\n" +
			"formula = \"mol-release\"\ntype = \"workflow\"\nversion = 1\n\n[[steps]]\nid = \"push-main\"\ntitle = \"Push to main\"\n" +
			"description = \"\"\"\nPush the release commit.\n\n" + fence + "bash\ngit push origin main\n" + fence + "\n\"\"\"\n",
		"plugins/p/plugin.md": "# Plugin\n\n" + strings.Repeat("word ", 2100) + "\n",
	}
}

// cleanFixture is fixtureTree with every violation removed or fixed.
func cleanFixture() memFS {
	m := fixtureTree()
	delete(m, "plugins/p/plugin.md")
	delete(m, "internal/formula/formulas/mol-release.formula.toml")
	delete(m, "docs/guides/marked.md")
	delete(m, "docs/plans/conflict.md")
	m["docs/plans/old-plan.md"] = "> Status: historical (2026-01). Abandoned: none. Not maintained.\n\n# Old plan\n"
	m["docs/guides/guide.md"] = "# Guide\n\nGood link: [hooks](../HOOKS.md).\nGood target: `make build`.\n"
	return m
}

// runDocsLint runs the command over fsys with the environment given, and returns
// stdout, stderr and the exit code.
func runDocsLint(t *testing.T, fsys FS, git Git, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, Env{
		Getenv:   func(k string) string { return env[k] },
		TopLevel: func() (string, error) { return "/repo", nil },
		NewFS:    func(string) FS { return fsys },
		NewGit:   func(string) Git { return git },
	}, &out, &errb)
	return out.String(), errb.String(), code
}

// readFailsFS is memFS with one file whose read fails.
type readFailsFS struct {
	memFS
	path string
}

func (r readFailsFS) ReadFile(p string) ([]byte, error) {
	if p == r.path {
		return nil, fs.ErrPermission
	}
	return r.memFS.ReadFile(p)
}

// TestRunFindingsAreSortedAndExitOne runs all seven checks over the fixture and
// compares the exact lines, and checks a line 9 sorts before a line 10.
func TestRunFindingsAreSortedAndExitOne(t *testing.T) {
	t.Parallel()
	t.Run("one fixture tree, one violation per rule", func(t *testing.T) {
		t.Parallel()
		out, errOut, code := runDocsLint(t, fixtureTree(), fakeGit{}, nil)
		want := `docs/guides/guide.md:4: dead-link: ../missing.md does not exist
docs/guides/guide.md:6: dead-make-target: make nope is not a Makefile target
docs/guides/marked.md:5: stray-markup: committed tool-call or merge-conflict markup
docs/guides/marked.md:6: stray-markup: committed tool-call or merge-conflict markup
docs/guides/marked.md:7: stray-markup: committed tool-call or merge-conflict markup
docs/plans/conflict.md:1: status-conflict: more than one "> Status:" line
docs/plans/old-plan.md:1: status-header: first non-blank line must start with "> Status:"
internal/formula/formulas/mol-release.formula.toml:12: polecat-main-push: pushes main/master and offers a polecat; the guards refuse a polecat session (gt-ibt8)
plugins/p/plugin.md:1: word-ceiling: 2102 words, ceiling 1500
`
		if out != want {
			t.Errorf("findings =\n%s\nwant\n%s", out, want)
		}
		if code != ExitFindings {
			t.Errorf("exit = %d, want %d", code, ExitFindings)
		}
		if errOut != "" {
			t.Errorf("stderr = %q, want empty", errOut)
		}
	})
	t.Run("line numbers sort numerically", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		for i := 1; i <= 10; i++ {
			b.WriteString("[x](nope" + string(rune('a'+i-1)) + ".md)\n")
		}
		fsys := memFS{"AGENTS.md": b.String()}
		out, _, code := runDocsLint(t, fsys, fakeGit{}, nil)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 10 || !strings.HasPrefix(lines[8], "AGENTS.md:9:") || !strings.HasPrefix(lines[9], "AGENTS.md:10:") {
			t.Errorf("line 9 must come before line 10:\n%s", out)
		}
		if code != ExitFindings {
			t.Errorf("exit = %d, want %d", code, ExitFindings)
		}
	})
	t.Run("ties by path then line then text", func(t *testing.T) {
		t.Parallel()
		got := []Finding{
			{File: "b.md", Line: 1, Rule: "x", Message: "m"},
			{File: "a.md", Line: 2, Rule: "x", Message: "m"},
			{File: "a.md", Line: 1, Rule: "z", Message: "m"},
			{File: "a.md", Line: 1, Rule: "a", Message: "m"},
		}
		sortFindings(got)
		want := []string{"a.md:1: a: m", "a.md:1: z: m", "a.md:2: x: m", "b.md:1: x: m"}
		for i, f := range got {
			if f.String() != want[i] {
				t.Errorf("finding %d = %q, want %q", i, f, want[i])
			}
		}
	})
}

// TestRunCleanTreeExitsZero checks a clean tree prints nothing and exits 0, and
// that a formula over the ceiling is a finding unless it is exempt.
func TestRunCleanTreeExitsZero(t *testing.T) {
	t.Parallel()
	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		out, errOut, code := runDocsLint(t, cleanFixture(), fakeGit{}, nil)
		if out != "" || errOut != "" || code != ExitClean {
			t.Errorf("clean tree: stdout %q stderr %q exit %d, want silent exit 0", out, errOut, code)
		}
	})
	t.Run("formulas are held to the ceiling unless exempt", func(t *testing.T) {
		t.Parallel()
		m := cleanFixture()
		big := fixtureTree()["plugins/p/plugin.md"]
		m["internal/formula/formulas/mol-big.formula.toml"] = big
		m["internal/formula/formulas/mol-polecat-work.formula.toml"] = big
		out, _, code := runDocsLint(t, m, fakeGit{}, nil)
		want := "internal/formula/formulas/mol-big.formula.toml:1: word-ceiling: 2102 words, ceiling 1500\n"
		if out != want || code != ExitFindings {
			t.Errorf("stdout %q exit %d, want %q exit 1", out, code, want)
		}
	})
}

// TestRunEmptyGlobIsAnEmptyTier checks a root with none of the paths a tier scans
// is an empty tier: findings still list, and a clean one exits 0 (gt-et39).
func TestRunEmptyGlobIsAnEmptyTier(t *testing.T) {
	t.Parallel()
	t.Run("a finding still prints", func(t *testing.T) {
		t.Parallel()
		fsys := memFS{"AGENTS.md": "# Agents\n\nSee [missing](docs/nope.md).\n"}
		out, _, code := runDocsLint(t, fsys, fakeGit{}, nil)
		if want := "AGENTS.md:3: dead-link: docs/nope.md does not exist\n"; out != want || code != ExitFindings {
			t.Errorf("stdout %q exit %d, want %q exit 1", out, code, want)
		}
	})
	t.Run("clean is silent and exits 0", func(t *testing.T) {
		t.Parallel()
		fsys := memFS{"AGENTS.md": "# Agents\n\nNo links here.\n"}
		out, errOut, code := runDocsLint(t, fsys, fakeGit{}, nil)
		if out != "" || errOut != "" || code != ExitClean {
			t.Errorf("stdout %q stderr %q exit %d, want silent exit 0", out, errOut, code)
		}
		list, _, lcode := runDocsLint(t, fsys, fakeGit{}, nil, "--list", "agent-facing")
		if list != "AGENTS.md\n" || lcode != ExitClean {
			t.Errorf("--list agent-facing = %q exit %d", list, lcode)
		}
		gos, _, gcode := runDocsLint(t, fsys, fakeGit{}, nil, "--list", "go")
		if gos != "" || gcode != ExitClean {
			t.Errorf("--list go on a root without cmd/ or internal/ = %q exit %d, want empty exit 0", gos, gcode)
		}
	})
}

// TestRunCheckThatCannotRunIsExitTwo checks an unreadable file is exit 2 with the
// reason on stderr, the findings of the checks that did run are still printed
// first, and an error with no findings is never exit 1.
func TestRunCheckThatCannotRunIsExitTwo(t *testing.T) {
	t.Parallel()
	t.Run("findings are printed, then exit 2", func(t *testing.T) {
		t.Parallel()
		fsys := readFailsFS{memFS: fixtureTree(), path: "docs/HOOKS.md"}
		out, errOut, code := runDocsLint(t, fsys, fakeGit{}, nil)
		if code != ExitFailed {
			t.Errorf("exit = %d, want %d", code, ExitFailed)
		}
		// The dead-link and dead-make-target checks read docs/HOOKS.md, so they are
		// the checks that could not run; the others did, and their findings print.
		for _, want := range []string{
			"docs/plans/old-plan.md:1: status-header:",
			"internal/formula/formulas/mol-release.formula.toml:12: polecat-main-push:",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("a finding from a check that ran is missing from stdout (%s):\n%s", want, out)
			}
		}
		if strings.Contains(out, "dead-link") {
			t.Errorf("a finding from a check that could not run was printed:\n%s", out)
		}
		if !strings.Contains(errOut, "docs-lint: ") || !strings.Contains(errOut, "docs/HOOKS.md") {
			t.Errorf("stderr = %q, want the message naming docs/HOOKS.md", errOut)
		}
	})
	t.Run("an error with no findings is exit 2, never 1", func(t *testing.T) {
		t.Parallel()
		fsys := readFailsFS{memFS: memFS{"AGENTS.md": "# Agents\n", "docs/HOOKS.md": "# Hooks\n"}, path: "docs/HOOKS.md"}
		out, errOut, code := runDocsLint(t, fsys, fakeGit{}, nil)
		if code != ExitFailed {
			t.Errorf("exit = %d, want %d (an error with no finding is not exit 1)", code, ExitFailed)
		}
		if out != "" {
			t.Errorf("stdout = %q, want nothing", out)
		}
		if errOut == "" {
			t.Errorf("an erroring check was silent")
		}
	})
	t.Run("--words stops with nothing on stdout", func(t *testing.T) {
		t.Parallel()
		fsys := readFailsFS{memFS: memFS{"AGENTS.md": "three words here\n", "docs/HOOKS.md": "# Hooks\n"}, path: "docs/HOOKS.md"}
		out, errOut, code := runDocsLint(t, fsys, fakeGit{}, nil, "--words")
		if code != ExitFailed || out != "" || !strings.Contains(errOut, "docs/HOOKS.md") {
			t.Errorf("--words: stdout %q stderr %q exit %d, want exit 2, no partial total, the file named", out, errOut, code)
		}
	})
}

// TestRunOptions covers the command line and the environment: the listings, the
// slice, the word total, the refusals and DOCS_LINT_CEILING and DOCS_LINT_ROOT.
func TestRunOptions(t *testing.T) {
	t.Parallel()
	const (
		jan = 1767225600 // 2026-01-01
		feb = 1769904000 // 2026-02-01
		mar = 1772323200 // 2026-03-01
	)
	dates := fakeGit{
		"docs/design/live.md": jan, "internal/pkg/b.go": jan,
		"docs/HOOKS.md": feb, "internal/pkg/a.go": feb,
		"AGENTS.md": mar, "README.md": mar, "docs/guides/guide.md": mar, "docs/guides/marked.md": mar,
		"internal/templates/polecat-CLAUDE.md": mar, "plugins/p/plugin.md": mar,
		"internal/formula/formulas/mol-release.formula.toml": mar, "internal/formula/formulas/mol-x.formula.toml": mar,
	}
	fsys := fixtureTree()
	t.Run("slice is least recently committed first, docs then go", func(t *testing.T) {
		t.Parallel()
		out, _, code := runDocsLint(t, fsys, dates, nil, "--slice", "2", "1")
		if want := "docs/design/live.md\ndocs/HOOKS.md\ninternal/pkg/b.go\n"; out != want || code != ExitClean {
			t.Errorf("--slice 2 1 = %q exit %d, want %q", out, code, want)
		}
	})
	t.Run("words is the agent-facing total", func(t *testing.T) {
		t.Parallel()
		out, _, code := runDocsLint(t, fsys, dates, nil, "--words")
		if code != ExitClean || strings.TrimSpace(out) == "" || strings.Trim(out, "0123456789\n") != "" {
			t.Errorf("--words = %q exit %d, want a number", out, code)
		}
	})
	t.Run("refusals are exit 2 with the reason", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			args []string
			env  map[string]string
			want string
		}{
			{name: "unknown option", args: []string{"--nope"}, want: "docs-lint: unknown option '--nope'"},
			{name: "list needs a tier", args: []string{"--list"}, want: "--list needs one tier"},
			{name: "unknown tier", args: []string{"--list", "nope"}, want: "docs-lint: unknown tier 'nope'"},
			{name: "slice needs two counts", args: []string{"--slice", "2"}, want: "--slice needs two counts"},
			{name: "slice counts are numbers", args: []string{"--slice", "x", "1"}, want: "--slice counts must be whole numbers"},
			{name: "slice counts are not negative", args: []string{"--slice", "-1", "1"}, want: "--slice counts must be whole numbers"},
			{name: "words takes no argument", args: []string{"--words", "x"}, want: "--words takes no argument"},
			{name: "ceiling is not a number", env: map[string]string{"DOCS_LINT_CEILING": "lots"}, want: "DOCS_LINT_CEILING 'lots' is not a number of words"},
			{name: "ceiling is not negative", env: map[string]string{"DOCS_LINT_CEILING": "-5"}, want: "DOCS_LINT_CEILING '-5' is not a number of words"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				out, errOut, code := runDocsLint(t, fsys, dates, tc.env, tc.args...)
				if code != ExitFailed || out != "" || !strings.Contains(errOut, tc.want) {
					t.Errorf("stdout %q stderr %q exit %d, want exit 2 with %q", out, errOut, code, tc.want)
				}
			})
		}
	})
	t.Run("a ceiling that is not a number does not stop a listing", func(t *testing.T) {
		t.Parallel()
		out, _, code := runDocsLint(t, fsys, dates, map[string]string{"DOCS_LINT_CEILING": "lots"}, "--list", "go")
		if code != ExitClean || out != "internal/pkg/a.go\ninternal/pkg/b.go\n" {
			t.Errorf("--list go with a bad ceiling = %q exit %d", out, code)
		}
	})
	t.Run("the ceiling moves the word-ceiling verdict", func(t *testing.T) {
		t.Parallel()
		m := memFS{"AGENTS.md": "a b c d e\n"}
		if out, _, code := runDocsLint(t, m, dates, map[string]string{"DOCS_LINT_CEILING": "5"}); out != "" || code != ExitClean {
			t.Errorf("at the ceiling: %q exit %d, want clean", out, code)
		}
		out, _, code := runDocsLint(t, m, dates, map[string]string{"DOCS_LINT_CEILING": "4"})
		if want := "AGENTS.md:1: word-ceiling: 5 words, ceiling 4\n"; out != want || code != ExitFindings {
			t.Errorf("one over the ceiling: %q exit %d, want %q", out, code, want)
		}
	})
	t.Run("the root: DOCS_LINT_ROOT, else the git top level, else a refusal", func(t *testing.T) {
		t.Parallel()
		var gotRoot string
		newFS := func(root string) FS { gotRoot = root; return fsys }
		run := func(env map[string]string, top func() (string, error)) (string, int) {
			var out, errb bytes.Buffer
			code := Run([]string{"--list", "go"}, Env{
				Getenv:   func(k string) string { return env[k] },
				TopLevel: top,
				NewFS:    newFS,
				NewGit:   func(string) Git { return dates },
			}, &out, &errb)
			return errb.String(), code
		}
		if _, code := run(map[string]string{"DOCS_LINT_ROOT": "/named"}, func() (string, error) { return "/top", nil }); code != ExitClean || gotRoot != "/named" {
			t.Errorf("DOCS_LINT_ROOT: root %q exit %d, want /named", gotRoot, code)
		}
		if _, code := run(nil, func() (string, error) { return "/top", nil }); code != ExitClean || gotRoot != "/top" {
			t.Errorf("git top level: root %q exit %d, want /top", gotRoot, code)
		}
		errText, code := run(nil, func() (string, error) { return "", errors.New("not a git repository") })
		if code != ExitFailed || !strings.Contains(errText, "cannot find the repository root") || !strings.Contains(errText, "DOCS_LINT_ROOT") {
			t.Errorf("no root: stderr %q exit %d, want a refusal naming DOCS_LINT_ROOT", errText, code)
		}
	})
}

// TestRunCaseTable runs the tier listings and the word ceiling and total over the
// shared fixture tree, one subtest each. No locale case is needed: the count is
// strings.Fields, so no locale can change it, and TestWordCountIsLocaleFree pins
// that.
func TestRunCaseTable(t *testing.T) {
	t.Parallel()
	fsys := fixtureTree()
	listings := []struct{ tier, want string }{
		{"historical", "docs/design/old.md\ndocs/plans/conflict.md\ndocs/plans/old-plan.md\ndocs/research/survey.md\n"},
		{"reference", "README.md\ndocs/design/live.md\ndocs/guides/guide.md\ndocs/guides/marked.md\n"},
		{"agent-facing", "AGENTS.md\ndocs/HOOKS.md\ninternal/formula/formulas/mol-release.formula.toml\ninternal/formula/formulas/mol-x.formula.toml\ninternal/templates/polecat-CLAUDE.md\nplugins/p/plugin.md\n"},
		{"go", "internal/pkg/a.go\ninternal/pkg/b.go\n"},
	}
	for _, tc := range listings {
		t.Run("--list "+tc.tier, func(t *testing.T) {
			t.Parallel()
			out, _, code := runDocsLint(t, fsys, fakeGit{}, nil, "--list", tc.tier)
			if out != tc.want || code != ExitClean {
				t.Errorf("--list %s = %q exit %d, want %q", tc.tier, out, code, tc.want)
			}
		})
	}
	t.Run("the ceiling and --words count a lone em dash as a word", func(t *testing.T) {
		t.Parallel()
		text := strings.Repeat("a ", 1498) + strings.Repeat("— ", 5) + "\n"
		m := memFS{"AGENTS.md": text}
		out, _, code := runDocsLint(t, m, fakeGit{}, nil)
		if want := "AGENTS.md:1: word-ceiling: 1503 words, ceiling 1500\n"; out != want || code != ExitFindings {
			t.Errorf("ceiling: %q exit %d, want %q", out, code, want)
		}
		words, _, wcode := runDocsLint(t, m, fakeGit{}, nil, "--words")
		if words != "1503\n" || wcode != ExitClean {
			t.Errorf("--words = %q exit %d, want 1503", words, wcode)
		}
	})
	t.Run("a finding after a fence carries its real line number", func(t *testing.T) {
		t.Parallel()
		m := memFS{"AGENTS.md": "# Agents\n\n" + fence + "\ncode\n" + fence + "\n\n[x](gone.md)\n"}
		out, _, _ := runDocsLint(t, m, fakeGit{}, nil)
		if want := "AGENTS.md:7: dead-link: gone.md does not exist\n"; out != want {
			t.Errorf("finding = %q, want %q (its line in the file, not its line after the fence is removed)", out, want)
		}
	})
}

// TestWithPrefix checks an error is printed under the command's name once.
func TestWithPrefix(t *testing.T) {
	t.Parallel()
	if got := withPrefix(errors.New("reading x: denied")); got != "docs-lint: reading x: denied" {
		t.Errorf("withPrefix = %q", got)
	}
	if got := withPrefix(errors.New("docs-lint: unknown tier 'x'")); got != "docs-lint: unknown tier 'x'" {
		t.Errorf("withPrefix doubled the prefix: %q", got)
	}
}

// TestRunCountFailureNamesTheFile checks a file whose words cannot be counted stops
// each mode that counts: exit 2, nothing on stdout (no partial total, no finding
// from the failing check), and a stderr line under the command's name that names
// the file.
func TestRunCountFailureNamesTheFile(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name string
		args []string
	}{{"the lint", nil}, {"--words", []string{"--words"}}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			// AGENTS.md sorts before docs/HOOKS.md, so the first file counts and
			// the second does not: a run that carried on, or printed what it had,
			// would show it.
			fsys := readFailsFS{memFS: memFS{"AGENTS.md": "three words here\n", "docs/HOOKS.md": "# Hooks\n"}, path: "docs/HOOKS.md"}
			out, errOut, code := runDocsLint(t, fsys, fakeGit{}, nil, mode.args...)
			if code != ExitFailed {
				t.Errorf("exit = %d, want %d", code, ExitFailed)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if !strings.HasPrefix(errOut, "docs-lint: ") || !strings.Contains(errOut, "docs/HOOKS.md") {
				t.Errorf("stderr = %q, want a docs-lint: line naming docs/HOOKS.md", errOut)
			}
		})
	}
}
