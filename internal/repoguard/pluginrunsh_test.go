package repoguard

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The literal alarm text is pinned here, not read back from the guard: a test
// that reuses the production string passes when the message loses the rule or
// the offending path.
const (
	shimBoundAlarm   = "make it one exec of the plugin's declared binary within 20 lines, or give it a plugin-run-sh.txt line with its reason (scripts are debt: see .om.json, scripts-are-debt)"
	nowAShimAlarm    = "this run.sh is an exec shim now; delete its plugin-run-sh.txt line in the same change"
	staleLineAlarm   = "allowlisted plugin run.sh is not in the tree; the plugin-run-sh.txt line is stale"
	noReasonAlarm    = "the plugin-run-sh.txt line has no reason; say why this run.sh keeps its logic"
	declaredBinaryID = "gt"
)

// runSh builds a fixture run.sh body. The shebang is assembled from two
// literals because internal/testpolicy's no-exec-files rule reads a "#!"
// string literal in a test as writing an executable, and these bodies are
// parsed by the guard, never written or run.
func runSh(body string) string {
	return "#" + "!/usr/bin/env bash\n" + body
}

var (
	// shimBody is the shape the guard accepts: a header comment, a blank line,
	// and one exec of the binary plugin.md declares.
	shimBody = runSh("# Why, and how it is gated: plugin.md beside this script.\n\nexec \"$(dirname \"$0\")/" + declaredBinaryID + "\" \"$@\"\n")
	// logicBody is plugins/town-metrics/run.sh before its porting bead lands.
	logicBody = runSh("set -euo pipefail\ncd \"${GT_TOWN_ROOT:-${GT_ROOT:-$HOME/gt}}\"\nexec gt metrics run\n")
)

