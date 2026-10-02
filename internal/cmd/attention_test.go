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

func TestAttentionFollowStreamsNewAndCleared(t *testing.T) {
	t.Parallel()
	town := attentionTown(t)
	now := time.Now()
	if err := attention.AppendEvents(town, []attention.Event{
		{TS: now, Class: attention.KindEscalation, Severity: attention.SeverityHigh, Text: "before start", Key: "esc:old", State: attention.EventNew},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- attentionFollowEvents(ctx, &buf, town, ticks) }()

	// The first tick is a barrier: the follower has snapshotted the count
	// before it receives, so anything appended after it is new.
	ticks <- now
	if err := attention.AppendEvents(town, []attention.Event{
		{TS: now, Class: attention.KindRedMain, Severity: attention.SeverityHigh, Text: "main is red", Key: "red-main:gastown", State: attention.EventNew},
	}); err != nil {
		t.Fatal(err)
	}
	if err := attention.AppendEvents(town, []attention.Event{
		{TS: now, Class: attention.KindEscalation, Severity: attention.SeverityHigh, Text: "escalation open", Key: "esc:hq-3", State: attention.EventCleared},
	}); err != nil {
		t.Fatal(err)
	}
	ticks <- now
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("attentionFollowEvents: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "before start") {
		t.Errorf("follow printed a pre-start line:\n%s", out)
	}
	if !strings.Contains(out, "+ ") || !strings.Contains(out, "red-main:gastown") {
		t.Errorf("follow missing the new line:\n%s", out)
	}
	if !strings.Contains(out, "- ") || !strings.Contains(out, "esc:hq-3") {
		t.Errorf("follow missing the cleared line:\n%s", out)
	}
}
