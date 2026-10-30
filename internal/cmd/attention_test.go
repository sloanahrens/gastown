package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// attentionTown is a fixture town root the workspace resolver accepts.
func attentionTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// attentionSet writes a state of three items and one ack: red-main oldest,
// then esc:hq-3, and stall:gastown/opal acked.
func attentionSet(t *testing.T, now time.Time) (town string, state attention.State) {
	t.Helper()
	town = attentionTown(t)
	state = attention.State{
		Updated: now,
		Items: []attention.Item{
			{Key: "esc:hq-3", Kind: attention.KindEscalation, Severity: attention.SeverityHigh, Summary: "escalation open", FirstSeen: now.Add(-10 * time.Minute), LastSeen: now},
			{Key: "red-main:gastown", Kind: attention.KindRedMain, Severity: attention.SeverityHigh, Rig: "gastown", Bead: "gt-abc", SHA: "6cf8b456", Summary: "main is red", FirstSeen: now.Add(-30 * time.Minute), LastSeen: now},
			{Key: "stall:gastown/opal", Kind: attention.KindPolecatStall, Severity: attention.SeverityLow, Rig: "gastown", Summary: "opal silent 12m", FirstSeen: now.Add(-5 * time.Minute), LastSeen: now},
		},
	}
	if err := attention.WriteState(town, state); err != nil {
		t.Fatal(err)
	}
	acks := attention.Acks{Acks: []attention.Ack{{Key: "stall:gastown/opal", At: now}}}
	if err := attention.WriteAcks(town, acks); err != nil {
		t.Fatal(err)
	}
	return town, state
}

// The queue is ranked by Jev's needs_human answer, highest first, with the
// answer drawn beside each item. A low score sinks the item without hiding it,
// and an item no answer names sorts last rather than leaving the table.
func TestAttentionTableOrdersByNeedsHumanScore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)
	writeAttentionScores(t, town, now)

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, true, false, view, now); err != nil {
		t.Fatalf("attentionReport: %v", err)
	}
	out := buf.String()

	// red-main scores 0.90, esc:hq-3 0.40, and stall:gastown/opal 0.20.
	want := []string{"red-main:gastown", "esc:hq-3", "stall:gastown/opal"}
	at := -1
	for _, key := range want {
		i := strings.Index(out, key)
		if i < 0 {
			t.Fatalf("the table dropped %q:\n%s", key, out)
		}
		if i < at {
			t.Fatalf("%q is out of the score order:\n%s", key, out)
		}
		at = i
	}
	for _, score := range []string{"0.90", "0.40", "0.20"} {
		if !strings.Contains(out, score) {
			t.Errorf("the NEEDS column has no %s:\n%s", score, out)
		}
	}
}

// writeAttentionScores writes answers for the three items attentionSet raises:
// red-main 0.90, esc:hq-3 0.40 and stall:gastown/opal 0.20, each keyed by the
// item's first_seen the way the Jev pass writes it.
func writeAttentionScores(t *testing.T, town string, now time.Time) {
	t.Helper()

	writeAttentionLines(t, town, []attentionAnswer{
		{Key: "red-main:gastown", FirstSeen: now.Add(-30 * time.Minute), NeedsHuman: 0.90},
		{Key: "esc:hq-3", FirstSeen: now.Add(-10 * time.Minute), NeedsHuman: 0.40},
		{Key: "stall:gastown/opal", FirstSeen: now.Add(-5 * time.Minute), NeedsHuman: 0.20},
	})
}

// writeAttentionLines appends answers to the town's answers log, one line each.
func writeAttentionLines(t *testing.T, town string, lines []attentionAnswer) {
	t.Helper()
	out := make([]any, len(lines))
	for i, l := range lines {
		out[i] = l.line()
	}
	appendJSONLines(t, attention.AnswersPath(town), out)
}

// writeAttentionLandings appends landing answers to the town's landing log.
func writeAttentionLandings(t *testing.T, town string, lines []attentionLanding) {
	t.Helper()
	out := make([]any, len(lines))
	for i, l := range lines {
		out[i] = l.line()
	}
	appendJSONLines(t, attention.LandingRiskPath(town), out)
}

