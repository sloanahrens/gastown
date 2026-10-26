package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
)

// omStatsFixture writes the two sources gt land om-stats reads into dir and
// returns their paths: three verdicts on two days, and a daemon-log day before
// the first record.
func omStatsFixture(t *testing.T) (recordsPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	day := func(d, hour int) time.Time { return time.Date(2026, 10, d, hour, 0, 0, 0, time.Local) }
	recs := []land.ReviewRecord{
		{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: land.VerdictRequestChanges, Agent: "opus", Findings: 2, Bar: 0.8, At: day(16, 10)},
		{BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.90, Verdict: land.VerdictApprove, Agent: "opus", Bar: 0.8, At: day(16, 11)},
		{BeadID: "gt-b", Rig: "longeye", Attempt: 1, Score: 0.85, Verdict: land.VerdictApprove, Agent: "sonnet", Bar: 0.8, At: day(17, 10)},
	}
	var lines []string
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	recordsPath = filepath.Join(dir, land.ReviewsFileName)
	if err := os.WriteFile(recordsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing the records: %v", err)
	}

	logPath = filepath.Join(dir, "daemon.log")
	log := strings.Join([]string{
		"2026/10/14 23:10:00 landing_worker: [land] gt-old: rejected (review): om requested changes (score 0.55, 3 finding(s))",
		"2026/10/14 23:40:00 landing_worker: [land] gt-old2: landed abc1234 on origin/main (patch-id def5678)",
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
		t.Fatalf("writing the log: %v", err)
	}
	return recordsPath, logPath
}

// runLandOmStatsOutput runs the command as the operator would, with the flags
// pinned and the output captured.
func runLandOmStatsOutput(t *testing.T, recordsPath, logPath, since string, asJSON bool) string {
	t.Helper()
	oldSince, oldRecords, oldLog, oldJSON := landOmStatsSince, landOmStatsRecords, landOmStatsLog, landOmStatsJSON
	t.Cleanup(func() {
		landOmStatsSince, landOmStatsRecords, landOmStatsLog, landOmStatsJSON = oldSince, oldRecords, oldLog, oldJSON
	})
	landOmStatsSince, landOmStatsRecords, landOmStatsLog, landOmStatsJSON = since, recordsPath, logPath, asJSON

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runLandOmStats(cmd, nil); err != nil {
		t.Fatalf("runLandOmStats: %v", err)
	}
	return out.String()
}

// TestLandOmStatsTextAndJSONAgree is gt-r05qz's command acceptance: the table
// carries the same days and figures the JSON does, and a fallback day is marked
// in both.
func TestLandOmStatsTextAndJSONAgree(t *testing.T) {
	recordsPath, logPath := omStatsFixture(t)

	text := runLandOmStatsOutput(t, recordsPath, logPath, "", false)
	for _, want := range []string{
		"om landings, all days",
		"2026-10-14*",
		"2026-10-16",
		"2026-10-17",
		"* filled from the daemon log: no review record for the day",
		"Rig",
		"Agent",
		"Rounds per bead: 1 took one, 1 two, 0 three or more",
		"Beads rejected most",
		"gt-a",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("table does not carry %q:\n%s", want, text)
		}
	}

	raw := runLandOmStatsOutput(t, recordsPath, logPath, "", true)
	var stats land.OmStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatalf("--json output does not decode: %v\n%s", err, raw)
	}
	if len(stats.Days) != 3 {
		t.Fatalf("days = %+v, want three", stats.Days)
	}
	want := []land.OmStatsDay{
		{Day: "2026-10-14", Approvals: 1, Rejections: 1, Rate: 0.5, MeanFindings: 3, MeanScore: 0.55, Fallback: true},
		{Day: "2026-10-16", Approvals: 1, Rejections: 1, Rate: 0.5, MeanFindings: 2, MeanScore: 0.40},
		{Day: "2026-10-17", Approvals: 1, Rejections: 0, Rate: 0},
	}
	for i, day := range want {
		got := stats.Days[i]
		if got.Day != day.Day || got.Approvals != day.Approvals || got.Rejections != day.Rejections || got.Fallback != day.Fallback {
			t.Errorf("day %d = %+v, want %+v", i, got, day)
		}
	}
	if len(stats.Rigs) != 2 || stats.Rigs[0].Name != "gastown" || stats.Rigs[1].Name != "longeye" {
		t.Errorf("rigs = %+v, want gastown then longeye", stats.Rigs)
	}
	if len(stats.Agents) != 2 || stats.Agents[0].Name != "opus" || stats.Agents[1].Name != "sonnet" {
		t.Errorf("agents = %+v, want opus then sonnet", stats.Agents)
	}
	if stats.Rounds != (land.OmStatsRounds{One: 1, Two: 1}) {
		t.Errorf("rounds = %+v, want one bead at two rounds", stats.Rounds)
	}
	if len(stats.TopBeads) != 1 || stats.TopBeads[0].Bead != "gt-a" || stats.TopBeads[0].Rejections != 1 {
		t.Errorf("top beads = %+v, want gt-a alone", stats.TopBeads)
	}

	// The same figures the JSON carries are the ones the table shows: the row
	// that opens with the day holds its approvals, rejections and rate.
	for _, day := range stats.Days {
		row := ""
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, day.Day) {
				row = line
				break
			}
		}
		if row == "" {
			t.Errorf("table lacks a row for %s", day.Day)
			continue
		}
		fields := strings.Fields(row)
		if len(fields) < 4 || fields[1] != strconv.Itoa(day.Approvals) || fields[2] != strconv.Itoa(day.Rejections) ||
			fields[3] != landOmStatsPercent(day.Rate) {
			t.Errorf("table row %q does not carry the JSON's %d approvals, %d rejections at %s",
				row, day.Approvals, day.Rejections, landOmStatsPercent(day.Rate))
		}
	}
}

