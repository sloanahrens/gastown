package shelltier

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// postLandScript is the post-land check (merge_queue.post_land_command). Its
// INPUTS line is the regex the skip filter applies to main's changed paths.
const postLandScript = "scripts/post-land-shell.sh"

// postLandInputs returns the ERE the script's INPUTS line holds. A script with
// no INPUTS line, an unterminated value or a value that is not an ERE has no
// rule the guard can read, so it is an error: a filter nothing can parse is
// not one that skips with evidence.
func postLandInputs(script string) (string, error) {
	for _, line := range strings.Split(script, "\n") {
		rest, ok := strings.CutPrefix(line, "INPUTS='")
		if !ok {
			continue
		}
		if !strings.HasSuffix(rest, "'") {
			return "", fmt.Errorf("the INPUTS line is not closed with a quote: %s", line)
		}
		pattern := strings.TrimSuffix(rest, "'")
		if _, err := regexp.Compile(pattern); err != nil {
			return "", fmt.Errorf("the INPUTS line is not an ERE: %w", err)
		}
		return pattern, nil
	}
	return "", fmt.Errorf("no INPUTS='...' line, so the skip filter has no rule to check")
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

// checkPostLandInputs returns the problems the script's INPUTS rule has
// against entries: one per manifest trigger the rule does not match, naming the
// trigger, and one per path the rule matches that no trigger covers. The
// manifest is the truth the post-land revert already reads (land.ShellTierInputs),
// so a landing the revert can blame for a shell failure must never be skipped
// by this filter (gt-vt2g4; the rule the previous drift test held, gt-er6jn).
func checkPostLandInputs(entries []Entry, script string) []string {
	pattern, err := postLandInputs(script)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", postLandScript, err)}
	}
	re := regexp.MustCompile(pattern)
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
	for _, path := range []string{"docs/reference.md", "tracker/api/actor.go"} {
		if tiertrigger.Runs(triggers, []string{path}, false) {
			continue // a trigger covers it, so matching it is the union's job
		}
		if re.MatchString(path) {
			out = append(out, fmt.Sprintf("the INPUTS rule %q matches %q, which no manifest trigger covers", pattern, path))
		}
	}
	return out
}

// TestPostLandShellInputsCoverTheManifestTriggers: the post-land skip filter is
// the manifest's trigger union, so a trigger added there cannot leave the
// filter skipping the tier it should run. Dropping deploy/ from the INPUTS
// line turns this red and names the trigger (gt-vt2g4).
func TestPostLandShellInputsCoverTheManifestTriggers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []Entry
		script  string
		want    string
	}{
		{
			name:    "a rule matching every trigger passes",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			script:  "INPUTS='^(scripts/|Makefile$)'\n",
		},
		{
			name:    "a trigger the rule does not match is named",
			entries: []Entry{{Path: "scripts/a_test.sh", Triggers: []string{"deploy/"}, StubsGo: "none: names no go command"}},
			script:  "INPUTS='^(scripts/|Makefile$)'\n",
			want:    `does not match the manifest trigger "deploy/"`,
		},
		{
			name:    "a Go trigger an entry without a stub exemption contributes is named",
			entries: []Entry{{Path: "scripts/a_test.sh"}},
			script:  "INPUTS='^(scripts/|Makefile$)'\n",
			want:    `does not match the manifest trigger "internal/"`,
		},
		{
			name:    "a rule that matches a path no trigger covers is named",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			script:  "INPUTS='.*'\n",
			want:    `matches "docs/reference.md"`,
		},
		{
			name:    "a script with no INPUTS line is a problem",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			script:  "set -uo pipefail\n",
			want:    "no INPUTS",
		},
		{
			name:    "an unterminated INPUTS value is a problem",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			script:  "INPUTS='^(scripts/)\n",
			want:    "not closed with a quote",
		},
		{
			name:    "an INPUTS value that is not an ERE is a problem",
			entries: []Entry{{Path: "scripts/a_test.sh", StubsGo: "none: names no go command"}},
			script:  "INPUTS='^(scripts/'\n",
			want:    "not an ERE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkPostLandInputs(tc.entries, tc.script)
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
		script, err := repoFiles(t)(postLandScript)
		if err != nil {
			t.Fatalf("read %s: %v", postLandScript, err)
		}
		for _, p := range checkPostLandInputs(Manifest(), script) {
			t.Error(p)
		}
	})
}
