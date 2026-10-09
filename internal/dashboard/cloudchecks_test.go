package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The checks line's clock: every fixture below is written against this instant,
// so a stale reading is the staleness the test meant.
var checksNow = time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

// checksFixture writes a status file at a path under a temporary directory and
// returns that path. The body is written verbatim.
func checksFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// checksStatusJSON builds the plugin's status around the counts and items
// given, with the run timestamp in unix seconds.
func checksStatusJSON(t *testing.T, at int64, ok, warnings, failed int, items ...map[string]any) string {
	t.Helper()
	if items == nil {
		items = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{
		"at": at, "ok": ok, "warnings": warnings, "failed": failed, "items": items,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func checksItem(level, section, text string) map[string]any {
	return map[string]any{"level": level, "section": section, "text": text}
}

// A status the plugin wrote a minute ago reads fresh, with its counts and the
// one warning it found.
func TestCloudChecksReadsAFreshRun(t *testing.T) {
	t.Parallel()

	path := checksFixture(t, checksStatusJSON(t, checksNow.Add(-time.Minute).Unix(), 19, 1, 0,
		checksItem("warn", "The town", "promote behind")))
	c := NewCloudChecksReader(path).Read(checksNow)
	if c == nil {
		t.Fatal("a status file reads as no status")
	}
	if c.State != CloudChecksFresh {
		t.Fatalf("state %q, want fresh", c.State)
	}
	if c.OK != 19 || c.Warnings != 1 || c.Failed != 0 {
		t.Errorf("counts %d ok %d warning %d failed, want 19/1/0", c.OK, c.Warnings, c.Failed)
	}
	if !c.At.Equal(checksNow.Add(-time.Minute)) {
		t.Errorf("at %v, want the file's own timestamp", c.At)
	}
	if len(c.Items) != 1 || c.Items[0].Level != cloudCheckWarn || c.Items[0].Text != "promote behind" {
		t.Errorf("items are not the file's: %+v", c.Items)
	}
}

// A run with failures lists them before the warnings, whatever order the file
// wrote them in, and counts both.
func TestCloudChecksListsFailuresBeforeWarnings(t *testing.T) {
	t.Parallel()

	path := checksFixture(t, checksStatusJSON(t, checksNow.Unix(), 17, 1, 2,
		checksItem("warn", "Apps", "slow"),
		checksItem("fail", "Runners", "offline"),
		checksItem("fail", "Apps", "unreachable")))
	c := NewCloudChecksReader(path).Read(checksNow)
	if c.OK != 17 || c.Warnings != 1 || c.Failed != 2 {
		t.Errorf("counts %d/%d/%d, want 17/1/2", c.OK, c.Warnings, c.Failed)
	}
	if len(c.Items) != 3 || c.Items[0].Level != cloudCheckFail || c.Items[1].Level != cloudCheckFail || c.Items[2].Level != cloudCheckWarn {
		t.Errorf("items are not failures-first: %+v", c.Items)
	}
}

// The checks line rides the same poll as the patrol's report: a reader with a
// checks reader carries the status into the Cloud state, and a reader without
// one leaves the line absent.
func TestCloudReadFillsTheChecksLineFromTheSamePoll(t *testing.T) {
	t.Parallel()

	dir := cloudFixture(t, cloudReportJSON(t), checksNow.Format(time.RFC3339))
	status := checksFixture(t, checksStatusJSON(t, checksNow.Unix(), 20, 0, 0))
	c := NewCloudReader(dir).WithChecks(NewCloudChecksReader(status)).Read(checksNow)
	if c.Checks == nil || c.Checks.OK != 20 || c.Checks.State != CloudChecksFresh {
		t.Fatalf("checks %+v, want the status read on the same poll", c.Checks)
	}
	if bare := NewCloudReader(dir).Read(checksNow); bare.Checks != nil {
		t.Errorf("a reader with no checks reader filled the line: %+v", bare.Checks)
	}
}

// A file the plugin has not written for longer than its interval reads as
// stale: the pane must not present a run that has stopped as current.
func TestCloudChecksGoesStalePastTheInterval(t *testing.T) {
	t.Parallel()

	path := checksFixture(t, checksStatusJSON(t, checksNow.Add(-26*time.Minute).Unix(), 20, 0, 0))
	if c := NewCloudChecksReader(path).Read(checksNow); c.State != CloudChecksStale {
		t.Errorf("state %q at 26 minutes, want stale", c.State)
	}
	path = checksFixture(t, checksStatusJSON(t, checksNow.Add(-24*time.Minute).Unix(), 20, 0, 0))
	if c := NewCloudChecksReader(path).Read(checksNow); c.State != CloudChecksFresh {
		t.Errorf("state %q at 24 minutes, want fresh", c.State)
	}
}

// A plugin that is not installed is nothing to draw: a missing file reads as
// no status rather than as an error the operator would have to explain.
func TestCloudChecksDrawsNothingWhenTheFileIsMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gone", "status.json")
	if c := NewCloudChecksReader(path).Read(checksNow); c != nil {
		t.Errorf("a missing file reads as %+v, want nothing", c)
	}
}

// A file the reader cannot use is one named error, never a guess: invalid
// JSON, a body that is not an object, a wrong-shaped object, and a file over
// the size cap all reach the page as the same dim line.
func TestCloudChecksNamesAnUnreadableStatus(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", cloudChecksMaxBytes+1)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid JSON", "{not json"},
		{"empty", ""},
		{"an array", "[]"},
		{"no timestamp", `{"ok":1,"warnings":0,"failed":0}`},
		{"a negative count", `{"at":1700000000,"ok":1,"warnings":-1,"failed":0}`},
		{"oversized", `{"at":1700000000,"pad":"` + long + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCloudChecksReader(checksFixture(t, tc.body)).Read(checksNow)
			if c == nil || c.State != CloudChecksError {
				t.Fatalf("state %+v, want error", c)
			}
		})
	}
}

