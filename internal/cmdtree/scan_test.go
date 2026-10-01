package cmdtree

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// refStrings renders refs as "line:bin words" for compact comparison.
func refStrings(refs []Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		line := strconv.Itoa(r.Line) + ":" + r.Bin + " " + strings.Join(r.Words, " ")
		if r.Comment {
			line += " #"
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

func assertRefs(t *testing.T, got []Ref, want ...string) {
	t.Helper()
	g := strings.Join(refStrings(got), " | ")
	w := strings.Join(want, " | ")
	if g != w {
		t.Fatalf("refs:\n got  %s\n want %s", g, w)
	}
}

func TestScanShell(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		"cd ~/gt/gastown && gt rig list",         // 1: path is not an invocation
		"bd close gt-abc --reason done",          // 2
		"for rig in $(gt rigs --names); do",      // 3
		"gt rigs                     # all rigs", // 4: trailing comment stops words
		"# gt context --usage",                   // 5: comment line skipped
		"// bd daemons list",                     // 6: comment line skipped
		`"command": "gt tap guard pr-workflow"`,  // 7
		"gt prime; bash -c 'gt mail inbox'",      // 8: two invocations, punctuation stops
		"gt --version && bd",                     // 9: no command words
		"x=$(bd list --json | jq length)",        // 10
		`log "Installing gt from $RIG_ROOT"`,     // 11: text inside an open quote
		`echo 'Run gt prime first'`,              // 12: text inside an open quote
		`echo "done" && gt done`,                 // 13: quote closed before gt
		`R="${GT_TOWN_ROOT:-$(gt town root)}"`,   // 14: closing braces stop words
		"# The gt boot command handles this",     // 16: comment prose
		"# - Close with: bd gate close <id>",     // 17: comment after a colon
		`a=[$(bd list)]`,                         // 17                         // 15
	}, "\n")
	assertRefs(t, ScanShell("f.sh", text, 1),
		"1:gt rig list",
		"2:bd close gt-abc",
		"3:gt rigs",
		"4:gt rigs",
		"5:gt context #",
		"6:bd daemons list #",
		"7:gt tap guard pr-workflow",
		"8:gt prime",
		"8:gt mail inbox",
		"10:bd list",
		"13:gt done",
		"14:gt town root",
		"16:bd gate close #",
		"17:bd list",
	)
}

func TestScanScript(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		"gt rig list", // 1
		`echo "gt does not land integration branches"`, // 2: quoted log text
		`if pgrep -f "bd daemon" >/dev/null; then`,     // 3: quoted pattern
		`echo '| run | bd timeouts |'`,                 // 4: | is not a separator in quotes
		`cd "$d" # bd operates from the parent`,        // 5: trailing comment
		`bash -c "gt prime --hook"`,                    // 6: sh -c command line
		`ssh host "cd /x && bd list"`,                  // 7: separator inside the quote
		"cat <<EOF",                                    // 8
		"  gt binary to use",                           // 9: here-document body
		"EOF",                                          // 10
		"# Run with: gt crew add",                      // 11: instruction comment
		`n=$(gt mq list --json)`,                       // 12
	}, "\n")
	assertRefs(t, ScanScript("f.sh", text),
		"1:gt rig list",
		"6:gt prime",
		"7:bd list",
		"11:gt crew add #",
		"12:gt mq list",
	)
}

func TestScanMarkdown(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		"The gt binary handles this, and bd is the tracker.", // 1: prose ignored
		"Run `gt mq close <id>` or `bd show x`.",             // 2: inline spans
		"```bash",                                            // 3
		"gt session kill rig/polecats/x",                     // 4
		"# bd sync  (comment)",                               // 5
		"```",                                                // 6
		"after the fence gt rigs is prose",                   // 7
		"log `Rogue bd check: clean` then stop",              // 8: span is prose
		"`cd x && gt done` and `FOO=1 bd list --json`",       // 9: command position
		"`$(gt rig list)` or `x | bd show y`",                // 10
	}, "\n")
	assertRefs(t, ScanMarkdown("f.md", text),
		"2:gt mq close",
		"2:bd show x",
		"4:gt session kill",
		"5:bd sync #",
		"9:gt done",
		"9:bd list",
		"10:gt rig list",
		"10:bd show y",
	)
}

// The role templates name the gt binary through the {{ cmd }} function, in any
// spacing or trim form, both in prose spans and in fenced shell.
func TestScanCmdTemplateIsGt(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		"Run `{{ cmd }} remember` now.",            // 1
		"Or `{{cmd}} worktree add`.",               // 2
		"Or `{{- cmd -}} nosuchcmd`.",              // 3
		"```bash",                                  // 4
		"{{ cmd }} mail send {{ .RigName }}/w",     // 5
		"x=$({{  cmd  }} rig list)",                // 6
		"```",                                      // 7
		"The {{ cmd }} binary is prose.",           // 8
		"A `{{ .Cmd }} nope` is not the function.", // 9
	}, "\n")
	assertRefs(t, ScanMarkdown("t.md", text),
		"1:gt remember",
		"2:gt worktree add",
		"3:gt nosuchcmd",
		"5:gt mail send",
		"6:gt rig list",
	)
}