// TestLandOmStatsSinceFiltersBothForms pins --since to the day tables and the
// JSON alike: nothing before the day is reported, including a fallback day.
func TestLandOmStatsSinceFiltersBothForms(t *testing.T) {
	recordsPath, logPath := omStatsFixture(t)

	text := runLandOmStatsOutput(t, recordsPath, logPath, "2026-10-17", false)
	if !strings.Contains(text, "om landings since 2026-10-17") {
		t.Errorf("table header does not name the range:\n%s", text)
	}
	for _, banned := range []string{"2026-10-14", "2026-10-16", "* filled from the daemon log"} {
		if strings.Contains(text, banned) {
			t.Errorf("table carries %q before --since:\n%s", banned, text)
		}
	}

	raw := runLandOmStatsOutput(t, recordsPath, logPath, "2026-10-17", true)
	var stats land.OmStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatalf("--json output does not decode: %v", err)
	}
	if len(stats.Days) != 1 || stats.Days[0].Day != "2026-10-17" {
		t.Errorf("days = %+v, want only 2026-10-17", stats.Days)
	}
	if len(stats.TopBeads) != 0 {
		t.Errorf("top beads = %+v, want none after --since", stats.TopBeads)
	}
}

// TestLandOmStatsWritesNothing: the command is a read. Every file under the
// fixture root has the same bytes, size and modification time after both
// forms run, and no file appeared.
func TestLandOmStatsWritesNothing(t *testing.T) {
	recordsPath, logPath := omStatsFixture(t)
	root := filepath.Dir(recordsPath)
	before := omStatsSnapshot(t, root)

	_ = runLandOmStatsOutput(t, recordsPath, logPath, "", false)
	_ = runLandOmStatsOutput(t, recordsPath, logPath, "2026-10-16", true)

	after := omStatsSnapshot(t, root)
	if len(before) != len(after) {
		t.Fatalf("file count %d -> %d: the command wrote a file", len(before), len(after))
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok || got != want {
			t.Errorf("%s changed: %q -> %q", name, want, got)
		}
	}
}

// omStatsSnapshot fingerprints every file under root: its size, mode,
// modification time and content hash.
func omStatsSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := fmt.Sprintf("%d %s %d", info.Size(), info.Mode(), info.ModTime().UnixNano())
		if !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry += " " + hex.EncodeToString(sum[:])
		}
		snap[name] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return snap
}

// TestLandOmStatsRejectsABadSince keeps a mistyped day from silently reporting
// nothing.
func TestLandOmStatsRejectsABadSince(t *testing.T) {
	for _, value := range []string{"2026-13-01", "16/10/2026", "yesterday"} {
		if _, err := parseLandOmStatsSince(value); err == nil {
			t.Errorf("parseLandOmStatsSince(%q) = nil error, want a refusal", value)
		}
	}
	if at, err := parseLandOmStatsSince(" 2026-10-16 "); err != nil || at.IsZero() {
		t.Errorf("parseLandOmStatsSince trimmed day = %v, %v", at, err)
	}
	if at, err := parseLandOmStatsSince(""); err != nil || !at.IsZero() {
		t.Errorf("parseLandOmStatsSince(empty) = %v, %v; want the zero time", at, err)
	}
}

// TestLandOmStatsMissingSources reads nothing rather than failing: a town that
// has not landed yet has neither file.
func TestLandOmStatsMissingSources(t *testing.T) {
	dir := t.TempDir()
	text := runLandOmStatsOutput(t, filepath.Join(dir, "no-records.jsonl"), filepath.Join(dir, "no-daemon.log"), "", false)
	if !strings.Contains(text, "no om verdicts in range") {
		t.Errorf("table = %q, want an empty report", text)
	}
	raw := runLandOmStatsOutput(t, filepath.Join(dir, "no-records.jsonl"), filepath.Join(dir, "no-daemon.log"), "", true)
	var stats land.OmStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatalf("--json output does not decode: %v", err)
	}
	if len(stats.Days) != 0 || len(stats.Rigs) != 0 || len(stats.Agents) != 0 || len(stats.TopBeads) != 0 {
		t.Errorf("empty sources read %+v, want no rows", stats)
	}
}