// The status is another process's file, so an item's text is stripped of the
// characters that carry no visible glyph and cut to the cap, and an item whose
// level this reader does not know is dropped rather than guessed at.
func TestCloudChecksStripsAndCapsTheItemsText(t *testing.T) {
	t.Parallel()

	hostile := "a\u0007b\nc\td‮e" + strings.Repeat("f", cloudChecksTextMax)
	path := checksFixture(t, checksStatusJSON(t, checksNow.Unix(), 1, 1, 1,
		checksItem("fail", "S‮D", hostile),
		checksItem("weird", "S", "dropped")))
	c := NewCloudChecksReader(path).Read(checksNow)
	if len(c.Items) != 1 {
		t.Fatalf("items %+v, want only the known level", c.Items)
	}
	it := c.Items[0]
	if strings.ContainsAny(it.Text, "\u0007\n\t‮") {
		t.Errorf("item text keeps control or format runes: %q", it.Text)
	}
	if !strings.HasPrefix(it.Text, "ab c de") {
		t.Errorf("item text %q, want the visible runes kept", it.Text)
	}
	r := []rune(it.Text)
	if len(r) != cloudChecksTextMax+1 || r[len(r)-1] != '…' {
		t.Errorf("item text is %d runes and ends %q, want the cap and the cut mark", len(r), string(r[len(r)-1]))
	}
	if it.Section != "SD" {
		t.Errorf("section %q, want the format rune dropped", it.Section)
	}
}

// The page draws a fixed number of rows, so a status with more items than that
// is cut, with its counts still covering the whole run.
func TestCloudChecksCapsTheItems(t *testing.T) {
	t.Parallel()

	items := make([]map[string]any, 0, cloudChecksMaxItems+5)
	for i := 0; i < cloudChecksMaxItems+5; i++ {
		items = append(items, checksItem("fail", "S", "x"))
	}
	path := checksFixture(t, checksStatusJSON(t, checksNow.Unix(), 0, 0, len(items), items...))
	c := NewCloudChecksReader(path).Read(checksNow)
	if len(c.Items) != cloudChecksMaxItems {
		t.Errorf("items %d, want the cap %d", len(c.Items), cloudChecksMaxItems)
	}
	if c.Failed != len(items) {
		t.Errorf("failed %d, want the whole run's %d", c.Failed, len(items))
	}
}
