package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

var updateUsageAttribGolden = flag.Bool("update-usage-attrib-golden", false, "rewrite testdata/usage_attrib_scan_show.golden")

const usageAttribGoldenPath = "usage_attrib_scan_show.golden"

// attribTown is a town with one bead run on seat gastown/nux: a sling, the seat's transcript and
// subagent file, and a done event. Its files are written where the scan reads them.
func attribTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	evs := []events.Event{
		{Timestamp: "2026-11-13T10:00:00Z", Source: "gt", Type: events.TypeSling, Actor: "mayor/",
			Payload: map[string]interface{}{"bead": "gt-abc", "target": "gastown/polecats/nux"}, Visibility: "audit"},
		{Timestamp: "2026-11-13T11:00:00Z", Source: "gt", Type: events.TypeDone, Actor: "gastown/polecats/nux",
			Payload: map[string]interface{}{"bead": "gt-abc", "branch": "polecat/nux/gt-abc"}, Visibility: "audit"},
	}
	var lines []byte
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(town, events.EventsFile), lines, 0o644); err != nil {
		t.Fatal(err)
	}

	folder := filepath.Join(session.SandboxTranscriptCopyDir(town, "gastown", "nux"), session.SandboxTranscriptFolder("gastown", "nux"))
	transcript := filepath.Join(folder, "sess-1.jsonl")
	writeAttribFile(t, transcript,
		`{"type":"assistant","timestamp":"2026-11-13T10:10:00Z","message":{"id":"msg-1","model":"claude-sonnet-5-5","usage":{"input_tokens":100,"cache_creation_input_tokens":40,"cache_read_input_tokens":900,"output_tokens":25}}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-11-13T10:12:00Z","message":{"id":"msg-2","model":"claude-sonnet-5-5","usage":{"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":300,"output_tokens":11}}}`+"\n")
	writeAttribFile(t, filepath.Join(folder, "sess-1", "subagents", "agent-review.jsonl"),
		`{"type":"assistant","timestamp":"2026-11-13T10:20:00Z","message":{"id":"sub-1","model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":50,"output_tokens":6}}}`+"\n")
	when := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)
	if err := os.Chtimes(transcript, when, when); err != nil {
		t.Fatal(err)
	}
	return town
}

func writeAttribFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// attribDepsFor runs the attrib command against town, with the clock fixed at now and the real
// events, landing and seat-copy reads. The zone is a fixed UTC-6 standing in for America/Chicago's
// winter offset, so the test does not read the host's zone database.
func attribDepsFor(town string, now time.Time) usageAttribDeps {
	d := defaultUsageAttribDeps()
	d.townRoot = func() (string, error) { return town, nil }
	d.scan.Now = func() time.Time { return now }
	d.zone = func(name string) (*time.Location, error) {
		if name != attribZone {
			return nil, fmt.Errorf("unexpected zone %q", name)
		}
		return time.FixedZone("CST", -6*60*60), nil
	}
	return d
}

// runAttrib executes `gt usage attrib <args>` and returns its stdout, stderr, and the exit code the
// command reports (0 when it returns nil).
func runAttrib(t *testing.T, d usageAttribDeps, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newUsageAttribCmd(d)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exit *SilentExitError
	if errors.As(err, &exit) {
		return out.String(), errOut.String(), exit.Code
	}
	t.Fatalf("gt usage attrib %v: %v", args, err)
	return "", "", -1
}

func TestUsageAttribScanAndShowGolden(t *testing.T) {
	t.Parallel()
	town := attribTown(t)
	d := attribDepsFor(town, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))

	scanOut, _, code := runAttrib(t, d, "scan")
	if code != 0 {
		t.Fatalf("scan exit = %d, want 0", code)
	}
	if want := "scanned: 1 records written, 0 final kept, 0 incomplete, 0 unmatched sessions\n"; scanOut != want {
		t.Errorf("scan output = %q, want %q", scanOut, want)
	}

	got, _, code := runAttrib(t, d, "show", "gt-abc")
	if code != 0 {
		t.Fatalf("show exit = %d, want 0", code)
	}
	golden := filepath.Join("testdata", usageAttribGoldenPath)
	if *updateUsageAttribGolden {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("show output differs from %s (rewrite with -update-usage-attrib-golden):\n%s\nwant:\n%s", golden, got, want)
	}
}