// appendJSONLines appends one JSON line per value to path, creating the log's
// directory first.
func appendJSONLines(t *testing.T, path string, lines []any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// attentionLanding is one landing-log line as the battery writes it, for the
// fixtures: the queue reads the bead, the head and the risk answer from it.
type attentionLanding struct {
	Bead  string
	Head  string
	Score float64
}

func (l attentionLanding) line() map[string]any {
	return map[string]any{
		"ts":               time.Now(),
		"bead":             l.Bead,
		"rig":              "gastown",
		"head":             l.Head,
		"answers":          map[string]any{"risk": map[string]any{"score": l.Score, "confidence": 0.8}},
		"question_version": 1,
		"model":            "jev-test",
	}
}

// attentionAnswer is one answers-log line as the battery writes it, for the
// fixtures: a written log's shape is the battery's, and the queue reads only the
// key, the first_seen and the needs_human answer from it.
type attentionAnswer struct {
	Key        string
	FirstSeen  time.Time
	NeedsHuman float64
}

func (a attentionAnswer) line() map[string]any {
	return map[string]any{
		"ts":         a.FirstSeen.Add(time.Minute),
		"key":        a.Key,
		"first_seen": a.FirstSeen,
		"answers":    map[string]any{"needs_human": a.NeedsHuman},
	}
}

// An item Jev has not answered yet is last but still listed: the score orders
// the queue and clears nothing from it.
func TestAttentionTableKeepsAnItemWithNoAnswer(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)
	// Score only the oldest item; esc:hq-3 keeps no answer.
	dir := filepath.Join(town, ".runtime", "jev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(attentionAnswer{Key: "red-main:gastown",
		FirstSeen: now.Add(-30 * time.Minute), NeedsHuman: 0.10}.line())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attention.AnswersPath(town), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, false, false, view, now); err != nil {
		t.Fatalf("attentionReport: %v", err)
	}
	out := buf.String()
	for _, key := range []string{"red-main:gastown", "esc:hq-3"} {
		if !strings.Contains(out, key) {
			t.Errorf("the table dropped %q, which has no answer:\n%s", key, out)
		}
	}
	if red, esc := strings.Index(out, "red-main:gastown"), strings.Index(out, "esc:hq-3"); red > esc {
		t.Errorf("the answered item is below the unanswered one:\n%s", out)
	}
	// The NEEDS column is the third field: a value for the answered item and a
	// dash for the one Jev has not reached.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		case "red-main:gastown":
			if fields[2] != "0.10" {
				t.Errorf("the answered item reads NEEDS %q, want 0.10 in:\n%s", fields[2], line)
			}
		case "esc:hq-3":
			if fields[2] != "-" {
				t.Errorf("the unanswered item reads NEEDS %q, want a dash in:\n%s", fields[2], line)
			}
		}
	}
}

// A log the queue cannot read is named, and its items read "?" rather than "-":
// an unreadable answers log is not a queue Jev answered nothing about, and the
// table must not draw it as one (gt-b9j3o.2).
func TestAttentionTableNamesAnAnswersLogItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)

	// The log exists and is not a file, so it stats, opens, and cannot be read.
	if err := os.MkdirAll(attention.AnswersPath(town), 0o755); err != nil { //nolint:gosec // G301: a test's own temp town
		t.Fatal(err)
	}

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	if view.ScoresErr == nil {
		t.Fatalf("an unreadable answers log reported no error, scores %v", view.Scores)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, false, false, view, now); err != nil {
		t.Fatalf("attentionReport: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "NEEDS unavailable") {
		t.Errorf("the table did not name the answers log it could not read:\n%s", out)
	}
	// Every item still stands, and none of them claims Jev answered it.
	for _, key := range []string{"red-main:gastown", "esc:hq-3"} {
		found := false
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[1] != key {
				continue
			}
			found = true
			if fields[2] != "?" {
				t.Errorf("the item %q reads NEEDS %q, want a question mark in:\n%s", key, fields[2], line)
			}
		}
		if !found {
			t.Errorf("the table dropped %q whose scores could not be read:\n%s", key, out)
		}
	}
}

