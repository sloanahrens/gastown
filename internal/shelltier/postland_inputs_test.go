package shelltier

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// checkPostLandInputs returns the problems the post-land skip rule pattern has
// against entries: one per manifest trigger the rule does not match, naming the
// trigger, and one per path the rule matches that no trigger covers. The
// manifest is the truth the post-land revert already reads (land.ShellTierInputs),
// so a landing the revert can blame for a shell failure must never be skipped
// by this filter (gt-vt2g4; the rule the previous drift test held, gt-er6jn).
//
// The rule is refused when it is not a POSIX ERE: grep -E reads that syntax,
// and RE2's Perl extensions are not POSIX ERE. GNU grep reads \d as a literal d
// and treats (?i) as a stray ?, so certifying either with RE2 could pass a rule
// the filter reads differently; both are refused instead (gt-r2u9g).
//
// The over-match side tries topLevel, the caller's sample of paths outside the
// triggers, and a near-miss path beside and below every trigger. A fixed pair
// of paths let a rule that also matched templates/ or scriptsx/ pass
// (gt-r2u9g).
func checkPostLandInputs(entries []Entry, pattern string, topLevel []string) []string {
	re, err := regexp.CompilePOSIX(pattern)
	if err != nil {
		return []string{fmt.Sprintf("the INPUTS rule %q is not a POSIX ERE: %v", pattern, err)}
	}
	var triggers []string
	seen := make(map[string]bool)
	for _, e := range entries {
		for _, trigger := range e.TriggersOrDefault() {
			if seen[trigger] {
				continue
			}
			seen[trigger] = true
			triggers = append(triggers, trigger)
		}
	}
	var out []string
	for _, trigger := range triggers {
		if !re.MatchString(triggerSample(trigger)) {
			out = append(out, fmt.Sprintf("the INPUTS rule %q does not match the manifest trigger %q", pattern, trigger))
		}
	}
	samples := append(append([]string{}, topLevel...), triggerNearMisses(triggers)...)
	for _, path := range samples {
		if tiertrigger.Runs(triggers, []string{path}, false) {
			continue // a trigger covers it, so matching it is the union's job
		}
		if re.MatchString(path) {
			out = append(out, fmt.Sprintf("the INPUTS rule %q matches %q, which no manifest trigger covers", pattern, path))
		}
	}
	return out
}

// triggerSample is a repo path whose change the trigger says runs the tier:
// the trigger itself when it names a file, and a path under it when it names a
// directory.
func triggerSample(trigger string) string {
	if strings.HasSuffix(trigger, "/") {
		return trigger + "changed.txt"
	}
	return trigger
}

// repoTopLevelPaths returns one repo path under every top-level entry of the
// tree this test runs in: a directory as "name/changed.txt", a file as its
// name. The over-match check tries them, so a rule that names a directory no
// trigger covers is caught when that directory exists rather than only when it
// happens to be one of a fixed pair of paths (gt-r2u9g).
func repoTopLevelPaths(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/changed.txt"
		}
		out = append(out, name)
	}
	return out
}

// triggerNearMisses returns paths that share a trigger's spelling without being
// covered by it: a sibling the directory trigger's separator keeps apart
// (scripts/ -> scriptsx/changed.txt), a sibling the file trigger's end anchor
// keeps apart (Makefile -> Makefile.bak), and the trigger carried below the
// repo root (scripts/ -> sub/scripts/changed.txt). tiertrigger.Runs covers none
// of them, so a rule that matches one matches a path no trigger covers.
func triggerNearMisses(triggers []string) []string {
	var out []string
	for _, trigger := range triggers {
		if dir, ok := strings.CutSuffix(trigger, "/"); ok {
			out = append(out, dir+"x/changed.txt", "sub/"+trigger+"changed.txt")
			continue
		}
		out = append(out, trigger+".bak", "sub/"+trigger)
	}
	return out
}

// TestPostLandShellInputsCoverTheManifestTriggers: the post-land skip filter is
// the manifest's trigger union, so a trigger added there cannot leave the
// filter skipping the tier it should run. Dropping deploy/ from postLandInputs
// turns this red and names the trigger (gt-vt2g4).
func TestPostLandShellInputsCoverTheManifestTriggers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		entries  []Entry
		pattern  string
		topLevel []string
		want     string
	}{
		{
			name:    "a rule matching every trigger passes",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			pattern: `^(scripts/|Makefile$)`,
		},
		{
			name:    "a trigger the rule does not match is named",
			entries: []Entry{{Path: "scripts/a_test.sh", Triggers: []string{"deploy/"}, StubsGo: "none: names no go command"}},
			pattern: `^(scripts/|Makefile$)`,
			want:    `does not match the manifest trigger "deploy/"`,
		},
		{
			name:    "a Go trigger an entry without a stub exemption contributes is named",
			entries: []Entry{{Path: "scripts/a_test.sh"}},
			pattern: `^(scripts/|Makefile$)`,
			want:    `does not match the manifest trigger "internal/"`,
		},
		{
			name:     "a rule that matches a path no trigger covers is named",
			entries:  []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			pattern:  `.*`,
			topLevel: []string{"docs/reference.md", "tracker/api/actor.go"},
			want:     `matches "docs/reference.md"`,
		},
		{
			name:    "a rule that drops a directory trigger's separator over-matches its sibling",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			pattern: `^(scripts|Makefile$)`,
			want:    `matches "scriptsx/changed.txt"`,
		},
		{
			name:    "a rule with no start anchor over-matches the trigger below the repo root",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			pattern: `scripts/|Makefile`,
			want:    `matches "sub/scripts/changed.txt"`,
		},
		{
			name:    "an INPUTS value that is not a POSIX ERE is a problem",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			pattern: `^(scripts/`,
			want:    "not a POSIX ERE",
		},
		{
			name:    "an RE2 Perl class is refused instead of read as a literal",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			pattern: `^\d`,
			want:    "not a POSIX ERE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkPostLandInputs(tc.entries, tc.pattern, tc.topLevel)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("checkPostLandInputs() = %q, want no problem", got)
				}
				return
			}
			for _, p := range got {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("checkPostLandInputs() = %q, want a problem naming %q", got, tc.want)
		})
	}
	t.Run("the manifest", func(t *testing.T) {
		t.Parallel()
		for _, p := range checkPostLandInputs(Manifest(), postLandInputs, repoTopLevelPaths(t)) {
			t.Error(p)
		}
	})
}
