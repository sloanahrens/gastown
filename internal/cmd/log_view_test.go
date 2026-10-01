package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gtevents "github.com/steveyegge/gastown/internal/events"
)

// seedEvent appends one event to a scratch town's events log.
func seedEvent(t *testing.T, townRoot, eventType, actor string, payload map[string]interface{}) {
	t.Helper()
	if err := gtevents.LogFeedTo(townRoot, eventType, actor, payload); err != nil {
		t.Fatalf("LogFeedTo(%s): %v", eventType, err)
	}
}

// TestFilterLogEventsDropsNonLifecycleTypes covers the boundary gt log draws
// now that the events log holds every type the town emits: the lifecycle view
// stays the lifecycle view (gt-i057g).
func TestFilterLogEventsDropsNonLifecycleTypes(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	seedEvent(t, townRoot, gtevents.TypeWake, "gastown/polecats/shale", gtevents.WakePayload("gastown", "gt-i057g"))
	seedEvent(t, townRoot, gtevents.TypeSlotWait, gtevents.ActorGt, nil)
	seedEvent(t, townRoot, gtevents.TypeSchedulerDispatch, "mayor/", nil)

	raw, err := gtevents.Read(townRoot)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := filterLogEvents(raw, logQuery{})
	if len(got) != 1 || got[0].Type != gtevents.TypeWake {
		t.Fatalf("filterLogEvents = %+v, want just the wake event", got)
	}
}

// TestFilterLogEventsAppliesTypeActorSinceAndTail exercises each narrowing gt
// log offers, since a reader trusts --type/--agent/--since to mean what they
// say.
func TestFilterLogEventsAppliesTypeActorSinceAndTail(t *testing.T) {
	t.Parallel()
	now := time.Now()
	raw := []gtevents.Event{
		{Timestamp: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339), Type: gtevents.TypeWake, Actor: "gastown/polecats/shale"},
		{Timestamp: now.Add(-90 * time.Minute).UTC().Format(time.RFC3339), Type: gtevents.TypeNudge, Actor: "mayor/"},
		{Timestamp: now.Add(-time.Minute).UTC().Format(time.RFC3339), Type: gtevents.TypeNudge, Actor: "gastown/crew/max"},
		{Timestamp: now.UTC().Format(time.RFC3339), Type: gtevents.TypeNudge, Actor: "gastown/crew/max"},
	}

	if got := filterLogEvents(raw, logQuery{Type: gtevents.TypeNudge}); len(got) != 3 {
		t.Errorf("--type nudge kept %d events, want 3", len(got))
	}
	if got := filterLogEvents(raw, logQuery{Actor: "gastown/"}); len(got) != 3 {
		t.Errorf("--agent gastown/ kept %d events, want 3", len(got))
	}
	if got := filterLogEvents(raw, logQuery{Actor: "gastown/crew/"}); len(got) != 2 {
		t.Errorf("--agent gastown/crew/ kept %d events, want 2", len(got))
	}
	if got := filterLogEvents(raw, logQuery{Since: now.Add(-time.Hour)}); len(got) != 2 {
		t.Errorf("--since 1h kept %d events, want 2", len(got))
	}
	if got := filterLogEvents(raw, logQuery{Tail: 2}); len(got) != 2 || got[1].Type != gtevents.TypeNudge {
		t.Errorf("-n 2 = %+v, want the 2 newest", got)
	}
}

// TestLogTypeAliasKeepsDocumentedFilterValues covers the flag the help text has
// always advertised: `--type crash` must still select crash records even though
// they are written as session_death (gt-i057g).
func TestLogTypeAliasKeepsDocumentedFilterValues(t *testing.T) {
	t.Parallel()
	raw := []gtevents.Event{
		{Type: gtevents.TypeSessionDeath, Actor: "gastown/polecats/shale"},
		{Type: gtevents.TypeWake, Actor: "gastown/polecats/shale"},
	}

	got := filterLogEvents(raw, logQuery{Type: "crash"})
	if len(got) != 1 || got[0].Type != gtevents.TypeSessionDeath {
		t.Fatalf("--type crash = %+v, want the session_death event", got)
	}
	got = filterLogEvents(raw, logQuery{Type: gtevents.TypeSessionDeath})
	if len(got) != 1 {
		t.Fatalf("--type session_death = %+v, want the session_death event", got)
	}
}

