package directives

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dir is the repo's directives/ directory, read from this package's place in
// the checkout: go test runs in the package directory, two levels down.
const dir = "../../directives"

// changelogLine is one entry of a directive's changelog: a date, a bead id and
// what changed (gt-dabdh).
var changelogLine = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} [a-z]+-[a-z0-9.]+: \S`)

// ruleHeading matches "## N. Title" for a numbered rule.
var ruleHeading = regexp.MustCompile(`^## (\d+)\. \S`)

func readDirective(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// changelogEntries returns the lines of the changelog block that opens the
// file, or fails when the file does not open with one.
func changelogEntries(t *testing.T, name, text string) []string {
	t.Helper()
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) || strings.TrimSpace(lines[i]) != "## Changelog" {
		t.Fatalf("%s does not open with a ## Changelog block", name)
	}
	var entries []string
	for _, l := range lines[i+1:] {
		if strings.TrimSpace(l) == "" {
			if len(entries) > 0 {
				break
			}
			continue
		}
		if !strings.HasPrefix(l, "- ") {
			break
		}
		entries = append(entries, l)
	}
	return entries
}

// Every directive file opens with a changelog whose lines each name a date, a
// bead id and a change (gt-dabdh).
func TestDirectiveFilesOpenWithACitedChangelog(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no directive files under %s", dir)
	}
	for _, f := range files {
		name := filepath.Base(f)
		entries := changelogEntries(t, name, readDirective(t, name))
		if len(entries) == 0 {
			t.Errorf("%s has an empty changelog", name)
		}
		for _, e := range entries {
			if !changelogLine.MatchString(e) {
				t.Errorf("%s changelog line lacks a date and a bead id: %q", name, e)
			}
		}
	}
}

// TestFindingInsteadOfCreate pins gt-wguxe: a failing test the diff does not
// touch is a defect met outside the task, so the polecat reports it with
// gt done --finding and the town files the Found bead in the owning rig. The
// polecat files nothing itself, so the directive names the finder and not a
// create.
func TestFindingInsteadOfCreate(t *testing.T) {
	t.Parallel()
	text := readDirective(t, "_common.md")
	t.Run("names gt done --finding", func(t *testing.T) {
		t.Parallel()
		const report = "`gt done --status DEFERRED --finding"
		if !strings.Contains(text, report) {
			t.Errorf("_common.md does not tell a polecat to report the failing test with %s...", report)
		}
	})
	t.Run("drops the Found bead create", func(t *testing.T) {
		t.Parallel()
		// Every wording the removed rule used, plus the command that files
		// the bead, so a rephrase that keeps the create still fails.
		for _, create := range []string{"`Found:` bead", "File a P1", "gt bead create"} {
			if strings.Contains(text, create) {
				t.Errorf("_common.md still tells a polecat to file a Found bead: %q", create)
			}
		}
	})
}

// ruleCountWords spells a rule count, so a test can hold the "What om rejects"
// intro's word to the rule headings it counts. Extend it before the directive
// outgrows it.
var ruleCountWords = []string{"", "One", "Two", "Three", "Four", "Five", "Six",
	"Seven", "Eight", "Nine", "Ten", "Eleven", "Twelve", "Thirteen", "Fourteen",
	"Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen", "Twenty"}

// polecat.md carries the "What om rejects" rules, numbered from 1 in order and
// each once, an intro stating their count, the rework rule's and rule 9's quoted
// findings, and no probe line. The changelog in polecat.md records how the set
// grew (gt-o87rv, gt-pccx7, gt-2iro9).
func TestPolecatDirectiveRuleHeadings(t *testing.T) {
	t.Parallel()
	const rules = 17
	text := readDirective(t, "polecat.md")
	var got []string
	for _, line := range strings.Split(text, "\n") {
		if m := ruleHeading.FindStringSubmatch(line); m != nil {
			got = append(got, m[1])
		}
	}
	var want []string
	for n := 1; n <= rules; n++ {
		want = append(want, fmt.Sprint(n))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("polecat.md rule headings = %v, want 1 to %d in order", got, rules)
	}
	intro := regexp.MustCompile(`(?m)^Read before you write code\. (\w+) rules\. `).FindStringSubmatch(text)
	if intro == nil || intro[1] != ruleCountWords[rules] {
		t.Errorf("polecat.md's intro does not open with %q rules; it carries %d numbered rule headings", ruleCountWords[rules], len(got))
	}
	for _, quoted := range []string{
		`- om flags: "still open"`,
		`"existing test fixture edited to fit new behaviour"`,
		`"exported API signature changed without recording it"`,
	} {
		if !strings.Contains(text, quoted) {
			t.Errorf("polecat.md no longer quotes the finding %q", quoted)
		}
	}
	if strings.Contains(text, "Probe word") {
		t.Error("polecat.md still carries the probe line")
	}
}

