package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The probe answers these tests feed the program, shaped as `brew outdated
// --json=v2` writes them.
const (
	outdatedBoth  = `{"formulae":[{"name":"dolt","installed_versions":["2.3.2"],"current_version":"2.4.1"},{"name":"beads","installed_versions":["1.2.2"],"current_version":"1.3.1"}]}`
	outdatedBeads = `{"formulae":[{"name":"beads","installed_versions":["1.2.2"],"current_version":"1.3.1"}]}`
	allCurrent    = `{"formulae":[]}`
)

// reply is what a scripted command answers.
type reply struct {
	out string
	err error
}

// call is one command the program ran, as the fake recorded it.
type call struct {
	name     string
	extraEnv []string
	combined bool
	args     []string
}

func (c call) String() string { return strings.Join(append([]string{c.name}, c.args...), " ") }

// fake is the program's outside world: PATH lookups and commands, both
// scripted, so a test drives every branch without brew, a subprocess or the
// host's PATH.
type fake struct {
	missing string           // the program name a lookup fails for
	replies map[string]reply // answers keyed by call.String()
	calls   []call
}

func (f *fake) lookup(name string) (string, error) {
	if name == f.missing {
		return "", errors.New("not found")
	}
	return "/stub/bin/" + name, nil
}

func (f *fake) run(name string, extraEnv []string, combined bool, args ...string) (string, error) {
	c := call{name: name, extraEnv: extraEnv, combined: combined, args: args}
	f.calls = append(f.calls, c)
	r := f.replies[c.String()]
	return r.out, r.err
}

// commands lists what ran, in order.
func (f *fake) commands() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.String()
	}
	return out
}

// report runs the program against f and returns its exit code and stdout.
func report(t *testing.T, f *fake) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := run(env{out: &out, look: f.lookup, cmd: f.run})
	return code, out.String()
}

// checkOutput asserts what the report said and what it never said.
func checkOutput(t *testing.T, got string, want, absent []string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("output is missing %q; got:\n%s", s, got)
		}
	}
	for _, s := range absent {
		if strings.Contains(got, s) {
			t.Errorf("output holds %q, want it absent; got:\n%s", s, got)
		}
	}
}

// TestReport_Outdated pins the whole report and the commands it runs: one
// line per outdated tool, the actionable tool's next step, the summary, and a
// receipt — with no brew upgrade or install among them.
func TestReport_Outdated(t *testing.T) {
	t.Parallel()
	const title = "tool-updater: outdated=2 (report-only)"
	f := &fake{replies: map[string]reply{
		"brew update":             {out: "Already up-to-date.\n"},
		"brew outdated --json=v2": {out: outdatedBoth},
		"gt plugin record-run --plugin tool-updater --result success --title " + title: {},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	want := "[tool-updater] Running brew update...\n" +
		"Already up-to-date.\n" +
		"[tool-updater] Checking for updates...\n" +
		"[tool-updater]   dolt: update available (installed: 2.3.2, available: 2.4.1)\n" +
		"[tool-updater]     to act, run by hand: brew upgrade dolt\n" +
		"[tool-updater]   beads: update available (installed: 1.2.2, available: 1.3.1)\n" +
		// The empty report line keeps the space the prefix ends with.
		"[tool-updater] \n" +
		"[tool-updater] === Done === " + title + "\n" +
		"[tool-updater] Upgrades are deliberate and manual (gt-th5it).\n"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	wantCalls := []string{
		"brew update",
		"brew outdated --json=v2",
		"gt plugin record-run --plugin tool-updater --result success --title " + title,
	}
	if calls := f.commands(); !slices.Equal(calls, wantCalls) {
		t.Fatalf("commands = %q, want %q", calls, wantCalls)
	}
	for _, c := range f.calls[:2] {
		if !slices.Equal(c.extraEnv, []string{brewEnv}) {
			t.Errorf("%s ran with env %q, want %q", c, c.extraEnv, brewEnv)
		}
	}
	if !f.calls[0].combined || f.calls[1].combined {
		t.Errorf("update read combined streams = %v and the probe = %v, want true then false", f.calls[0].combined, f.calls[1].combined)
	}
	if len(f.calls[2].extraEnv) != 0 {
		t.Errorf("the receipt ran with env %q, want the inherited environment alone", f.calls[2].extraEnv)
	}
}

// TestReport_AllCurrent: an empty formulae list is the one all-clear, and it
// still records the run.
func TestReport_AllCurrent(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew outdated --json=v2": {out: allCurrent},
		"gt plugin record-run --plugin tool-updater --result success --title tool-updater: all tools current": {},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got,
		[]string{
			"[tool-updater]   dolt: up to date\n",
			"[tool-updater]   beads: up to date\n",
			"[tool-updater] === Done === tool-updater: all tools current\n",
		},
		[]string{"update available", "to act", "outdated="})
}

// TestReport_NonActionableToolStillBlocksTheAllClear: a tool the operator is
// not told to act on still counts, so the summary never claims "all current"
// while beads is behind (gt-th5it).
func TestReport_NonActionableToolStillBlocksTheAllClear(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew outdated --json=v2": {out: outdatedBeads},
		"gt plugin record-run --plugin tool-updater --result success --title tool-updater: outdated=1 (report-only)": {},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got,
		[]string{
			"[tool-updater]   beads: update available (installed: 1.2.2, available: 1.3.1)\n",
			"[tool-updater] === Done === tool-updater: outdated=1 (report-only)\n",
		},
		[]string{"all tools current", "to act"})
}

