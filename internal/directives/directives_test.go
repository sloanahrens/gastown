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

// polecat.md carries the sixteen "What om rejects" rules, numbered 1 to 16 in
// order and each once, and no probe line (gt-dabdh, gt-o87rv).
func TestPolecatDirectiveHasSixteenRules(t *testing.T) {
	t.Parallel()
	text := readDirective(t, "polecat.md")
	var got []string
	for _, line := range strings.Split(text, "\n") {
		if m := ruleHeading.FindStringSubmatch(line); m != nil {
			got = append(got, m[1])
		}
	}
	var want []string
	for n := 1; n <= 16; n++ {
		want = append(want, fmt.Sprint(n))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("polecat.md rule headings = %v, want 1 to 16 in order", got)
	}
	if strings.Contains(text, "Probe word") {
		t.Error("polecat.md still carries the probe line")
	}
}