func TestUsageAttribShowExitCodes(t *testing.T) {
	t.Parallel()
	town := attribTown(t)
	d := attribDepsFor(town, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	usageDir := filepath.Join(town, ".runtime", "usage")

	if _, errOut, code := runAttrib(t, d, "show", "gt-missing"); code != 1 || !strings.Contains(errOut, "no usage record for bead gt-missing") {
		t.Errorf("show unknown bead: exit %d stderr %q, want exit 1 naming the bead", code, errOut)
	}

	writeAttribFile(t, usageattrib.WorkRecordPath(usageDir, "gt-bad"), "{not a record")
	if _, _, code := runAttrib(t, d, "show", "gt-bad"); code != 2 {
		t.Errorf("show of an unreadable record: exit %d, want 2", code)
	}
	if _, _, code := runAttrib(t, d, "show", "../gt-abc"); code != 2 {
		t.Errorf("show of a bead id with a path in it: exit %d, want 2", code)
	}
}

func TestUsageAttribScanRefusesWithoutWriting(t *testing.T) {
	t.Parallel()
	town := attribTown(t)
	writeAttribFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":`)
	d := attribDepsFor(town, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))

	_, errOut, code := runAttrib(t, d, "scan")
	if code != 2 || !strings.Contains(errOut, "workspaceid") {
		t.Errorf("scan with a malformed workspaces file: exit %d stderr %q, want exit 2 naming the file", code, errOut)
	}
	// The fixture's transcript copy lives under the usage directory, so the check is on what a scan writes.
	usageDir := filepath.Join(town, ".runtime", "usage")
	for _, p := range []string{filepath.Join(usageDir, "beads"), filepath.Join(usageDir, usageattrib.StateFile), filepath.Join(usageDir, usageattrib.UnmatchedFile)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written after a hard error: stat err = %v", p, err)
		}
	}
}

func TestUsageAttribScanSinceNeedsADay(t *testing.T) {
	t.Parallel()
	town := attribTown(t)
	d := attribDepsFor(town, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	if _, errOut, code := runAttrib(t, d, "scan", "--since", "13/11/2026"); code != 2 || !strings.Contains(errOut, "YYYY-MM-DD") {
		t.Errorf("scan --since with a bad day: exit %d stderr %q, want exit 2", code, errOut)
	}
}

func TestUsageAttribScanSinceReadsTheDayInTheTownZone(t *testing.T) {
	t.Parallel()
	town := attribTown(t)
	d := attribDepsFor(town, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	// The run ended at 11:00 UTC on 2026-11-13, which is 05:00 in America/Chicago (CST, UTC-6).
	// --since 2026-11-13 is midnight Chicago, so the run is kept; --since 2026-11-14 drops it.
	if out, _, _ := runAttrib(t, d, "scan", "--since", "2026-11-13"); !strings.Contains(out, "1 records written") {
		t.Errorf("scan --since 2026-11-13 output %q, want the run kept", out)
	}
	if out, _, _ := runAttrib(t, d, "scan", "--since", "2026-11-14"); !strings.Contains(out, "0 records written") {
		t.Errorf("scan --since 2026-11-14 output %q, want the run dropped", out)
	}
}

// TestUsageAttribScanHelpStatesWhatTheCodeDoes checks the scan's help and its --since flag against what
// the code does: open runs are kept, nothing is written when the read phase fails, and a write that
// fails partway leaves the beads before it rewritten, named by the error.
func TestUsageAttribScanHelpStatesWhatTheCodeDoes(t *testing.T) {
	t.Parallel()
	scan := newUsageAttribScanCmd(defaultUsageAttribDeps())
	help := strings.Join(strings.Fields(scan.Long), " ")
	for _, want := range []string{
		"and every run still open",
		"stops the scan before anything is written",
		"A write that fails partway leaves the beads before it rewritten",
		"the error names the file",
		"gives the same records as a scan that did not fail",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("scan help does not say %q", want)
		}
	}
	since := scan.Flags().Lookup("since")
	if since == nil {
		t.Fatal("scan has no --since flag")
	}
	if usage := strings.Join(strings.Fields(since.Usage), " "); !strings.Contains(usage, "and the runs still open") {
		t.Errorf("--since help = %q, want it to say open runs are kept", usage)
	}
}