// The risk-path items are ranked by the landing battery's risk answer, highest
// first, in the places the needs_human score gave them: the other kinds keep
// their order and nothing leaves the table (gt-n8tmf).
func TestAttentionTableOrdersRiskPathByLandingRisk(t *testing.T) {
	t.Parallel()
	now := time.Now()

	const (
		headA = "6cf8b456abcd0123456789abcdef0123456789ab"
		headB = "0123456789abcdef0123456789abcdef01234567"
		headC = "fedcba9876543210fedcba9876543210fedcba98"
	)
	keyA, keyB, keyC := "risk:gt-a:"+headA[:12], "risk:gt-b:"+headB[:12], "risk:gt-c:"+headC[:12]
	town := attentionTown(t)
	state := attention.State{
		Updated: now,
		Items: []attention.Item{
			{Key: keyA, Kind: attention.KindRiskPath, Severity: attention.SeverityLow, Rig: "gastown", Bead: "gt-a", SHA: headA, Summary: "gt-a landed touching cmd", FirstSeen: now.Add(-30 * time.Minute), LastSeen: now},
			{Key: keyB, Kind: attention.KindRiskPath, Severity: attention.SeverityLow, Rig: "gastown", Bead: "gt-b", SHA: headB, Summary: "gt-b landed touching cmd", FirstSeen: now.Add(-20 * time.Minute), LastSeen: now},
			{Key: keyC, Kind: attention.KindRiskPath, Severity: attention.SeverityLow, Rig: "gastown", Bead: "gt-c", SHA: headC, Summary: "gt-c landed touching cmd", FirstSeen: now.Add(-10 * time.Minute), LastSeen: now},
			{Key: "red-main:gastown", Kind: attention.KindRedMain, Severity: attention.SeverityHigh, Rig: "gastown", Bead: "gt-abc", Summary: "main is red", FirstSeen: now.Add(-40 * time.Minute), LastSeen: now},
		},
	}
	if err := attention.WriteState(town, state); err != nil {
		t.Fatal(err)
	}
	// All four items share one needs_human answer, so today's order is by age
	// and only the landing risk can move the reviews.
	writeAttentionLines(t, town, []attentionAnswer{
		{Key: keyA, FirstSeen: now.Add(-30 * time.Minute), NeedsHuman: 0.90},
		{Key: keyB, FirstSeen: now.Add(-20 * time.Minute), NeedsHuman: 0.90},
		{Key: keyC, FirstSeen: now.Add(-10 * time.Minute), NeedsHuman: 0.90},
		{Key: "red-main:gastown", FirstSeen: now.Add(-40 * time.Minute), NeedsHuman: 0.90},
	})
	writeAttentionLandings(t, town, []attentionLanding{
		{Bead: "gt-a", Head: headA, Score: 1},
		{Bead: "gt-b", Head: headB, Score: 3},
		{Bead: "gt-c", Head: headC, Score: 2},
	})

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	if view.RisksErr != nil {
		t.Fatalf("the landing answers: %v", view.RisksErr)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, true, false, view, now); err != nil {
		t.Fatalf("attentionReport: %v", err)
	}
	out := buf.String()

	// red-main is oldest and keeps the lead; the reviews follow in landing risk
	// order, gt-b (3) first and gt-a (1) last.
	want := []string{"red-main:gastown", keyB, keyC, keyA}
	at := -1
	for _, key := range want {
		i := strings.Index(out, key)
		if i < 0 {
			t.Fatalf("the table dropped %q:\n%s", key, out)
		}
		if i < at {
			t.Fatalf("%q is out of the landing risk order:\n%s", key, out)
		}
		at = i
	}
}

