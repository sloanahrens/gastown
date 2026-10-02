package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
)

var testLeakNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// leakTown builds a town with one registered rig, the watched directories and
// an events log holding one legitimate line.
func leakTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	for _, dir := range []string{"mayor", ".beads", ".dolt-data"} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeLeakFile(t, filepath.Join(town, "mayor", "rigs.json"), `{"version":1,"rigs":{"gastown":{}}}`)
	writeLeakFile(t, filepath.Join(town, events.EventsFile),
		`{"ts":"2026-09-30T11:00:00Z","source":"gt","type":"boot","actor":"mayor","visibility":"feed"}`+"\n")
	return town
}

func writeLeakFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLeakEvents(t *testing.T, town, content string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(town, events.EventsFile), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// newLeakCheck fixes the clock, makes every file old, and gives the town tmux
// server the given sessions.
func newLeakCheck(sessions ...string) *TestLeakCheck {
	c := NewTestLeakCheck()
	c.nowForTest = func() time.Time { return testLeakNow }
	c.fileAgeForTest = func(string) time.Duration { return time.Hour }
	c.townSessionsForTest = func() ([]string, error) { return sessions, nil }
	return c
}

func runLeakCheck(t *testing.T, c *TestLeakCheck, town string) *CheckResult {
	t.Helper()
	return c.Run(&CheckContext{TownRoot: town})
}

func TestTestLeakCheck_CleanTownPasses(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	if r := runLeakCheck(t, newLeakCheck("hq-mayor", "gt-opal"), town); r.Status != StatusOK {
		t.Errorf("clean town: %v %s %v", r.Status, r.Message, r.Details)
	}
}

// gt-x9o: a fixture rig that is not in rigs.json is the leak; a registered
// rig's concurrent traffic is not.
func TestTestLeakCheck_FlagsFixtureActorEvents(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	appendLeakEvents(t, town,
		`{"ts":"2026-09-30T11:01:00Z","source":"gt","type":"done","actor":"gastown/witness","visibility":"feed"}`+"\n"+
			`{"ts":"2026-09-30T11:01:01Z","source":"gt","type":"spawn","actor":"myr/mycat","payload":{"caller":"myr/mycat"},"visibility":"feed"}`+"\n")

	r := runLeakCheck(t, newLeakCheck(), town)
	if r.Status != StatusWarning || len(r.Details) != 1 || !strings.Contains(r.Details[0], "myr/mycat") {
		t.Errorf("fixture actor: %v %s %v; want one warning naming myr/mycat", r.Status, r.Message, r.Details)
	}
}

// Old lines from a rig since removed are outside the window and do not warn
// for the life of the log.
func TestTestLeakCheck_IgnoresEventsOutsideWindow(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	appendLeakEvents(t, town,
		`{"ts":"2026-09-28T11:00:00Z","source":"gt","type":"spawn","actor":"retired/polecat","visibility":"feed"}`+"\n")
	if r := runLeakCheck(t, newLeakCheck(), town); r.Status != StatusOK {
		t.Errorf("event outside the window: %v %v", r.Status, r.Details)
	}
}

// gt-ro0, gt-kvc, gt-d9423: every built-in actor and the daemon's crash
// detection (which names a session as the actor) are legitimate.
func TestTestLeakCheck_ToleratesTownActors(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	var content strings.Builder
	for _, actor := range BuiltinActorPrefixes() {
		line, err := json.Marshal(map[string]string{"ts": "2026-09-30T11:02:00Z", "type": "test", "actor": actor})
		if err != nil {
			t.Fatal(err)
		}
		content.Write(append(line, '\n'))
	}
	content.WriteString(`{"ts":"2026-09-30T11:03:00Z","type":"session_death","actor":"gt-opal","payload":{"caller":"daemon"}}` + "\n")
	appendLeakEvents(t, town, content.String())

	if r := runLeakCheck(t, newLeakCheck(), town); r.Status != StatusOK {
		t.Errorf("town actors flagged: %v", r.Details)
	}
}

// gt-rqajq: gt doctor --fix kills zombies and orphans, and gt down stops the
// Mayor, each appending a session_death that names the tmux session as the
// actor. A gate running beside that housekeeping is not leaking fixtures, so
// every town-process stamp is tolerated — and only those stamps.
func TestTestLeakCheck_ToleratesTownProcessSessionDeaths(t *testing.T) {
	t.Parallel()
	for _, caller := range []string{events.CallerDaemon, events.CallerDoctor, events.CallerDown} {
		t.Run(caller, func(t *testing.T) {
			t.Parallel()
			town := leakTown(t)
			line, err := json.Marshal(map[string]any{
				"ts":      "2026-09-30T11:05:00Z",
				"type":    "session_death",
				"actor":   "gt-gastown-opal",
				"payload": map[string]string{"caller": caller, "session": "gt-gastown-opal"},
			})
			if err != nil {
				t.Fatal(err)
			}
			appendLeakEvents(t, town, string(line)+"\n")

			if r := runLeakCheck(t, newLeakCheck(), town); r.Status != StatusOK {
				t.Errorf("%s session_death flagged: %v %s %v", caller, r.Status, r.Message, r.Details)
			}
		})
	}
}

// A session-id actor with a caller no town process stamps is still the leak
// the check exists to catch, so the tolerance is not "any session-ish actor".
func TestTestLeakCheck_FlagsSessionActorWithUnknownCaller(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	appendLeakEvents(t, town,
		`{"ts":"2026-09-30T11:06:00Z","type":"session_death","actor":"gt-gastown-opal","payload":{"caller":"some-test"},"visibility":"feed"}`+"\n")

	r := runLeakCheck(t, newLeakCheck(), town)
	if r.Status != StatusWarning || len(r.Details) != 1 || !strings.Contains(r.Details[0], "gt-gastown-opal") {
		t.Errorf("unknown caller: %v %s %v; want one warning naming the session actor", r.Status, r.Message, r.Details)
	}
}

// A complete malformed line is reported; a trailing line still being
// appended is not.
func TestTestLeakCheck_MalformedAndPartialLines(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	appendLeakEvents(t, town, "not json\n"+`{"ts":"2026-09-30T11:04:00Z","actor":"myr`)

	r := runLeakCheck(t, newLeakCheck(), town)
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "unparseable") {
		t.Errorf("malformed/partial lines: %v; want only the complete malformed line", r.Details)
	}
}

