package bdgateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logOne writes one record through a call log in a temp dir and returns the
// dir, the line as decoded and the file's raw bytes, so a test reads what the
// writer wrote.
func logOne(t *testing.T, rec CallRecord) (string, map[string]any, string) {
	t.Helper()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	l.Log(rec)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(opts.Dir, DefaultCallLogName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return opts.Dir, decodeLine(t, readLines(t, path)[0]), string(raw)
}

// TestCallLogRefusedFlag: an ambiguous-flag refusal names the flag it refused
// without its value, bounded, and no other call carries the key.
func TestCallLogRefusedFlag(t *testing.T) {
	t.Parallel()
	longArg := "--" + strings.Repeat("a", 100)
	cases := []struct {
		name    string
		argv    []string
		exit    int
		refusal string
		want    string // the refused_flag value; empty means the key is absent
	}{
		{"a flag before any command word", []string{"--version"}, ExitRefused, "forbidden", "--version"},
		{"a flag whose next token a flag-taking reading would swallow", []string{"show", "--thread", "--db"}, ExitRefused, "forbidden", "--thread"},
		{"a name past the field bound is truncated", []string{longArg, "show"}, ExitRefused, "forbidden", longArg[:maxFieldBytes]},
		{"an attached value stays off the line", []string{"-x=secret", "show"}, ExitRefused, "forbidden", "-x"},
		{"a separate operand stays off the line", []string{"--foo", "secret", "show"}, ExitRefused, "forbidden", "--foo"},
		{"a clustered shorthand names no single flag", []string{"-Zsecret", "show"}, ExitRefused, "forbidden", ""},
		{"a token with no placeable name", []string{"--123", "show"}, ExitRefused, "forbidden", ""},
		{"an attached value is never ambiguous", []string{"--token=secret", "show"}, 0, "", ""},
		{"a verb the policy forbids", []string{"dolt", "push"}, ExitRefused, "forbidden", ""},
		{"an authentication refusal", []string{"show", "gt-abc"}, ExitRefused, "unauthorized", ""},
		{"an authorized call", []string{"show", "gt-abc"}, 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, got, raw := logOne(t, CallRecord{Class: "daemon", Caller: "gt-wrap", Argv: tc.argv, ExitCode: tc.exit, Refusal: tc.refusal})
			flag, has := got["refused_flag"]
			if tc.want == "" && has {
				t.Errorf("argv %q carries refused_flag %v, want no key", tc.argv, flag)
			}
			if tc.want != "" && (flag != tc.want || got["verb"] != unparsedVerb || got["exit_code"] != float64(ExitRefused)) {
				t.Errorf("argv %q line = %v, want verb %q, exit %d and refused_flag %s", tc.argv, got, unparsedVerb, ExitRefused, tc.want)
			}
			if strings.Contains(raw, "secret") {
				t.Errorf("argv %q line holds its flag value: %s", tc.argv, raw)
			}
		})
	}
}

// TestCallLogRefusedFlagNameDropsValue: a token names its flag's spelling, never
// an attached value; the rows above cover the forms a real call reaches.
func TestCallLogRefusedFlagNameDropsValue(t *testing.T) {
	t.Parallel()
	for arg, want := range map[string]string{"--token=secret": "--token", "-x=secret": "-x"} {
		if got := refusedFlagName(arg); got != want {
			t.Errorf("refusedFlagName(%q) = %q, want %q", arg, got, want)
		}
	}
}

// TestCallLogRefusedFlagServed: the field the log writes reaches GET /v1/calls
// through CallEntry.
func TestCallLogRefusedFlagServed(t *testing.T) {
	t.Parallel()
	dir, _, _ := logOne(t, CallRecord{Class: "daemon", Caller: "gt-wrap", Argv: []string{"--version"}, ExitCode: ExitRefused, Refusal: "forbidden"})
	entries, err := ReadCalls(dir, fixedNow().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ReadCalls: %v", err)
	}
	if len(entries) != 1 || entries[0].RefusedFlag != "--version" {
		t.Errorf("entries = %+v, want one with refused_flag --version", entries)
	}
}