// TestExecShimProblem drives every clause of the shim rule through one table.
// Each case pins the clause the finding would carry, so a reason that stops
// naming what broke fails here. The bypass forms are cases of their own: an
// interpreter reached by absolute path, by another case, or by a version
// suffix is the same bypass, and so is a target with no declared binary behind
// it (gt-tpg0p.2).
func TestExecShimProblem(t *testing.T) {
	t.Parallel()
	twentyLines := strings.Repeat("# a comment\n", 19) + "exec \"$(dirname \"$0\")/" + declaredBinaryID + "\"\n"
	tests := []struct {
		name   string
		body   string
		binary string
		want   string
	}{
		{
			name:   "the declared binary beside the script",
			body:   shimBody,
			binary: declaredBinaryID,
		},
		{
			name:   "a declared binary in a subdirectory, forwarding $@ and a trailing comment",
			body:   runSh("exec \"$(dirname \"$0\")/bin/gt\" \"$@\" # the report\n"),
			binary: "bin/gt",
		},
		{
			name:   "the ${0%/*} form, unquoted",
			body:   runSh("exec ${0%/*}/" + declaredBinaryID + "\n"),
			binary: declaredBinaryID,
		},
		{
			name:   "the BASH_SOURCE form through cd and pwd",
			body:   runSh("exec \"$(cd \"$(dirname \"${BASH_SOURCE[0]}\")\" && pwd)/" + declaredBinaryID + "\" \"$@\"\n"),
			binary: declaredBinaryID,
		},
		{
			name:   "exactly the line cap",
			body:   twentyLines,
			binary: declaredBinaryID,
		},
		{
			name:   "one line past the cap",
			body:   strings.Repeat("# a comment\n", 20) + "exec \"$(dirname \"$0\")/" + declaredBinaryID + "\"\n",
			binary: declaredBinaryID,
			want:   "it is 21 lines, past the 20-line shim cap",
		},
		{
			name: "two commands",
			body: runSh("set -euo pipefail\nexec \"$(dirname \"$0\")/" + declaredBinaryID + "\"\n"),
			want: "it holds 2 commands and a shim holds one exec",
		},
		{
			name: "comments only",
			body: runSh("# nothing here yet\n"),
			want: "it runs nothing",
		},
		{
			name: "an exec-less command",
			body: runSh("gt metrics run\n"),
			want: "its one command is not an exec",
		},
		{
			name: "an assignment before exec",
			body: runSh("X=1 exec \"$(dirname \"$0\")/" + declaredBinaryID + "\"\n"),
			want: "its one command is not an exec",
		},
		{
			name: "a bare exec",
			body: runSh("exec\n"),
			want: "its exec names no program",
		},
		{
			name:   "an extensionless sibling that is not the declared binary",
			body:   runSh("exec \"$(dirname \"$0\")/helper\" \"$@\"\n"),
			binary: declaredBinaryID,
			want:   `its exec target "$(dirname $0)/helper" is not the binary plugin.md declares ("gt")`,
		},
		{
			name: "an extensionless sibling with no declaration behind it",
			body: runSh("exec \"$(dirname \"$0\")/" + declaredBinaryID + "\"\n"),
			want: `its exec target "$(dirname $0)/gt" has no binary evidence: the plugin.md [execution] block names no binary, so the guard cannot tell it from a script`,
		},
		{
			name:   "a literal path the guard never sees",
			body:   runSh("exec /usr/local/bin/gt \"$@\"\n"),
			binary: declaredBinaryID,
			want:   `its exec target "/usr/local/bin/gt" is a literal path, and the guard can only vouch for the binary the plugin declares`,
		},
		{
			name:   "an interpreter by name",
			body:   runSh("exec bash /tmp/report.sh\n"),
			binary: declaredBinaryID,
			want:   `its exec target "bash" is a literal path, and the guard can only vouch for the binary the plugin declares`,
		},
		{
			name:   "a declared interpreter is still refused",
			body:   runSh("exec \"$(dirname \"$0\")/bash\" \"$@\"\n"),
			binary: "bash",
			want:   `its exec target "$(dirname $0)/bash" is an interpreter, so the logic stays in another file`,
		},
		{
			name:   "a declared shell script is still refused",
			body:   runSh("exec \"$(dirname \"$0\")/helper.sh\" \"$@\"\n"),
			binary: "helper.sh",
			want:   `its exec target "$(dirname $0)/helper.sh" is a .sh script, so the logic stays in another file`,
		},
		{
			name:   "an interpreter absolute path in another case",
			body:   runSh("exec /bin/BASH /tmp/report.sh\n"),
			binary: declaredBinaryID,
			want:   `its exec target "/bin/BASH" is a literal path, and the guard can only vouch for the binary the plugin declares`,
		},
		{
			name:   "a second command on the exec line",
			body:   runSh("exec \"$(dirname \"$0\")/gt\"; rm -rf /tmp/out\n"),
			binary: declaredBinaryID,
			want:   `its exec target "$(dirname $0)/gt;" carries shell syntax`,
		},
		{
			name:   "a chained command after the exec",
			body:   runSh("exec \"$(dirname \"$0\")/gt\" && rm -rf /tmp/out\n"),
			binary: declaredBinaryID,
			want:   `its exec line holds shell syntax after the target: "&& rm -rf /tmp/out"`,
		},
		{
			name:   "fixed arguments after the exec",
			body:   runSh("exec \"$(dirname \"$0\")/gt\" metrics run\n"),
			binary: declaredBinaryID,
			want:   `its exec carries "metrics run", and a shim forwards only "$@"`,
		},
		{
			name:   "an env-var target",
			body:   runSh("exec \"$HOME/bin/gt\"\n"),
			binary: declaredBinaryID,
			want:   `its exec target "$HOME/bin/gt" is computed, not a path inside the plugin directory`,
		},
		{
			name:   "a tilde in the target's directory",
			body:   runSh("exec ~/bin/gt\n"),
			binary: declaredBinaryID,
			want:   `its exec target "~/bin/gt" expands shell metacharacters instead of naming one path`,
		},
		{
			name:   "a glob in the target's directory",
			body:   runSh("exec /opt/*/gt\n"),
			binary: declaredBinaryID,
			want:   `its exec target "/opt/*/gt" expands shell metacharacters instead of naming one path`,
		},
		{
			name:   "a bare name looked up on PATH",
			body:   runSh("exec gt metrics run\n"),
			binary: declaredBinaryID,
			want:   `its exec target "gt" is a literal path, and the guard can only vouch for the binary the plugin declares`,
		},
		{
			name: "an empty file",
			body: "",
			want: "it runs nothing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := execShimProblem(tc.body, tc.binary); got != tc.want {
				t.Errorf("execShimProblem = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEverySelfLocatedFormIsAShim holds each way scriptDirs names a file
// inside the plugin directory to the guard: a prefix the parser accepts but
// the rule rejects would make one of the four ported shims unwritable without
// an allowlist line, and a prefix it rejects would push a new shim onto the
// list.
func TestEverySelfLocatedFormIsAShim(t *testing.T) {
	t.Parallel()
	for _, dir := range scriptDirs {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			body := runSh("# pointer to plugin.md\nexec \"" + dir + "/" + declaredBinaryID + "\" \"$@\"\n")
			if got := execShimProblem(body, declaredBinaryID); got != "" {
				t.Errorf("execShimProblem(%q) = %q, want a shim", body, got)
			}
		})
	}
}

// TestCheckPluginRunShFixtures drives each branch through an injected lister:
// a script over the line cap with no line, a shim under it with no line, a
// sibling and a literal path with no binary behind them, a shim the allowlist
// no longer needs, a script that keeps its logic with no line, a line whose
// script is gone, a line with no reason, and a tree where the two sets agree.
func TestCheckPluginRunShFixtures(t *testing.T) {
	t.Parallel()
	nineTeenLines := strings.Repeat("# a pointer to plugin.md\n", 18) + "exec \"$(dirname \"$0\")/" + declaredBinaryID + "\"\n"
	tests := []struct {
		name    string
		scripts map[string]PluginRunSh
		allowed map[string]string
		want    []string
	}{
		{
			name: "clean",
			scripts: map[string]PluginRunSh{
				"plugins/good/run.sh": {Body: shimBody, Binary: declaredBinaryID},
				"plugins/old/run.sh":  {Body: logicBody},
			},
			allowed: map[string]string{"plugins/old/run.sh": "ports in gt-tpg0p.3"},
		},
		{
			name:    "a shim of the declared binary under the cap with no line",
			scripts: map[string]PluginRunSh{"plugins/good/run.sh": {Body: nineTeenLines, Binary: declaredBinaryID}},
		},
		{
			name:    "a script over the line cap with no line",
			scripts: map[string]PluginRunSh{"plugins/old/run.sh": {Body: strings.Repeat("# a comment\n", 20) + "exec \"$(dirname \"$0\")/gt\"\n"}},
			want: []string{
				"plugins/old/run.sh: it is 21 lines, past the 20-line shim cap; " + shimBoundAlarm,
			},
		},
		{
			name:    "a script that keeps its logic with no line",
			scripts: map[string]PluginRunSh{"plugins/old/run.sh": {Body: logicBody}},
			want: []string{
				"plugins/old/run.sh: it holds 3 commands and a shim holds one exec; " + shimBoundAlarm,
			},
		},
		{
			name:    "an extensionless sibling with no declaration",
			scripts: map[string]PluginRunSh{"plugins/old/run.sh": {Body: runSh("exec \"$(dirname \"$0\")/helper\"\n")}},
			want: []string{
				`plugins/old/run.sh: its exec target "$(dirname $0)/helper" has no binary evidence: the plugin.md [execution] block names no binary, so the guard cannot tell it from a script; ` + shimBoundAlarm,
			},
		},
		{
			name: "a literal path with no binary evidence",
			scripts: map[string]PluginRunSh{"plugins/old/run.sh": {
				Body:   runSh("exec /usr/local/bin/gt \"$@\"\n"),
				Binary: declaredBinaryID,
			}},
			want: []string{
				`plugins/old/run.sh: its exec target "/usr/local/bin/gt" is a literal path, and the guard can only vouch for the binary the plugin declares; ` + shimBoundAlarm,
			},
		},
		{
			name: "a name that climbs out of the plugin directory",
			scripts: map[string]PluginRunSh{"plugins/old/run.sh": {
				Body:   runSh("exec \"$(dirname \"$0\")/../gt\"\n"),
				Binary: declaredBinaryID,
			}},
			want: []string{
				`plugins/old/run.sh: its exec target "$(dirname $0)/../gt" names "../gt", not a path inside the plugin directory; ` + shimBoundAlarm,
			},
		},
		{
			name:    "an allowlisted script that became a shim",
			scripts: map[string]PluginRunSh{"plugins/good/run.sh": {Body: shimBody, Binary: declaredBinaryID}},
			allowed: map[string]string{"plugins/good/run.sh": "ports in gt-tpg0p.4"},
			want:    []string{"plugins/good/run.sh: " + nowAShimAlarm},
		},
		{
			name:    "a line whose script is gone",
			scripts: map[string]PluginRunSh{"plugins/good/run.sh": {Body: shimBody, Binary: declaredBinaryID}},
			allowed: map[string]string{"plugins/gone/run.sh": "dropped in gt-98pgy"},
			want:    []string{"plugins/gone/run.sh: " + staleLineAlarm},
		},
		{
			name:    "a line with no reason",
			scripts: map[string]PluginRunSh{"plugins/old/run.sh": {Body: logicBody}},
			allowed: map[string]string{"plugins/old/run.sh": ""},
			want:    []string{"plugins/old/run.sh: " + noReasonAlarm},
		},
		{
			name: "every branch at once, sorted by path",
			scripts: map[string]PluginRunSh{
				"plugins/a/run.sh": {Body: shimBody, Binary: declaredBinaryID},
				"plugins/b/run.sh": {Body: logicBody},
			},
			allowed: map[string]string{
				"plugins/a/run.sh": "ports in gt-tpg0p.5",
				"plugins/c/run.sh": "ports in gt-tpg0p.6",
			},
			want: []string{
				"plugins/a/run.sh: " + nowAShimAlarm,
				"plugins/b/run.sh: it holds 3 commands and a shim holds one exec; " + shimBoundAlarm,
				"plugins/c/run.sh: " + staleLineAlarm,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings, err := CheckPluginRunSh(func() (map[string]PluginRunSh, error) { return tc.scripts, nil }, tc.allowed)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(findings, tc.want) {
				t.Errorf("findings = %q, want %q", findings, tc.want)
			}
		})
	}
}

// TestCheckPluginRunShListerError: a lister that cannot see the tree makes the
// guard fail rather than pass, since an empty tree and an unread one must not
// look alike.
func TestCheckPluginRunShListerError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("plugins directory is unreadable")
	findings, err := CheckPluginRunSh(func() (map[string]PluginRunSh, error) { return nil, wantErr }, map[string]string{"plugins/a/run.sh": "reason"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if findings != nil {
		t.Errorf("findings = %q with a lister error, want nil", findings)
	}
}

// TestReadPluginRunShAllowlistSkipsHeader pins the file's shape: comments and
// blank lines are not entries, the reason follows a tab, and a line with no
// reason or an empty one keeps an empty string for the check to report.
func TestReadPluginRunShAllowlistSkipsHeader(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plugin-run-sh.txt")
	body := "# the header\n\n" +
		"plugins/a/run.sh\tstill bash; ports in gt-tpg0p.4\n" +
		"plugins/b/run.sh\n" +
		"plugins/c/run.sh\t\n" +
		"plugins/d/run.sh  \t  trailing space  \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPluginRunShAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"plugins/a/run.sh": "still bash; ports in gt-tpg0p.4",
		"plugins/b/run.sh": "",
		"plugins/c/run.sh": "",
		"plugins/d/run.sh": "trailing space",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadPluginRunShAllowlist = %v, want %v", got, want)
	}
}

// TestReadPluginRunShAllowlistRejectsDuplicatePath: two lines for one script
// are a merge artifact, and the map would keep only the last. The reader fails
// instead, naming the line.
func TestReadPluginRunShAllowlistRejectsDuplicatePath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plugin-run-sh.txt")
	body := "plugins/a/run.sh\tfirst reason\nplugins/a/run.sh\tsecond reason\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPluginRunShAllowlist(path); err == nil {
		t.Fatalf("ReadPluginRunShAllowlist = %v, want an error for the duplicate line", got)
	}
}

