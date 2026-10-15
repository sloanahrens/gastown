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

func TestAttentionTableOldestFirstHidesAcked(t *testing.T) {
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

	if err := ackAttention(town, "esc:hq-3", now); err != nil {
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

	err = ackAttention(town, "no:such:key", now)
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