// TestReport_ProbeFailsClosed: a failed probe is an error, never the all-clear,
// and it records no receipt for a report it could not make.
func TestReport_ProbeFailsClosed(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew outdated --json=v2": {err: errors.New("exit 1")},
	}}
	code, got := report(t, f)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	checkOutput(t, got,
		[]string{"[tool-updater] ERROR: brew outdated failed; cannot determine tool status\n"},
		[]string{"all tools current", "record-run"})
	if n := len(f.calls); n != 2 {
		t.Errorf("ran %d commands, want the refresh and the probe only: %q", n, f.commands())
	}
}

// TestReport_UnreadableProbe: every shape the probe can arrive in that is not
// a list of entries fails closed, with no all-clear and no receipt.
func TestReport_UnreadableProbe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, probe string }{
		{"not JSON", "not json"},
		{"a top-level list", `[]`},
		{"JSON null", `null`},
		{"no formulae key", `{"other":[]}`},
		{"formulae is an object", `{"formulae":{}}`},
		{"formulae is null", `{"formulae":null}`},
		{"a name that is not a string", `{"formulae":[{"name":7}]}`},
		{"versions that are not a list", `{"formulae":[{"name":"dolt","installed_versions":"2.1"}]}`},
		{"a current version that is not a string", `{"formulae":[{"name":"dolt","current_version":2.1}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fake{replies: map[string]reply{
				"brew outdated --json=v2": {out: tc.probe},
			}}
			code, got := report(t, f)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			checkOutput(t, got,
				[]string{"[tool-updater] ERROR: unreadable brew outdated output; cannot determine tool status\n"},
				[]string{"all tools current", "up to date"})
			if n := len(f.calls); n != 2 {
				t.Errorf("ran %d commands, want the refresh and the probe only: %q", n, f.commands())
			}
		})
	}
}

// TestReport_BrewMissing: the report needs brew, and says so once, before it
// runs anything.
func TestReport_BrewMissing(t *testing.T) {
	t.Parallel()
	f := &fake{missing: "brew"}
	code, got := report(t, f)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := "[tool-updater] ERROR: brew not found on PATH; nothing to report\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %q, want no commands at all", f.commands())
	}
}

// TestReport_StaleIndex: a failed refresh is surfaced, and its report still
// runs — marked stale, so it cannot claim a confident all-clear (gt-th5it).
func TestReport_StaleIndex(t *testing.T) {
	t.Parallel()
	const title = "tool-updater: all tools current [formula index stale: brew update failed]"
	f := &fake{replies: map[string]reply{
		"brew update":             {out: "Error: Not a git repository\nsome detail\n", err: errors.New("exit 128")},
		"brew outdated --json=v2": {out: allCurrent},
		"gt plugin record-run --plugin tool-updater --result success --title " + title: {},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got,
		[]string{
			"Error: Not a git repository\n",
			"some detail\n",
			"[tool-updater] WARNING: brew update failed; the Homebrew formula index may be stale\n",
			"[tool-updater] === Done === " + title + "\n",
		},
		nil)
	want := "gt plugin record-run --plugin tool-updater --result success --title " + title
	if len(f.calls) != 3 || f.calls[2].String() != want {
		t.Errorf("commands = %q, want the receipt %q last", f.commands(), want)
	}
}

// TestReport_UpdateTail: only the end of `brew update` reaches the log, since
// that is what explains a failure.
func TestReport_UpdateTail(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew update":             {out: "one\ntwo\nthree\nfour\nfive\n"},
		"brew outdated --json=v2": {out: allCurrent},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got,
		[]string{"three\n", "four\n", "five\n"},
		[]string{"one\n", "two\n"})
}

// TestReport_VersionsFallBack: a probe entry missing its versions reads "?"
// rather than a blank or a zero version.
func TestReport_VersionsFallBack(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew outdated --json=v2": {out: `{"formulae":[
			{"name":"dolt","installed_versions":["1.0","2.0"]},
			{"name":"beads","installed_versions":[],"current_version":""}]}`},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got,
		[]string{
			"[tool-updater]   dolt: update available (installed: 2.0, available: ?)\n",
			"[tool-updater]   beads: update available (installed: ?, available: ?)\n",
		},
		nil)
}

// TestReport_FirstEntryWins: a tool listed twice is reported from its first
// entry, the one the shell's next() read.
func TestReport_FirstEntryWins(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew outdated --json=v2": {out: `{"formulae":[
			{"name":"dolt","installed_versions":["1.0"],"current_version":"2.0"},
			{"name":"dolt","installed_versions":["9.9"],"current_version":"9.9"}]}`},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got,
		[]string{"[tool-updater]   dolt: update available (installed: 1.0, available: 2.0)\n"},
		[]string{"9.9"})
}

// TestReport_ReceiptFailureDoesNotFailTheReport: the receipt is a convenience,
// so a town that cannot record it still gets the report and exit 0.
func TestReport_ReceiptFailureDoesNotFailTheReport(t *testing.T) {
	t.Parallel()
	f := &fake{replies: map[string]reply{
		"brew outdated --json=v2": {out: allCurrent},
		"gt plugin record-run --plugin tool-updater --result success --title tool-updater: all tools current": {err: errors.New("no town")},
	}}
	code, got := report(t, f)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	checkOutput(t, got, []string{"=== Done === tool-updater: all tools current\n"}, []string{"ERROR"})
	if len(f.calls) != 3 {
		t.Errorf("ran %q, want the refresh, the probe and the receipt", f.commands())
	}
}

// TestOutputOf: a command's output reaches the caller whether it succeeded or
// not, and its error is passed through rather than swallowed.
func TestOutputOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		written string
		fail    bool
	}{
		{"a run that writes and succeeds", "out\n", false},
		{"a run that writes and fails", "boom\n", true},
		{"a run that writes nothing", "", false},
		{"a run that writes nothing and fails", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			got, err := outputOf(&buf, func() error {
				buf.WriteString(tc.written)
				if tc.fail {
					return errors.New("exit 1")
				}
				return nil
			})
			if got != tc.written {
				t.Errorf("output = %q, want %q", got, tc.written)
			}
			if (err != nil) != tc.fail {
				t.Errorf("error = %v, want a failure: %v", err, tc.fail)
			}
		})
	}
}

func TestTailLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"empty output", "", nil},
		{"one line", "a\n", []string{"a"}},
		{"exactly the bound", "a\nb\nc\n", []string{"a", "b", "c"}},
		{"past the bound", "a\nb\nc\nd\ne\n", []string{"c", "d", "e"}},
		{"no trailing newline", "a\nb\nc\nd", []string{"b", "c", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tailLines(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("tailLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
