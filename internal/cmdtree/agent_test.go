package cmdtree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentProseBdAllowlist is the agent prose contract (deep review D1
// rule 6, gt-7iwy0.6): formulas, role and message templates, plugins, the
// repo's agent commands and skills, AGENTS.md, gt prime's output and the
// hints gt's commands print (gt-7iwy0.9) may name only the bd commands in
// AgentBdAllowed. Every mutation goes through a gt
// verb. Sites still waiting on a verb are counted in agent-bd-baseline.txt
// (gt-7iwy0.8), which may only shrink.
//
// Fix a failure by routing the instruction through a gt verb, never by
// raising a baseline count.
func TestAgentProseBdAllowlist(t *testing.T) {
	t.Parallel()
	bd, _, err := LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	refs, err := ScanAgentProse(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("ScanAgentProse: %v", err)
	}
	// Near half the 2026-10-01 count (772): a scanner gone blind passes.
	if len(refs) < 380 {
		t.Fatalf("ScanAgentProse found %d gt/bd invocations (floor 380); the scanner has stopped reading agent prose", len(refs))
	}
	src, err := os.ReadFile("agent-bd-baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := ParseAgentBdBaseline(string(src))
	if err != nil {
		t.Fatal(err)
	}
	fresh, stale := CompareAgentBdBaseline(CheckAgentBd(refs, bd), baseline, bd)
	for _, v := range fresh {
		t.Errorf("%s", v)
	}
	for _, s := range stale {
		t.Errorf("agent-bd-baseline.txt: %s", s)
	}
}

func testBdTree() *Tree {
	bd := NewTree()
	bd.Add([]string{"create"}, []string{"new"}, true)
	bd.Add([]string{"show"}, []string{"view"}, true)
	bd.Add([]string{"close"}, nil, true)
	bd.Add([]string{"list"}, nil, true)
	bd.Add([]string{"dep", "add"}, nil, true)
	return bd
}

func TestCheckAgentBd(t *testing.T) {
	t.Parallel()
	refs := []Ref{
		{File: "f", Line: 1, Bin: "bd", Words: []string{"show"}},
		{File: "f", Line: 2, Bin: "bd", Words: []string{"view"}},       // alias of show
		{File: "f", Line: 3, Bin: "bd", Words: []string{"new"}},        // alias of create
		{File: "f", Line: 4, Bin: "bd", Words: []string{"dep", "add"}}, // parent counts
		{File: "f", Line: 5, Bin: "gt", Words: []string{"close"}},      // gt is not checked
		{File: "f", Line: 6, Bin: "bd", Words: []string{"bogus"}},      // unknown: Check's job
		{File: "f", Line: 7, Bin: "bd", Words: []string{"close"}, Comment: true},
		{File: "f", Line: 8, Bin: "bd", Words: []string{"binary"}, Comment: true},
	}
	var got []string
	for _, v := range CheckAgentBd(refs, testBdTree()) {
		got = append(got, v.String())
	}
	want := []string{
		"f:3: bd new: " + agentBdReason,
		"f:4: bd dep add: " + agentBdReason,
		"f:7: bd close: " + agentBdReason,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CheckAgentBd:\n got\n%s\n want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCompareAgentBdBaseline(t *testing.T) {
	t.Parallel()
	bd := testBdTree()
	v := func(file string, line int, words ...string) Violation {
		return Violation{Ref: Ref{File: file, Line: line, Bin: "bd", Words: words}}
	}
	violations := []Violation{
		v("a.md", 1, "new"), v("a.md", 2, "create"), // a.md create: 2 vs baseline 1
		v("b.md", 3, "close"), // b.md close: 1 vs baseline 1
		v("c.md", 4, "close"), // c.md close: 1 vs baseline 3
	}
	baseline := map[AgentBdKey]int{
		{File: "a.md", Command: "create"}: 1,
		{File: "b.md", Command: "close"}:  1,
		{File: "c.md", Command: "close"}:  3,
		{File: "d.md", Command: "dep"}:    2,
	}
	fresh, stale := CompareAgentBdBaseline(violations, baseline, bd)
	if len(fresh) != 2 || fresh[0].Line != 1 || fresh[1].Line != 2 {
		t.Errorf("fresh = %v; want both a.md create sites", fresh)
	}
	want := []string{
		"c.md bd close: baseline 3, found 1; lower it to 1",
		"d.md bd dep: baseline 2, found none; remove the entry",
	}
	if strings.Join(stale, "\n") != strings.Join(want, "\n") {
		t.Errorf("stale:\n got\n%s\n want\n%s", strings.Join(stale, "\n"), strings.Join(want, "\n"))
	}
}

func TestParseAgentBdBaseline(t *testing.T) {
	t.Parallel()
	got, err := ParseAgentBdBaseline("# header\n\na.md bd create 2\nb.toml bd dep 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[AgentBdKey{"a.md", "create"}] != 2 || got[AgentBdKey{"b.toml", "dep"}] != 1 {
		t.Fatalf("ParseAgentBdBaseline = %v", got)
	}
	for _, bad := range []string{"a.md bd create", "a.md gt create 1", "a.md bd create 0", "a.md bd create 1\na.md bd create 2"} {
		if _, err := ParseAgentBdBaseline(bad); err == nil {
			t.Errorf("ParseAgentBdBaseline(%q) = nil error; want one", bad)
		}
	}
}

func TestScanGoStrings(t *testing.T) {
	t.Parallel()
	src := "package p\n" +
		"func f() {\n" +
		"\tprintln(\"- `bd close <issue>` - Mark issue complete\")\n" +
		"\tprintln(\"  1. Close the step: bd close %s\\n  2. Next: bd show x\")\n" +
		"\tprintln(\"| ~~bd complete~~ (not a command) |\")\n" +
		"}\n"
	refs, err := ScanGoStrings("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	assertRefs(t, refs, "3:bd close", "4:bd close", "4:bd show x")
}

func TestScanGoHints(t *testing.T) {
	t.Parallel()
	src := "package p\n" +
		"func f() {\n" +
		"\tfmt.Printf(\"  2. Cook to proto:  bd cook %s\\n\", x)\n" +
		"\tfmt.Fprintf(w, \"%s Warning: bd blocked failed for %s\\n\", icon, x)\n" +
		"\tfmt.Fprintln(os.Stderr, \"║  Running 'bd init' here would create an orphan\")\n" +
		"\tfmt.Fprintf(&b, \"more memories — see `bd kv list`.\\n\")\n" +
		"\tfmt.Println(\"  1. bd close \" + id)\n" +
		"\tfmt.Println(\"Next:\\n  $ bd update x\")\n" +
		"\treturn fmt.Errorf(\"bd config set %s: %w\", k, err)\n" +
		"\tmsg := fmt.Sprintf(\"bd history %s\", id)\n" +
		"\tif opts.DryRun {\n" +
		"\t\tfmt.Printf(\"Would run: bd update %s\\n\", id)\n" +
		"\t} else {\n" +
		"\t\tfmt.Println(\"Close it: bd close x\")\n" +
		"\t}\n" +
		"}\n" +
		"func dryRunFormula() { fmt.Println(\"  1. bd cook x\") }\n"
	refs, err := ScanGoHints("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	assertRefs(t, refs, "3:bd cook", "6:bd kv list", "7:bd close", "8:bd update x", "14:bd close x")
}

func TestAgentFacing(t *testing.T) {
	t.Parallel()
	for rel, want := range map[string]bool{
		"AGENTS.md": true,
		"internal/formula/formulas/mol-polecat-work.formula.toml": true,
		"internal/templates/roles/polecat.md.tmpl":                true,
		"plugins/tool-updater/run.sh":                             true,
		"plugins/tool-updater/run_test.sh":                        false,
		".claude/skills/pr-sheriff/skill.md":                      true,
		"internal/cmd/prime_output.go":                            true,
		"internal/cmd/prime_output_test.go":                       false,
		"internal/cmd/memory_index.go":                            true,
		"internal/cmd/sling.go":                                   true,
		"internal/cmd/sling_test.go":                              false,
		"internal/cmd/testdata/x.go":                              false,
		"scripts/land.sh":                                         false,
		"docs/reference.md":                                       false,
	} {
		if got := agentFacing(rel); got != want {
			t.Errorf("agentFacing(%q) = %v; want %v", rel, got, want)
		}
	}
}