// TestRenderEventLineKeepsLifecyclePhrasing pins the operator-visible lines:
// the retired townlog's vocabulary, read from the events log (gt-i057g).
func TestRenderEventLineKeepsLifecyclePhrasing(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 1, 9, 44, 33, 0, time.Local).UTC().Format(time.RFC3339)

	cases := []struct {
		name string
		e    gtevents.Event
		want string
	}{
		{
			name: "wake",
			e:    gtevents.Event{Timestamp: ts, Type: gtevents.TypeWake, Actor: "gastown/shale", Payload: gtevents.WakePayload("gastown", "gt-i057g")},
			want: "[wake] gastown/shale resumed (gt-i057g)",
		},
		{
			name: "wake without context",
			e:    gtevents.Event{Timestamp: ts, Type: gtevents.TypeWake, Actor: "gastown/shale", Payload: gtevents.WakePayload("gastown", "")},
			want: "[wake] gastown/shale resumed",
		},
		{
			name: "kill",
			e:    gtevents.Event{Timestamp: ts, Type: gtevents.TypeKill, Actor: "gastown/shale", Payload: gtevents.KillPayload("gastown", "shale", "gt session stop")},
			want: "[kill] gastown/shale killed (gt session stop)",
		},
		{
			name: "nudge",
			e: gtevents.Event{Timestamp: ts, Type: gtevents.TypeNudge, Actor: "mayor/",
				Payload: gtevents.NudgePayload("gastown", "gastown/polecats/shale", "start work")},
			want: "[nudge] mayor/ nudged gastown/polecats/shale with \"start work\"",
		},
		{
			name: "handoff",
			e: gtevents.Event{Timestamp: ts, Type: gtevents.TypeHandoff, Actor: "gastown/crew/max",
				Payload: gtevents.HandoffPayload("picking up the sling rework", true)},
			want: "[handoff] gastown/crew/max handed off (picking up the sling rework)",
		},
		{
			name: "handoff nopersist",
			e: gtevents.Event{Timestamp: ts, Type: gtevents.TypeHandoffNoPersist, Actor: "gastown/crew/max",
				Payload: gtevents.HandoffFailedPayload("picking up the sling rework", errFixture)},
			want: "[handoff-NOPERSIST] gastown/crew/max handoff FAILED (picking up the sling rework): dolt is down",
		},
		{
			name: "done",
			e: gtevents.Event{Timestamp: ts, Type: gtevents.TypeDone, Actor: "gastown/polecats/shale",
				Payload: gtevents.DonePayload("gt-i057g", "polecat/shale/gt-i057g")},
			want: "[done] gastown/polecats/shale completed gt-i057g",
		},
		{
			name: "spawn",
			e: gtevents.Event{Timestamp: ts, Type: gtevents.TypeSpawn, Actor: gtevents.ActorGt,
				Payload: gtevents.SpawnPayload("gastown", "shale")},
			want: "[spawn] gt spawned for gastown/shale",
		},
		{
			name: "session death",
			e: gtevents.Event{Timestamp: ts, Type: gtevents.TypeSessionDeath, Actor: "gastown/polecats/shale",
				Payload: gtevents.SessionDeathPayload("gt-gastown-shale", "gastown/polecats/shale", "crashed with exit code 42", "gt log crash")},
			want: "[session_death] gastown/polecats/shale exited (crashed with exit code 42)",
		},
	}

	wantPrefix := time.Date(2026, 10, 1, 9, 44, 33, 0, time.Local).Format("2006-01-02 15:04:05")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderEventLine(tc.e)
			if !strings.HasPrefix(got, wantPrefix+" ") {
				t.Errorf("line %q does not start with the local timestamp %q", got, wantPrefix)
			}
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("line = %q, want it to end with %q", got, tc.want)
			}
		})
	}
}

// TestWriteLogEventsRendersTheEventsFile covers the whole gt log read path:
// what lands in .events.jsonl is what the operator sees.
func TestWriteLogEventsRendersTheEventsFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	seedEvent(t, townRoot, gtevents.TypeWake, "gastown/shale", gtevents.WakePayload("gastown", "gt-i057g"))
	seedEvent(t, townRoot, gtevents.TypeDone, "gastown/shale", gtevents.DonePayload("gt-i057g", "b"))

	raw, err := gtevents.Read(townRoot)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var buf strings.Builder
	writeLogEvents(&buf, filterLogEvents(raw, logQuery{}))

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("rendered %d lines, want 2: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "[wake] gastown/shale resumed (gt-i057g)") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "[done] gastown/shale completed gt-i057g") {
		t.Errorf("line 1 = %q", lines[1])
	}
}

// TestRenderRawEventLineClassifiesOneLineAtATime covers the follow path, where
// lines arrive from a tail rather than a query: each is classified on its own,
// and a non-event or a non-lifecycle event is passed over rather than printed.
func TestRenderRawEventLineClassifiesOneLineAtATime(t *testing.T) {
	t.Parallel()

	tailLines := []string{
		`{"ts":"2026-10-01T09:44:33Z","source":"gt","type":"slot_wait","actor":"gt","visibility":"both"}`,
		`{"ts":"2026-10-01T09:44:34Z","source":"gt","type":"wake","actor":"gastown/shale","payload":{"rig":"gastown"},"visibility":"feed"}`,
		`{"ts":"2026-10-01T09:44`,
		"",
	}

	var got []string
	for _, line := range tailLines {
		if rendered, ok := renderRawEventLine(line); ok {
			got = append(got, rendered)
		}
	}

	want := []string{time.Date(2026, 10, 1, 9, 44, 34, 0, time.UTC).Local().Format("2006-01-02 15:04:05") +
		" [wake] gastown/shale resumed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renderRawEventLine kept %q, want %q", got, want)
	}
}

// errFixture is the persistence failure the handoff-NOPERSIST rendering test
// carries through to the line.
var errFixture = errors.New("dolt is down")

// TestReadLogEventsMatchesTheEventsLog proves gt log no longer reads
// logs/town.log: the same events file the writers append to is the only input.
func TestReadLogEventsMatchesTheEventsLog(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	seedEvent(t, townRoot, gtevents.TypeKill, "gastown/shale", gtevents.KillPayload("gastown", "shale", "gt session stop"))

	if _, err := os.Stat(filepath.Join(townRoot, "logs", "town.log")); !os.IsNotExist(err) {
		t.Fatalf("logs/town.log exists after seeding events: %v", err)
	}

	raw, err := gtevents.Read(townRoot)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := filterLogEvents(raw, logQuery{Type: gtevents.TypeKill})
	if len(got) != 1 {
		t.Fatalf("gt log selected %d kill events, want 1", len(got))
	}
}