// A landing log the queue cannot read is named, and the table still prints: the
// order it keeps is the last answers read, not this read's (gt-n8tmf).
func TestAttentionTableNamesALandingLogItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)

	// The log exists and is not a file, so it stats, opens, and cannot be read.
	if err := os.MkdirAll(attention.LandingRiskPath(town), 0o755); err != nil { //nolint:gosec // G301: a test's own temp town
		t.Fatal(err)
	}

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	if view.RisksErr == nil {
		t.Fatalf("an unreadable landing log reported no error, answers %v", view.Risks)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, false, false, view, now); err != nil {
		t.Fatalf("attentionReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "RISK unavailable") {
		t.Errorf("the table did not name the landing log it could not read:\n%s", out)
	}
	for _, key := range []string{"red-main:gastown", "esc:hq-3"} {
		if !strings.Contains(out, key) {
			t.Errorf("the table dropped %q whose landing answers could not be read:\n%s", key, out)
		}
	}
}

func TestAttentionTableHidesAckedAndKeepsArrivalWithoutAnswers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, false, false, view, now); err != nil {
		t.Fatalf("attentionReport: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"KIND", "KEY", "BEAD/SHA", "AGE", "SUMMARY"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing header %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stall:gastown/opal") {
		t.Errorf("acked item shown without --all:\n%s", out)
	}
	red := strings.Index(out, "red-main:gastown")
	esc := strings.Index(out, "esc:hq-3")
	if red < 0 || esc < 0 {
		t.Fatalf("items missing:\n%s", out)
	}
	if red > esc {
		t.Errorf("items not oldest first (red-main at %d, esc:hq-3 at %d):\n%s", red, esc, out)
	}
	if !strings.Contains(out, "gt-abc") {
		t.Errorf("BEAD/SHA column missing the bead:\n%s", out)
	}

	var all bytes.Buffer
	if err := attentionReport(&all, true, false, view, now); err != nil {
		t.Fatalf("attentionReport --all: %v", err)
	}
	if !strings.Contains(all.String(), "stall:gastown/opal") {
		t.Errorf("--all did not show the acked item:\n%s", all.String())
	}
}

// A sweep item names no bead, so its sha is what the BEAD/SHA column shows. For
// a verdict a Partial cycle carried forward that is an earlier tip than the
// record's, and the column is where an operator reads it (gt-euxf1).
func TestAttentionBeadOrSHAShowsASweepItemsTip(t *testing.T) {
	t.Parallel()
	it := attention.Item{
		Key: "sweep:gastown:integration", Kind: attention.KindTierSweepRed,
		Severity: attention.SeverityHigh, Rig: "gastown", SHA: "tipa0002",
		Summary: "tier sweep gastown integration RED",
	}
	if got := beadOrSHA(it); got != "tipa0002" {
		t.Errorf("beadOrSHA = %q, want the item's sha", got)
	}
}

func TestAttentionJSONPrintsState(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, state := attentionSet(t, now)

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	var buf bytes.Buffer
	if err := attentionReport(&buf, false, true, view, now); err != nil {
		t.Fatalf("attentionReport --json: %v", err)
	}
	var got attention.State
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not state JSON: %v\n%s", err, buf.String())
	}
	if len(got.Items) != len(state.Items) {
		t.Errorf("--json items = %d, want %d", len(got.Items), len(state.Items))
	}
}

func TestAttentionAckWritesAcksAndUnknownKeyExits1(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)

	if err := ackAttention(town, "esc:hq-3", now, attention.DispositionNeededHuman); err != nil {
		t.Fatalf("ackAttention: %v", err)
	}
	acks, err := attention.ReadAcks(town)
	if err != nil {
		t.Fatal(err)
	}
	if !attention.AckedKey(acks, "esc:hq-3") {
		t.Errorf("ack not written: %+v", acks.Acks)
	}
	if !attention.AckedKey(acks, "stall:gastown/opal") {
		t.Errorf("existing ack lost: %+v", acks.Acks)
	}

	err = ackAttention(town, "no:such:key", now, attention.DispositionNeededHuman)
	var coded *ExitCodeError
	if !errors.As(err, &coded) || coded.Code != 1 {
		t.Fatalf("unknown key error = %v, want ExitCodeError code 1", err)
	}
}

