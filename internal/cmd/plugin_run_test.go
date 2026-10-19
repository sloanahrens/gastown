package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/plugin"
)

// fakePluginRecorder is an in-memory run ledger: a fixed count of recent
// runs, and every receipt written.
type fakePluginRecorder struct {
	recentRuns int
	counted    []string // plugin names whose gate was checked
	recorded   []plugin.PluginRunRecord
}

func (f *fakePluginRecorder) CountRunsSince(name, _ string) (int, error) {
	f.counted = append(f.counted, name)
	return f.recentRuns, nil
}

func (f *fakePluginRecorder) RecordRun(rec plugin.PluginRunRecord) (string, error) {
	f.recorded = append(f.recorded, rec)
	return "gt-test-run", nil
}

// newFakePluginRun builds a minimal Gas Town workspace (mayor/town.json)
// with one plugin under <townRoot>/plugins/<name>, and a pluginRun over it
// recording into rec.
func newFakePluginRun(t *testing.T, name, pluginMD string, rec *fakePluginRecorder) pluginRun {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(townRoot, "plugins", name)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.md"), []byte(pluginMD), 0644); err != nil {
		t.Fatal(err)
	}
	return pluginRun{
		scanner:  plugin.NewScanner(townRoot, nil),
		recorder: rec,
		out:      io.Discard,
		errOut:   io.Discard,
	}
}

// A closed cooldown gate used to be recorded as a `result:skipped` receipt.
// That receipt is itself a type:plugin-run bead, so it counted toward the
// daemon's own CountRunsSince query and pushed the daemon's next scheduled
// dispatch further out on every refused manual invocation (gt-o1z7,
// gt-wisp-1h80 finding 875706a6c45c). A refusal must record nothing.
func TestRunPluginRun_ClosedGateRecordsNoReceipt(t *testing.T) {
	t.Parallel()
	// One prior run inside the cooldown window closes the gate.
	rec := &fakePluginRecorder{recentRuns: 1}
	r := newFakePluginRun(t, "cooldown-plugin", `+++
name = "cooldown-plugin"
description = "test plugin"

[gate]
type = "cooldown"
duration = "1h"
+++

# Instructions

Do the thing.
`, rec)

	if err := r.run("cooldown-plugin"); err != nil {
		t.Fatalf("runPluginRun: %v", err)
	}
	if len(rec.counted) != 1 {
		t.Fatalf("expected one gate check, got %v", rec.counted)
	}
	if len(rec.recorded) != 0 {
		t.Fatalf("closed-gate refusal recorded a receipt, extending the daemon's own cooldown window: %+v", rec.recorded)
	}
}

// The receipt for a merely-printed run must not read as success: a success
// receipt for work nobody did was the original fail-open bug (gt-o1z7,
// gt-wisp-1h80 finding df02feb0c9ed).
func TestRunPluginRun_PrintedInstructionsRecordsPrintedNotSuccess(t *testing.T) {
	t.Parallel()
	rec := &fakePluginRecorder{}
	r := newFakePluginRun(t, "manual-plugin", `+++
name = "manual-plugin"
description = "test plugin"

[gate]
type = "manual"
+++

# Instructions

Do the thing by hand.
`, rec)

	if err := r.run("manual-plugin"); err != nil {
		t.Fatalf("runPluginRun: %v", err)
	}
	if len(rec.recorded) != 1 || rec.recorded[0].Result != plugin.ResultPrinted {
		t.Fatalf("expected one result:printed receipt, got %+v", rec.recorded)
	}
}

// A script-type plugin has no manual trigger: `gt plugin run` refuses it
// outright, since there is no script interpreter here and the daemon
// heartbeat is that plugin's only executor (gt-o1z7).
func TestRunPluginRun_ScriptPluginRefuses(t *testing.T) {
	t.Parallel()
	rec := &fakePluginRecorder{}
	r := newFakePluginRun(t, "script-plugin", `+++
name = "script-plugin"
description = "test plugin"

[gate]
type = "manual"

[execution]
type = "script"
+++

# Instructions

(unused for a script plugin)
`, rec)

	if err := r.run("script-plugin"); err == nil {
		t.Fatal("expected an error for a script-type plugin, got nil")
	}
	if len(rec.recorded) != 0 {
		t.Fatalf("script-type refusal recorded a receipt: %+v", rec.recorded)
	}
}

// The dry run previews the real run, so it refuses what the real run refuses:
// a script plugin on an open gate printed "Gate open" and exited 0 while the
// real run returned this error.
func TestPluginRunDryRunRefusesScriptPlugin(t *testing.T) {
	t.Parallel()
	rec := &fakePluginRecorder{}
	r := newFakePluginRun(t, "script-plugin", `+++
name = "script-plugin"
description = "test plugin"

[gate]
type = "manual"

[execution]
type = "script"
+++

# Instructions

(unused for a script plugin)
`, rec)
	r.dryRun = true
	var out, errOut bytes.Buffer
	r.out, r.errOut = &out, &errOut

	err := r.run("script-plugin")
	if err == nil {
		t.Fatalf("dry run of a script plugin returned nil; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "script-type") {
		t.Errorf("error = %q, want the real run's script-type refusal", err)
	}
	if strings.Contains(out.String(), "Gate open") {
		t.Errorf("dry run promised an execution that cannot happen; output:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "has a run.sh") {
		t.Errorf("stderr = %q, want the real run's refusal message", errOut.String())
	}
}

// The list entry cuts a long description at 47 bytes; a multi-byte rune
// straddling that byte must be dropped whole, not sliced into invalid UTF-8.
func TestPrintPluginSummaryTruncatesOnRuneBoundary(t *testing.T) {
	t.Parallel()
	// 46 ASCII bytes, then 3-byte runes: byte 47 falls inside the first one.
	desc := strings.Repeat("a", 46) + strings.Repeat("世", 10)
	p := &plugin.Plugin{Name: "wide", Description: desc}

	var out bytes.Buffer
	writePluginSummary(&out, p)

	if !utf8.ValidString(out.String()) {
		t.Errorf("summary is not valid UTF-8: %q", out.String())
	}
	if !strings.Contains(out.String(), strings.Repeat("a", 46)+"...") {
		t.Errorf("summary = %q, want the description cut after the 46 ASCII bytes", out.String())
	}
}