func TestScanTOMLMarkdownKeepsPhysicalLines(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		`[[steps]]`,
		`id = "a"`,
		`description = "Check:\n\n` + "```" + `bash\ngt context --usage\nbd sync --status\n` + "```" + `\nDone."`,
		`[[steps]]`,
		`description = """`,
		"Use `gt rigs` here.",
		`"""`,
	}, "\n")
	assertRefs(t, ScanTOMLMarkdown("f.toml", text),
		"3:gt context",
		"3:bd sync",
		"6:gt rigs",
	)
}

func TestScanTOMLMarkdownResetsFenceAtNewKey(t *testing.T) {
	t.Parallel()
	// An unbalanced fence in one step must not turn the next step's prose into code.
	text := strings.Join([]string{
		`description = "` + "```" + `bash\ngt prime"`,
		`[[steps]]`,
		`description = "the gt binary is prose"`,
	}, "\n")
	assertRefs(t, ScanTOMLMarkdown("f.toml", text), "1:gt prime")
}

func TestScanGo(t *testing.T) {
	t.Parallel()
	src := `package x

import (
	"context"
	"os/exec"
)

func f(ctx context.Context, id string, args []string) {
	_ = exec.Command("gt", "swarm", "land", id)
	_ = exec.CommandContext(ctx, "bd", "mol", "wisp", "--json")
	_ = exec.Command("git", "status")
	_ = exec.Command("gt", args...)
	_ = BdCmd("dep", "add", id)
	_ = beads.CommandContext(ctx, "d", "b", beads.MutationPinned, "sync")
	_ = beads.Command("d", "b", beads.ReadOnlyPinned, args...)
	_ = beads.CommandWithPath(bin, "d", env, "show", id)
	_ = beads.CommandContextWithBin(ctx, bin, "d", "b", beads.ReadOnlyPinned, "ready")
}
`
	refs, err := ScanGo("x.go", []byte(src))
	if err != nil {
		t.Fatalf("ScanGo: %v", err)
	}
	assertRefs(t, refs,
		"9:gt swarm land",
		"10:bd mol wisp",
		"13:bd dep add",
		"14:bd sync",
		"16:bd show",
		"17:bd ready",
	)
}

func TestScanRepoSelectsFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/formula/formulas/a.formula.toml", "description = \"`gt one`\"\n")
	write("internal/templates/roles/r.md.tmpl", "`gt two`\n")
	write("plugins/p/plugin.md", "`gt three`\n")
	write("plugins/p/run.sh", "gt four\n")
	write("plugins/p/run_test.sh", "gt skipped\n")
	write("internal/hooks/templates/claude/s.json", `"gt five"`+"\n")
	write("scripts/guards/g.sh", "gt six\n")
	write("scripts/s.sh", "gt nine\n")
	write("scripts/s.py", "gt skipped\n")
	write(".githooks/pre-push", "gt ten\n")
	write(".claude/commands/c.md", "`gt twelve`\n")
	write(".claude/skills/k/SKILL.md", "`bd thirteen`\n")
	write("internal/config/roles/x.toml", "nudge = \"Run 'gt seven'\"\n")
	write("internal/x/x.go", "package x\nimport \"os/exec\"\nvar _ = exec.Command(\"gt\", \"eight\")\n")
	write("internal/x/x_test.go", "package x\nimport \"os/exec\"\nvar _ = exec.Command(\"gt\", \"skipped\")\n")
	write("internal/x/testdata/t.sh", "gt skipped\n")
	write("docs/notes.md", "`gt skipped`\n")

	refs, err := ScanRepo(root)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	var words []string
	for _, r := range refs {
		words = append(words, r.Words[0])
	}
	got := strings.Join(words, ",")
	if got != "twelve,thirteen,ten,seven,one,five,two,eight,three,four,six,nine" {
		t.Fatalf("ScanRepo words = %s", got)
	}
	for _, r := range refs {
		if filepath.IsAbs(r.File) {
			t.Errorf("Ref.File %q is absolute; want repo-relative", r.File)
		}
	}
}

func TestScanTOMLMarkdownShellAssignmentIsNotAKey(t *testing.T) {
	t.Parallel()
	// fail=0 inside a multiline string is shell, not a TOML key: it must not
	// reset the fence, or the closing ``` would open one over the prose after it.
	text := strings.Join([]string{
		`description = """`,
		"```bash",
		"fail=0; n=0",
		"gt prime",
		"```",
		"Log `Rogue bd check: clean` and move on.",
		`"""`,
		`title = "x"`,
		"Use `gt done`.",
	}, "\n")
	assertRefs(t, ScanTOMLMarkdown("f.toml", text), "4:gt prime", "9:gt done")
}