func TestAttentionStaleExits3(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town := attentionTown(t)
	if err := attention.WriteState(town, attention.State{
		Updated: now.Add(-16 * time.Minute),
		Items:   []attention.Item{{Key: "esc:hq-3", Kind: attention.KindEscalation, Severity: attention.SeverityHigh, Summary: "escalation open", FirstSeen: now.Add(-20 * time.Minute), LastSeen: now.Add(-16 * time.Minute)}},
	}); err != nil {
		t.Fatal(err)
	}

	view, err := buildAttention(town, now)
	if err != nil {
		t.Fatalf("buildAttention: %v", err)
	}
	if !view.Stale {
		t.Fatal("a 16-minute-old state read as fresh")
	}
	var buf bytes.Buffer
	err = attentionReport(&buf, false, false, view, now)
	code, ok := IsSilentExit(err)
	if !ok || code != 3 {
		t.Fatalf("stale report error = %v, want silent exit 3", err)
	}
	if !strings.Contains(buf.String(), "STALE") {
		t.Errorf("stale output missing the STALE header:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "daemon has not written attention state since") {
		t.Errorf("stale output missing the since phrase:\n%s", buf.String())
	}
}

// followHarness runs attentionFollowEvents against a town and steps it one
// poll at a time.
type followHarness struct {
	t     *testing.T
	town  string
	buf   *bytes.Buffer
	ticks chan time.Time
	done  chan error
	stop  context.CancelFunc
}

func startFollow(t *testing.T, town string, opts followOptions) *followHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &followHarness{t: t, town: town, buf: &bytes.Buffer{}, ticks: make(chan time.Time), done: make(chan error, 1), stop: cancel}
	go func() { h.done <- attentionFollowEvents(ctx, h.buf, town, h.ticks, opts) }()
	t.Cleanup(cancel)
	return h
}

// poll steps the follower once and waits for that poll to finish: the second
// send only returns after the loop has gone back to select.
func (h *followHarness) poll() {
	h.t.Helper()
	now := time.Now()
	h.ticks <- now
	h.ticks <- now
}

// finish stops the follower and returns what it printed.
func (h *followHarness) finish() string {
	h.t.Helper()
	h.stop()
	if err := <-h.done; err != nil {
		h.t.Fatalf("attentionFollowEvents: %v", err)
	}
	return h.buf.String()
}

func appendAttention(t *testing.T, town string, evs ...attention.Event) {
	t.Helper()
	if err := attention.AppendEvents(town, evs); err != nil {
		t.Fatal(err)
	}
}

func raised(rule, key, text string) attention.Event {
	f, _ := attention.Flags(rule)
	return attention.Event{TS: time.Now(), Class: attention.KindRedMain, Severity: attention.SeverityHigh, Text: text, Key: key,
		State: attention.EventNew, Rule: rule, Handled: f.Handled, Wake: f.Wake}
}

