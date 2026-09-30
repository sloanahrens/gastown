package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// scriptedBD is a bdRunner that records each call's arguments, one line per
// call, and answers from reply keyed by the bd verb (the first argument). An
// unscripted verb fails, as the old fake bd's catch-all exit 2 did.
type scriptedBD struct {
	reply map[string]string
	calls []string
}

func (s *scriptedBD) run(_ context.Context, _ beads.SubprocessEnvMode, args ...string) ([]byte, string, error) {
	s.calls = append(s.calls, strings.Join(args, " "))
	out, ok := s.reply[args[0]]
	if !ok {
		return nil, "unscripted bd " + args[0], errors.New("exit status 2")
	}
	return []byte(out), "", nil
}

// newScriptedRecorder is a Recorder whose bd is scripted by reply.
func newScriptedRecorder(t *testing.T, reply map[string]string) (*Recorder, *scriptedBD) {
	t.Helper()
	bd := &scriptedBD{reply: reply}
	r := NewRecorder(t.TempDir())
	r.bd = bd.run
	return r, bd
}

func TestPluginRunRecord(t *testing.T) {
	t.Parallel()
	record := PluginRunRecord{
		PluginName: "test-plugin",
		RigName:    "gastown",
		Result:     ResultSuccess,
		Body:       "Test run completed successfully",
	}

	if record.PluginName != "test-plugin" {
		t.Errorf("expected plugin name 'test-plugin', got %q", record.PluginName)
	}
	if record.RigName != "gastown" {
		t.Errorf("expected rig name 'gastown', got %q", record.RigName)
	}
	if record.Result != ResultSuccess {
		t.Errorf("expected result 'success', got %q", record.Result)
	}
}

func TestRecordRunCreatesAndClosesReceipt(t *testing.T) {
	t.Parallel()
	recorder, bd := newScriptedRecorder(t, map[string]string{
		"create": `{"id":"gt-test-run"}` + "\n",
		"close":  "",
	})
	id, err := recorder.RecordRun(PluginRunRecord{
		PluginName:  "tool-updater",
		RigName:     "gastown",
		Result:      RunResult("warning"),
		Title:       "tool-updater: failed=brew",
		Body:        "brew failed",
		ExtraLabels: []string{"source:test"},
	})
	if err != nil {
		t.Fatalf("RecordRun failed: %v", err)
	}
	if id != "gt-test-run" {
		t.Fatalf("RecordRun id = %q, want gt-test-run", id)
	}

	log := strings.Join(bd.calls, "\n")
	for _, want := range []string{
		"create --ephemeral --json -t chore --title=tool-updater: failed=brew",
		"-l type:plugin-run",
		"-l plugin:tool-updater",
		"-l result:warning",
		"-l rig:gastown",
		"-l source:test",
		"--description=brew failed",
		"close gt-test-run --reason plugin run recorded",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("fake bd log missing %q in:\n%s", want, log)
		}
	}
}

func TestRunResultConstants(t *testing.T) {
	t.Parallel()
	if ResultSuccess != "success" {
		t.Errorf("expected ResultSuccess to be 'success', got %q", ResultSuccess)
	}
	if ResultFailure != "failure" {
		t.Errorf("expected ResultFailure to be 'failure', got %q", ResultFailure)
	}
	if ResultSkipped != "skipped" {
		t.Errorf("expected ResultSkipped to be 'skipped', got %q", ResultSkipped)
	}
	// ResultWarning must stay distinct from ResultSuccess: it is what a
	// plugin records for a run that found something and escalated it, and
	// collapsing the two would report an escalated signal as a quiet run.
	if ResultWarning != "warning" {
		t.Errorf("expected ResultWarning to be 'warning', got %q", ResultWarning)
	}
	if ResultWarning == ResultSuccess {
		t.Error("ResultWarning must not equal ResultSuccess")
	}
	// ResultPrinted must stay distinct from ResultSuccess: it is what `gt
	// plugin run` records for a merely-printed, not-yet-executed run
	// (gt-o1z7). Collapsing the two back together is the fail-open bug.
	if ResultPrinted != "printed" {
		t.Errorf("expected ResultPrinted to be 'printed', got %q", ResultPrinted)
	}
	if ResultPrinted == ResultSuccess {
		t.Error("ResultPrinted must not equal ResultSuccess")
	}
}