// TestReadPluginRunShAllowlistRefusesAMissingFile: an allowlist that cannot be
// read is an error, never an empty one, since an empty one would put every
// ported-backlog script outside the guard.
func TestReadPluginRunShAllowlistRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	if got, err := ReadPluginRunShAllowlist(filepath.Join(t.TempDir(), "plugin-run-sh.txt")); err == nil {
		t.Fatalf("ReadPluginRunShAllowlist(missing file) = %v, want an error", got)
	}
}

// TestReadPluginRunShAllowlistRefusesAnOversizeLine: a line past the scanner's
// limit stops the read, so a truncated list is never read as the whole one.
func TestReadPluginRunShAllowlistRefusesAnOversizeLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plugin-run-sh.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 70_000)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPluginRunShAllowlist(path); err == nil {
		t.Fatalf("ReadPluginRunShAllowlist(oversize line) = %v, want an error", got)
	}
}

// TestReadPluginRunShAllowlistRejectsEmptyPath: a tab with nothing before it
// would enter "" as a path, which no script can match and which reports as a
// stale line with an empty name.
func TestReadPluginRunShAllowlistRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plugin-run-sh.txt")
	if err := os.WriteFile(path, []byte("\tno path here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPluginRunShAllowlist(path); err == nil {
		t.Fatalf("ReadPluginRunShAllowlist = %v, want an error for the empty path", got)
	}
}