func TestAttentionFollowPrintsOnlyNewEventsAsRuleLines(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	appendAttention(t, town, raised(attention.RuleEscalation, "esc:old", "before start"))
	h := startFollow(t, town, followOptions{})
	h.poll() // the follower has opened the tail before it receives a tick

	red := raised(attention.RuleRedMain, "red-main:gastown", "main is red")
	cleared := raised(attention.RuleEscalation, "esc:hq-3", "escalation open")
	cleared.State, cleared.Handled, cleared.Wake = attention.EventCleared, attention.HandledInfo, false
	info := raised(attention.RuleRiskPath, "risk:gt-1:abc", "landing touched a risk path")
	appendAttention(t, town, red, cleared, info)
	h.poll()

	lines := strings.Split(strings.TrimSpace(h.finish()), "\n")
	want := []string{
		red.TS.Local().Format("15:04:05") + " main.red main is red",
		cleared.TS.Local().Format("15:04:05") + " escalation.open cleared: escalation open",
		info.TS.Local().Format("15:04:05") + " landing.risk_path landing touched a risk path",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("follow lines:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestAttentionFollowWakeOnlyFilters(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	h := startFollow(t, town, followOptions{wakeOnly: true})
	h.poll()

	cleared := raised(attention.RuleRedMain, "red-main:gastown", "main is red")
	cleared.State, cleared.Handled, cleared.Wake = attention.EventCleared, attention.HandledInfo, false
	legacy := attention.Event{TS: time.Now(), Class: attention.KindBDSlow, Severity: attention.SeverityLow, Text: "legacy line", Key: "bd-slow", State: attention.EventNew}
	appendAttention(t, town,
		raised(attention.RuleHostDiskWarn, "host.disk_warn", "disk 82% used"),
		raised(attention.RuleHostDockerDown, "host.docker_stack_down", "1 of 5 stack containers not running"),
		raised(attention.RuleOverrunStopped, "overrun:x", "stopped a polecat"),
		cleared, legacy)
	h.poll()

	out := h.finish()
	if !strings.Contains(out, "host.docker_stack_down 1 of 5 stack containers not running") {
		t.Errorf("wake-only dropped the wake event:\n%s", out)
	}
	for _, drop := range []string{"disk 82%", "stopped a polecat", "cleared", "legacy line"} {
		if strings.Contains(out, drop) {
			t.Errorf("wake-only printed %q:\n%s", drop, out)
		}
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 {
		t.Errorf("wake-only printed %d lines, want 1:\n%s", n, out)
	}
}

func TestAttentionFollowJSONPrintsTheLine(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	h := startFollow(t, town, followOptions{wakeOnly: true, asJSON: true})
	h.poll()
	appendAttention(t, town, raised(attention.RuleLandingStuck, "landing-stuck:gastown:gt-abc", "gt-abc in flight 40m"))
	h.poll()

	var e attention.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(h.finish())), &e); err != nil {
		t.Fatalf("--json line is not an event: %v", err)
	}
	if e.Rule != attention.RuleLandingStuck || !e.Wake || e.Handled != attention.HandledOperatorAction {
		t.Errorf("event = %+v", e)
	}
}

func TestAttentionFollowSurvivesRotation(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	appendAttention(t, town, raised(attention.RuleRedMain, "red-main:a", "already there"))
	h := startFollow(t, town, followOptions{wakeOnly: true})
	h.poll()

	// The writer's rotation: events.jsonl moves to events.jsonl.1 and a fresh
	// file takes its place.
	if err := os.Rename(attention.EventsPath(town), attention.RotatedEventsPath(town)); err != nil {
		t.Fatal(err)
	}
	appendAttention(t, town, raised(attention.RuleEscalation, "esc:hq-9", "after rotation"))
	h.poll()

	out := h.finish()
	if !strings.Contains(out, "escalation.open after rotation") {
		t.Errorf("follow went deaf after rotation:\n%s", out)
	}
	if strings.Contains(out, "already there") {
		t.Errorf("follow replayed pre-start history after rotation:\n%s", out)
	}
}

func TestAttentionFollowSinceReplaysRecentHistoryOnly(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	now := time.Now()
	old := raised(attention.RuleRedMain, "red-main:old", "two hours ago")
	old.TS = now.Add(-2 * time.Hour)
	recent := raised(attention.RuleEscalation, "esc:hq-1", "ten minutes ago")
	recent.TS = now.Add(-10 * time.Minute)
	appendAttention(t, town, old, recent)

	h := startFollow(t, town, followOptions{since: time.Hour, now: now})
	h.poll()
	appendAttention(t, town, raised(attention.RuleBDSlow, "bd-slow", "bd read took 6s"))
	h.poll()

	out := h.finish()
	if strings.Contains(out, "two hours ago") {
		t.Errorf("--since replayed an event older than the window:\n%s", out)
	}
	if strings.Index(out, "ten minutes ago") < 0 || strings.Index(out, "bd read took 6s") < strings.Index(out, "ten minutes ago") {
		t.Errorf("--since output out of order or missing lines:\n%s", out)
	}
}

func TestAttentionReplayIncludesRotatedFile(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	now := time.Now()
	before := raised(attention.RuleRedMain, "red-main:before", "before rotation")
	before.TS = now.Add(-20 * time.Minute)
	appendAttention(t, town, before)

	// The writer's rotation: the line above moves to events.jsonl.1 and the
	// next line lands in a fresh events.jsonl.
	if err := os.Rename(attention.EventsPath(town), attention.RotatedEventsPath(town)); err != nil {
		t.Fatal(err)
	}
	after := raised(attention.RuleEscalation, "esc:hq-2", "after rotation")
	after.TS = now.Add(-5 * time.Minute)
	appendAttention(t, town, after)

	h := startFollow(t, town, followOptions{since: time.Hour, now: now})
	h.poll()
	out := h.finish()

	if !strings.Contains(out, "before rotation") {
		t.Errorf("--since skipped the event in events.jsonl.1:\n%s", out)
	}
	if !strings.Contains(out, "after rotation") {
		t.Errorf("--since skipped the event in events.jsonl:\n%s", out)
	}
	if strings.Index(out, "before rotation") > strings.Index(out, "after rotation") {
		t.Errorf("--since replayed the rotated file out of order:\n%s", out)
	}
}

func TestAttentionFollowFlagsNeedFollow(t *testing.T) {
	t.Parallel()
	if err := checkFollowFlags(false, true, 0); err == nil || !strings.Contains(err.Error(), "--follow") {
		t.Errorf("--wake-only without --follow = %v, want an error naming --follow", err)
	}
	if err := checkFollowFlags(false, false, time.Hour); err == nil {
		t.Error("--since without --follow = nil, want an error")
	}
	if err := checkFollowFlags(true, true, time.Hour); err != nil {
		t.Errorf("--follow with both = %v, want nil", err)
	}
	if err := checkFollowFlags(false, false, 0); err != nil {
		t.Errorf("no follow flags = %v, want nil", err)
	}
}

// The ack leaves a line in acks-history.jsonl that is still there once the
// item has cleared and the daemon's prune has dropped the ack.
func TestAttentionAckAppendsHistoryThatSurvivesTheItemClearing(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, state := attentionSet(t, now)

	if err := ackAttention(town, "red-main:gastown", now, attention.DispositionNoise); err != nil {
		t.Fatalf("ackAttention: %v", err)
	}
	// The item clears: the daemon writes a state without it and prunes.
	cleared := attention.State{Updated: now, Items: state.Items[:1]}
	if err := attention.WriteState(town, cleared); err != nil {
		t.Fatal(err)
	}
	if err := attention.PruneAcks(town, cleared); err != nil {
		t.Fatal(err)
	}

	hist, err := attention.ReadAcksHistory(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("history = %+v, want one line", hist)
	}
	h := hist[0]
	if h.Key != "red-main:gastown" || h.Kind != attention.KindRedMain || h.Rule != attention.RuleRedMain || !h.TS.Equal(now) {
		t.Errorf("history line = %+v, want key red-main:gastown, kind red-main, rule %s at %v", h, attention.RuleRedMain, now)
	}
	if h.Disposition != attention.DispositionNoise {
		t.Errorf("history disposition = %q, want %q", h.Disposition, attention.DispositionNoise)
	}

	// A refused ack of an unknown key leaves no line.
	if err := ackAttention(town, "no:such:key", now, attention.DispositionNeededHuman); err == nil {
		t.Fatal("unknown key acked")
	}
	if hist, _ = attention.ReadAcksHistory(town); len(hist) != 1 {
		t.Errorf("history after refused ack = %+v, want still one line", hist)
	}
}

// gt attention ack requires --disposition, and the one it is given is written
// into acks-history.jsonl. ParseDisposition's rejection of other values is
// tested in internal/attention.
func TestAttentionAckDispositionFlag(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)

	// The flag is required: cobra refuses the run before ackAttention sees it.
	if err := attentionAckCmd.ValidateRequiredFlags(); err == nil || !strings.Contains(err.Error(), "disposition") {
		t.Fatalf("ack without --disposition = %v, want an error naming the flag", err)
	}

	if err := ackAttention(town, "esc:hq-3", now, attention.DispositionFixedByOverseer); err != nil {
		t.Fatalf("ackAttention: %v", err)
	}
	hist, err := attention.ReadAcksHistory(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Disposition != attention.DispositionFixedByOverseer {
		t.Fatalf("history = %+v, want one line with disposition fixed-by-overseer", hist)
	}
}

// The help text counts the queue's files and names the rotation size from the
// code, so adding a file or changing EventsMaxBytes shows up here.
func TestAttentionHelpNamesFilesAndRotationSize(t *testing.T) {
	t.Parallel()
	long := attentionCmd.Long
	files := []string{"state.json", "events.jsonl", "acks.json", "acks-history.jsonl"}
	for _, f := range files {
		if !strings.Contains(long, "  "+f+" ") {
			t.Errorf("help does not list %s", f)
		}
	}
	if !strings.Contains(long, "The queue is four files under") {
		t.Errorf("help does not say the queue is %d files", len(files))
	}
	if !strings.Contains(long, "events.jsonl.1 at\n                 "+attentionRotateSize+".") {
		t.Errorf("help does not give events.jsonl's rotation size %q", attentionRotateSize)
	}
	if !strings.Contains(long, "acks-history.jsonl.1 at\n                 "+attentionRotateSize+".") {
		t.Errorf("help does not give acks-history.jsonl's rotation size %q", attentionRotateSize)
	}
	if want := "1 MB"; attentionRotateSize != want {
		t.Errorf("attentionRotateSize = %q, want %q for EventsMaxBytes %d", attentionRotateSize, want, attention.EventsMaxBytes)
	}
}

// ackAttentionWithDisposition, which runAttentionAck calls with the flag value,
// refuses a missing or unknown disposition with an error that lists the valid
// ones, and neither acks.json nor acks-history.jsonl changes.
func TestAckAttentionWithDispositionRefusesABadOne(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town, _ := attentionSet(t, now)
	wantAcks, err := attention.ReadAcks(town)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, disposition string }{
		{"missing", ""},
		{"unknown value", "whatever"},
		{"wrong case", "Noise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := ackAttentionWithDisposition(&out, town, "esc:hq-3", tc.disposition, now)
			if err == nil || !strings.Contains(err.Error(), "disposition must be needed-human, fixed-by-overseer or noise") {
				t.Fatalf("disposition %q: err = %v, want the disposition error", tc.disposition, err)
			}
			if out.Len() != 0 {
				t.Errorf("a refused ack printed %q", out.String())
			}
			acks, err := attention.ReadAcks(town)
			if err != nil {
				t.Fatal(err)
			}
			if len(acks.Acks) != len(wantAcks.Acks) {
				t.Errorf("acks = %+v, want unchanged %+v", acks, wantAcks)
			}
			if hist, _ := attention.ReadAcksHistory(town); len(hist) != 0 {
				t.Errorf("history = %+v, want none", hist)
			}
		})
	}

	var out bytes.Buffer
	if err := ackAttentionWithDisposition(&out, town, "esc:hq-3", "noise", now); err != nil {
		t.Fatalf("a valid disposition: %v", err)
	}
	if want := "acked esc:hq-3 (noise)\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
	hist, err := attention.ReadAcksHistory(town)
	if err != nil || len(hist) != 1 || hist[0].Disposition != attention.DispositionNoise {
		t.Errorf("history = %+v, %v, want one noise line", hist, err)
	}
}

// The help text tells a caller written before --disposition was required what
// to change.
func TestAttentionAckHelpNamesTheRequiredFlag(t *testing.T) {
	t.Parallel()
	long := attentionAckCmd.Long
	for _, want := range []string{"--disposition is required", "must add the flag"} {
		if !strings.Contains(long, want) {
			t.Errorf("ack help does not contain %q", want)
		}
	}
}

// The jev-replay subcommand is retired (gt-3ey5j): the attention battery and
// its scorecard adapter replaced the three-way triage it replayed, so
// `gt attention jev-replay` is an unknown command.
func TestAttentionJevReplayIsRetired(t *testing.T) {
	t.Parallel()
	for _, c := range attentionCmd.Commands() {
		if c.Name() == "jev-replay" {
			t.Errorf("gt attention still registers jev-replay")
		}
	}
}