func TestNewRecorder(t *testing.T) {
	t.Parallel()
	recorder := NewRecorder("/tmp/test-town")
	if recorder == nil {
		t.Fatal("NewRecorder returned nil")
	}
	if recorder.townRoot != "/tmp/test-town" {
		t.Errorf("expected townRoot '/tmp/test-town', got %q", recorder.townRoot)
	}
}

// TestRunsSinceSendsAbsoluteCutoff checks that a plugin gate duration is
// read as Go's time.ParseDuration ("30m" is minutes) and sent to bd as an
// absolute RFC3339 cutoff, never as the raw duration: bd's compact duration
// reads "m" as months.
func TestRunsSinceSendsAbsoluteCutoff(t *testing.T) {
	t.Parallel()
	recorder, bd := newScriptedRecorder(t, map[string]string{"list": "[]\n"})
	before := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	if _, err := recorder.GetRunsSince("p", "30m"); err != nil {
		t.Fatalf("GetRunsSince: %v", err)
	}
	after := time.Now().Add(-30 * time.Minute)
	var cutoff string
	for _, a := range strings.Fields(strings.Join(bd.calls, " ")) {
		if v, ok := strings.CutPrefix(a, "--created-after="); ok {
			cutoff = v
		}
	}
	got, err := time.Parse(time.RFC3339, cutoff)
	if err != nil {
		t.Fatalf("--created-after=%q is not RFC3339 (calls %q): %v", cutoff, bd.calls, err)
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("cutoff %v, want 30 minutes ago (between %v and %v)", got, before, after)
	}
	if _, err := recorder.GetRunsSince("p", "bogus"); err == nil {
		t.Error("GetRunsSince(\"bogus\") = nil error, want a parse error")
	}
}

// TestQueryRunsIncludesInfraFlag guards against gt-5sq: RecordRun creates
// receipts with --ephemeral, and `bd list` hides ephemeral beads by default
// even with --all. Without --include-infra, queryRuns always sees zero
// results, so cooldown gates never see a "last run" and never gate.
func TestQueryRunsIncludesInfraFlag(t *testing.T) {
	t.Parallel()
	recorder, bd := newScriptedRecorder(t, map[string]string{"list": "[]\n"})
	if _, err := recorder.CountRunsSince("tool-updater", "1h"); err != nil {
		t.Fatalf("CountRunsSince failed: %v", err)
	}

	log := strings.Join(bd.calls, "\n")
	if !strings.Contains(log, "--include-infra") {
		t.Fatalf("queryRuns did not pass --include-infra, ephemeral receipts would never be found:\n%s", log)
	}
}

// A ResultPrinted receipt records that `gt plugin run` printed a plugin's
// instructions without doing any work. It must not count toward the
// cooldown gate — otherwise printing instructions for a cooldown-gated
// plugin holds the daemon off for a full cooldown window even though
// nothing happened (gt-o1z7, finding f53b7b837c35).
func TestCountRunsSinceExcludesPrinted(t *testing.T) {
	t.Parallel()
	recorder, _ := newScriptedRecorder(t, map[string]string{
		"list": `[{"id":"gt-1","title":"Plugin run: p","created_at":"2020-01-01T00:00:00Z","labels":["type:plugin-run","plugin:p","result:printed"]}]` + "\n",
	})
	count, err := recorder.CountRunsSince("p", "1h")
	if err != nil {
		t.Fatalf("CountRunsSince failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountRunsSince = %d, want 0: a printed receipt must not satisfy the cooldown gate", count)
	}
}