// gt-lqri: a .tmp no known writer produces is residue once it is past the
// grace period; known atomic-write temps and bd's .~ temps are not.
func TestTestLeakCheck_AbandonedTemps(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	for _, rel := range []string{
		"stray.tmp",
		filepath.Join(".beads", "leaked.tmp"),
		events.EventsFile + events.PruneTempSuffix,
		filepath.Join(".beads", beads.RoutesTempPrefix+"12345.tmp"),
		filepath.Join(".beads", ".~issues.jsonl.4096041347"),
	} {
		writeLeakFile(t, filepath.Join(town, rel), "")
	}

	r := runLeakCheck(t, newLeakCheck(), town)
	got := strings.Join(r.Details, "\n")
	if len(r.Details) != 2 || !strings.Contains(got, "stray.tmp") || !strings.Contains(got, ".beads/leaked.tmp") {
		t.Errorf("abandoned temps: %v; want exactly stray.tmp and .beads/leaked.tmp", r.Details)
	}

	young := newLeakCheck()
	young.fileAgeForTest = func(string) time.Duration { return time.Minute }
	if r := runLeakCheck(t, young, town); r.Status != StatusOK {
		t.Errorf("temps inside the grace period: %v", r.Details)
	}
}

// gt-2bj: a gt-test-* session on the town server reads as a phantom polecat.
func TestTestLeakCheck_FlagsTestSessionsOnTownServer(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	r := runLeakCheck(t, newLeakCheck("gt-opal", "gt-test-phantom"), town)
	if r.Status != StatusWarning || len(r.Details) != 1 || !strings.Contains(r.Details[0], "gt-test-phantom") {
		t.Errorf("test session: %v %v", r.Status, r.Details)
	}
}

// A session list that failed is an unknown, not a pass.
func TestTestLeakCheck_SessionListFailureIsUnknown(t *testing.T) {
	t.Parallel()
	town := leakTown(t)
	c := newLeakCheck()
	c.townSessionsForTest = func() ([]string, error) { return nil, errors.New("tmux: permission denied") }
	if r := runLeakCheck(t, c, town); r.Status != StatusSkipped {
		t.Errorf("failed session list: %v %s; want skipped", r.Status, r.Message)
	}
}