// TestPolecatDirectiveRule16HasItsBody pins gt-2iro9's review findings: the
// heading count passes with rule 16 empty or cut to a fragment, so this test
// reads the rule's own section and requires each clause the rule rests on, and
// the bulleted om flags line that names its findings.
func TestPolecatDirectiveRule16HasItsBody(t *testing.T) {
	t.Parallel()
	text := readDirective(t, "polecat.md")
	const heading = "\n## 16. One workspace per answer\n"
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatal("polecat.md has no rule 16 titled \"One workspace per answer\"")
	}
	body := text[start+len(heading):]
	end := strings.Index(body, "\n# Before gt done")
	if end < 0 {
		t.Fatal("polecat.md has no \"# Before gt done\" checklist after rule 16")
	}
	rule := body[:end]
	for _, clause := range []string{
		"checks that every record it reads names that workspace",
		"and with an empty workspace id",
		"A missing registry, prefix or map is an error",
		"\n- om flags: ",
	} {
		if !strings.Contains(rule, clause) {
			t.Errorf("rule 16 lacks the clause %q", clause)
		}
	}
}

// TestPolecatDirectiveRule17HasItsBody holds rule 17 to its body, as rule 16's
// test does: the heading count passes with the rule cut to a fragment, so this
// reads the rule's own section and requires the clauses it rests on, and the
// om flags line that names its findings.
func TestPolecatDirectiveRule17HasItsBody(t *testing.T) {
	t.Parallel()
	text := readDirective(t, "polecat.md")
	const heading = "\n## 17. Run the tier the gate skips\n"
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatal("polecat.md has no rule 17 titled \"Run the tier the gate skips\"")
	}
	body := text[start+len(heading):]
	end := strings.Index(body, "\n# Before gt done")
	if end < 0 {
		t.Fatal("polecat.md has no \"# Before gt done\" checklist after rule 17")
	}
	rule := body[:end]
	for _, clause := range []string{
		"tagged `//go:build integration`",
		"in your package or a caller's",
		"-tags integration",
		"\"not run: <reason>\"",
		"\n- om flags: ",
	} {
		if !strings.Contains(rule, clause) {
			t.Errorf("rule 17 lacks the clause %q", clause)
		}
	}
}

// TestPolecatDirectiveChecklistPointsAtRule17 pins gt-qgum2's rework: line 11
// of the "Before gt done" checklist is the checklist's caller of rule 17. It
// points at the rule instead of restating its command, and its search covers
// integration-tagged files in every package, as the rule's caller scope does.
func TestPolecatDirectiveChecklistPointsAtRule17(t *testing.T) {
	t.Parallel()
	text := readDirective(t, "polecat.md")
	start := strings.Index(text, "\n# Before gt done\n")
	if start < 0 {
		t.Fatal("polecat.md has no \"# Before gt done\" checklist")
	}
	var line11 string
	for _, l := range strings.Split(text[start:], "\n") {
		if strings.HasPrefix(l, "11. ") {
			line11 = l
			break
		}
	}
	if line11 == "" {
		t.Fatal("the \"Before gt done\" checklist has no line 11")
	}
	t.Run("points at rule 17 without restating its command", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(line11, "rule 17") {
			t.Errorf("checklist line 11 does not point at rule 17: %q", line11)
		}
		if strings.Contains(line11, "-tags integration") {
			t.Errorf("checklist line 11 restates rule 17's command instead of pointing at it: %q", line11)
		}
	})
	t.Run("searches every package, not the changed one", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(line11, "go:build integration") {
			t.Errorf("checklist line 11 does not search for integration-tagged files: %q", line11)
		}
		if strings.Contains(line11, "<pkg>") {
			t.Errorf("checklist line 11 searches only the changed package, not callers in other packages: %q", line11)
		}
	})
}
