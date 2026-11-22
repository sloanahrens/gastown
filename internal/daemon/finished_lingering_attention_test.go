package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// seat.finished_lingering (gt-klpqb3): a seat whose last turn ended and whose
// bead is closed, still in the walk past the linger bound, is a finished seat
// nobody ended.

// finishedSeat is one seat the walk lists: its bare name, the work bead its
// record names, that bead's status, the agent state, and how long ago the
// agent bead was last written (the done time the rule ages from).
type finishedSeat struct {
	name   string
	bead   string
	status string
	state  string
	age    time.Duration
}

// finishedSeatTown points the walk at seats and answers their agent and work
// bead reads from memory, dated against keptSeatAttentionNow. It returns the
// reader so a test can break one read and keep the rest.
func finishedSeatTown(t *testing.T, f *attentionFixture, seats ...finishedSeat) keptSeatHookReader {
	t.Helper()
	r := keptSeatHookReader{issues: map[string]*beads.Issue{}}
	var walk []townhealth.Seat
	for _, s := range seats {
		id := keptSeatAgentID(f, s.name)
		r.issues[id] = &beads.Issue{
			ID:          id,
			UpdatedAt:   keptSeatAttentionNow.Add(-s.age).Format(time.RFC3339),
			Description: "agent_state: " + s.state + "\nlast_source_issue: " + s.bead,
		}
		if s.bead != "" {
			r.issues[s.bead] = &beads.Issue{ID: s.bead, Status: s.status}
		}
		walk = append(walk, townhealth.Seat{Rig: attentionRig, Name: "polecat/" + s.name, Run: true})
	}
	f.d.openWorkBeads = r.open
	f.src.seats = func() ([]townhealth.Seat, error) { return walk, nil }
	return r
}

// A done seat whose bead is closed raises one item once it has lingered past
// thirty minutes, naming the seat, the bead and the age; the same seat at
// twenty-nine minutes raises none.
func TestSeatFinishedLingeringRaisesAfterTheWindow(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
		finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusClosed), state: "done", age: 29 * time.Minute},
	)
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 29m seat has not lingered past the bound)", items)
	}
	it := items[0]
	if it.Key != "seat-finished:gastown/capable" || it.Kind != attention.KindSeatFinishedLingering ||
		it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.Bead != "gt-abc12" {
		t.Errorf("item = %+v, want a low seat-finished item on gastown/capable and gt-abc12", it)
	}
	if want := "gastown/capable done 31m on gt-abc12"; it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}
}

// A seat that ended with a DEFERRED or ESCALATED exit writes agent_state
// stuck, not done (done.DoneTurnState), so a stuck seat whose bead is closed
// and whose record has not been written for the window is the same finished
// seat nobody ended: it raises one item whose summary names the state it
// judged. A stuck seat whose bead is still open is rework, and a working seat
// is mid-turn, so neither raises.
func TestSeatFinishedLingeringRaisesForAStuckSeat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		state   string
		status  beads.IssueStatus
		raises  bool
		summary string
	}{
		{
			name:    "stuck with a closed bead raises",
			state:   "stuck",
			status:  beads.StatusClosed,
			raises:  true,
			summary: "gastown/capable stuck 31m on gt-abc12",
		},
		{name: "stuck with an open bead does not", state: "stuck", status: beads.StatusOpen},
		{name: "working with a closed bead does not", state: "working", status: beads.StatusClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			finishedSeatTown(t, f,
				finishedSeat{name: "capable", bead: "gt-abc12", status: string(tc.status), state: tc.state, age: 31 * time.Minute},
			)
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !tc.raises {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none for a %s seat with a %s bead", items, tc.state, tc.status)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one for a stuck seat whose bead is closed", items)
			}
			it := items[0]
			if it.Key != "seat-finished:gastown/capable" || it.Kind != attention.KindSeatFinishedLingering ||
				it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.Bead != "gt-abc12" {
				t.Errorf("item = %+v, want a low seat-finished item on gastown/capable and gt-abc12", it)
			}
			if it.Summary != tc.summary {
				t.Errorf("summary = %q, want %q", it.Summary, tc.summary)
			}
		})
	}
}

// A parked seat whose bead is closed is the same finished seat nobody ended.
// The overrun guard parks a seat by writing a held intent record (desired park
// or frozen) and leaves its agent state as the turn left it — the keeper of
// gt-sof0lm was idle — so the state alone never says done, and the rule judges
// a held seat whatever its state names. A parked seat whose bead is open is
// rework, and an idle seat nothing holds is a free seat the walk has no reason
// to judge: neither raises.
func TestSeatFinishedLingeringRaisesForAParkedSeat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		held    bool
		state   string
		status  beads.IssueStatus
		raises  bool
		summary string
	}{
		{
			name:    "parked with a closed bead raises",
			held:    true,
			state:   "idle",
			status:  beads.StatusClosed,
			raises:  true,
			summary: "gastown/capable parked 31m on gt-abc12",
		},
		{name: "parked with an open bead does not", held: true, state: "idle", status: beads.StatusOpen},
		{name: "an unheld idle seat with a closed bead does not", held: false, state: "idle", status: beads.StatusClosed},
		{name: "a held done seat raises as parked", held: true, state: "done", status: beads.StatusClosed, raises: true, summary: "gastown/capable parked 31m on gt-abc12"},
		{name: "a held stuck seat raises as parked", held: true, state: "stuck", status: beads.StatusClosed, raises: true, summary: "gastown/capable parked 31m on gt-abc12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			finishedSeatTown(t, f,
				finishedSeat{name: "capable", bead: "gt-abc12", status: string(tc.status), state: tc.state, age: 31 * time.Minute},
			)
			if tc.held {
				seat := intent.Seat{Rig: attentionRig, Role: constants.RolePolecat, Name: "capable"}
				writeJSONFile(t, seat.Path(f.d.config.TownRoot), intent.Record{Desired: intent.DesiredPark})
			}
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !tc.raises {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none for %s", items, tc.name)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one for a parked seat whose bead is closed", items)
			}
			it := items[0]
			if it.Key != "seat-finished:gastown/capable" || it.Kind != attention.KindSeatFinishedLingering ||
				it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.Bead != "gt-abc12" {
				t.Errorf("item = %+v, want a low seat-finished item on gastown/capable and gt-abc12", it)
			}
			if it.Summary != tc.summary {
				t.Errorf("summary = %q, want %q", it.Summary, tc.summary)
			}
		})
	}
}

// gt-s4ywbt: an unreadable intent record skips that seat alone.
func TestSeatFinishedLingeringUnreadableIntentSkipsThatSeat(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "idle", age: 31 * time.Minute},
		finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusClosed), state: "idle", age: 31 * time.Minute},
	)
	broken := intent.Seat{Rig: attentionRig, Role: constants.RolePolecat, Name: "capable"}
	if err := os.MkdirAll(filepath.Dir(broken.Path(f.d.config.TownRoot)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken.Path(f.d.config.TownRoot), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, intent.Seat{Rig: attentionRig, Role: constants.RolePolecat, Name: "nux"}.Path(f.d.config.TownRoot), intent.Record{Desired: intent.DesiredPark})
	f.src.prev = attention.State{Items: []attention.Item{
		{Key: "seat-finished:gastown/capable", Kind: attention.KindSeatFinishedLingering, Severity: attention.SeverityLow, Rig: attentionRig, Bead: "gt-abc12"},
	}}
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect = %+v, %v; want no error for one seat's unreadable record", items, err)
	}
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	if want := []string{"seat-finished:gastown/capable", "seat-finished:gastown/nux"}; !slices.Equal(keys, want) {
		t.Fatalf("item keys = %v, want the unreadable seat's previous item and the good parked seat's own", keys)
	}
}

// A seat that raised the item and has left the walk raises nothing next tick:
// the item is keyed by the seat, and a key the walk no longer shows is one the
// queue clears. The previous tick's item is carried in prev, as the daemon
// carries it across heartbeats.
func TestSeatFinishedLingeringClearsWhenTheSeatLeavesTheWalk(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
	)
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	first, err := c.collect(context.Background())
	if err != nil || len(first) != 1 {
		t.Fatalf("first tick = %+v, %v; want the one item for the walking seat", first, err)
	}

	f.src.seats = func() ([]townhealth.Seat, error) { return nil, nil }
	got := collectAttention(context.Background(), attention.State{Items: first},
		[]attentionCollector{c}, func(string, ...any) {})
	if len(got) != 0 {
		t.Errorf("items after the seat left the walk = %+v, want none", got)
	}
}

// A done seat whose bead is still open is rework with its branch preserved,
// not a seat left behind, so it raises nothing however long it has lingered.
func TestSeatFinishedLingeringIgnoresOpenWork(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusOpen), state: "done", age: 3 * time.Hour},
	)
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none for a done seat whose bead is open", items)
	}
}

// A seat the rule cannot judge raises nothing: one that is not a polecat, one
// whose record is not done, and one whose record names no bead.
func TestSeatFinishedLingeringSkipsSeatsItCannotJudge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, f *attentionFixture)
	}{
		{
			name: "the seat is not a polecat",
			setup: func(t *testing.T, f *attentionFixture) {
				f.d.openWorkBeads = func([]string, time.Duration) workBeadReader {
					t.Error("a non-polecat seat was read")
					return keptSeatHookReader{issues: map[string]*beads.Issue{}}
				}
				f.src.seats = func() ([]townhealth.Seat, error) {
					return []townhealth.Seat{{Rig: attentionRig, Name: "witness/capable", Run: true}}, nil
				}
			},
		},
		{
			name: "the record is not done",
			setup: func(t *testing.T, f *attentionFixture) {
				finishedSeatTown(t, f, finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "working", age: 3 * time.Hour})
			},
		},
		{
			name: "the record names no bead",
			setup: func(t *testing.T, f *attentionFixture) {
				finishedSeatTown(t, f, finishedSeat{name: "capable", state: "done", age: 3 * time.Hour})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			tc.setup(t, f)
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 {
				t.Fatalf("items = %+v, want none for a seat the rule cannot judge", items)
			}
		})
	}
}

// One seat the rule cannot judge does not freeze the collection: the rule
// skips that seat and judges the others, so one broken seat no longer hides
// every item the collector would raise (gt-ujxfez).
func TestSeatFinishedLingeringOneBrokenSeatDoesNotFreezeTheOthers(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	broken := finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 3 * time.Hour}
	r := finishedSeatTown(t, f,
		broken,
		finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
	)
	r.fail = map[string]error{keptSeatAgentID(f, broken.name): errors.New("bedrock: bd unavailable")}
	f.d.openWorkBeads = r.open
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect = %+v, %v; want no error for one seat's failed read", items, err)
	}
	if len(items) != 1 || items[0].Key != "seat-finished:gastown/nux" {
		t.Fatalf("items = %+v, want only the lingering seat's item", items)
	}
}

// A skipped seat keeps its own previous item from s.prev, so an item a seat
// raised before its read broke outlives the break; a broken seat with no
// previous item raises nothing. Every seat read the rule makes is a row: the
// skipped seat is the only one whose item survives (gt-ujxfez).
func TestSeatFinishedLingeringBrokenSeatKeepsItsPreviousItem(t *testing.T) {
	t.Parallel()
	readFails := errors.New("bedrock: bd unavailable")
	seat := finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 3 * time.Hour}
	cases := []struct {
		name string
		read func(t *testing.T, f *attentionFixture)
	}{
		{
			name: "the agent bead read fails",
			read: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.fail = map[string]error{keptSeatAgentID(f, seat.name): readFails}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the agent bead reads as no bead",
			read: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.noBead = map[string]bool{keptSeatAgentID(f, seat.name): true}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the work bead read fails",
			read: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.fail = map[string]error{seat.bead: readFails}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the work bead reads as no bead",
			read: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.noBead = map[string]bool{seat.bead: true}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the record carries no update time",
			read: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.issues[keptSeatAgentID(f, seat.name)].UpdatedAt = ""
				f.d.openWorkBeads = r.open
			},
		},
	}
	previous := attention.State{Items: []attention.Item{
		{Key: "seat-finished:gastown/capable", Kind: attention.KindSeatFinishedLingering, Severity: attention.SeverityLow, Rig: attentionRig, Bead: seat.bead},
		{Key: "stall:gastown/opal", Kind: attention.KindPolecatStall, Severity: attention.SeverityHigh, Summary: "another collector's item"},
	}}
	for _, tc := range cases {
		for _, kept := range []bool{true, false} {
			name := tc.name + ", no previous item"
			if kept {
				name = tc.name + ", previous item kept"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newAttentionFixture(t, keptSeatAttentionNow)
				tc.read(t, f)
				if kept {
					f.src.prev = previous
				}
				c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

				items, err := c.collect(context.Background())
				if err != nil {
					t.Fatalf("collect = %+v, %v; want no error for a skipped seat", items, err)
				}
				if !kept {
					if len(items) != 0 {
						t.Fatalf("items = %+v, want none for a skipped seat with no previous item", items)
					}
					return
				}
				if len(items) != 1 || items[0].Key != "seat-finished:gastown/capable" {
					t.Fatalf("items = %+v, want only the skipped seat's previous item", items)
				}
			})
		}
	}
}

// The seat walk is the read the collection starts from, not a per-seat read: a
// walk that cannot be listed still fails the collection, so the queue keeps the
// seat-finished items it had rather than clearing them (gt-ujxfez; the walk's
// own visible item is gt-w797qr).
func TestSeatFinishedLingeringWalkFailureStillFailsTheCollection(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	f.src.seats = func() ([]townhealth.Seat, error) { return nil, errors.New("bedrock: bd unavailable") }
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err == nil {
		t.Fatalf("collect = %+v, nil; want the walk's error", items)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none beside the error", items)
	}

	prev := attention.State{Items: []attention.Item{
		{Key: "seat-finished:gastown/capable", Kind: attention.KindSeatFinishedLingering, Severity: attention.SeverityLow, Rig: attentionRig, Bead: "gt-abc12"},
	}}
	got := collectAttention(context.Background(), prev, []attentionCollector{c}, func(string, ...any) {})
	if len(got) != 1 || got[0].Key != "seat-finished:gastown/capable" {
		t.Errorf("collectAttention = %+v, want the previous seat-finished item kept", got)
	}
}

// gt-w797qr: a tick that skips seats says so in one log line, naming each
// skipped seat and the read that failed, and a tick with no broken seat logs
// none. The walk's own failure is the collection's error, not a line here.
func TestSeatFinishedLingeringLogsTheSkippedSeats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		breakRead func(t *testing.T, f *attentionFixture, r *keptSeatHookReader)
		reason    string
	}{
		{
			name: "the agent bead read fails",
			breakRead: func(t *testing.T, f *attentionFixture, r *keptSeatHookReader) {
				r.fail = map[string]error{keptSeatAgentID(f, "capable"): errors.New("bd show: agent bead unavailable")}
			},
			reason: "bd show: agent bead unavailable",
		},
		{
			name: "the agent bead reads as no issue",
			breakRead: func(t *testing.T, f *attentionFixture, r *keptSeatHookReader) {
				r.noBead = map[string]bool{keptSeatAgentID(f, "capable"): true}
			},
			reason: "bd show {agent}: no issue",
		},
		{
			name: "the work bead read fails",
			breakRead: func(t *testing.T, f *attentionFixture, r *keptSeatHookReader) {
				r.fail = map[string]error{"gt-abc12": errors.New("bd show: work bead unavailable")}
			},
			reason: "bd show: work bead unavailable",
		},
		{
			name: "the work bead reads as no bead",
			breakRead: func(t *testing.T, f *attentionFixture, r *keptSeatHookReader) {
				r.noBead = map[string]bool{"gt-abc12": true}
			},
			reason: "gt-abc12 reads as no bead",
		},
		{
			name: "the agent bead has no update time",
			breakRead: func(t *testing.T, f *attentionFixture, r *keptSeatHookReader) {
				r.issues[keptSeatAgentID(f, "capable")].UpdatedAt = ""
			},
			reason: "agent bead has no update time",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			r := finishedSeatTown(t, f,
				finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
				finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
			)
			tc.breakRead(t, f, &r)
			f.d.openWorkBeads = r.open
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatalf("collect = %+v, %v; want no error for one skipped seat", items, err)
			}
			if len(items) != 1 || items[0].Key != "seat-finished:gastown/nux" {
				t.Fatalf("items = %+v, want the judged seat's item", items)
			}
			reason := strings.ReplaceAll(tc.reason, "{agent}", keptSeatAgentID(f, "capable"))
			want := "attention: seat-finished: gastown/capable not judged: " + reason + "; the others are judged"
			if got := seatFinishedLines(f.logs.String()); !slices.Equal(got, []string{want}) {
				t.Errorf("log lines = %q, want one line %q", got, want)
			}
		})
	}

	t.Run("two skipped seats share one line", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, keptSeatAttentionNow)
		r := finishedSeatTown(t, f,
			finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
			finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
			finishedSeat{name: "opal", bead: "gt-abc14", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
		)
		r.fail = map[string]error{
			keptSeatAgentID(f, "capable"): errors.New("bd show: agent bead unavailable"),
			"gt-abc13":                    errors.New("bd show: work bead unavailable"),
		}
		f.d.openWorkBeads = r.open
		c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

		items, err := c.collect(context.Background())
		if err != nil {
			t.Fatalf("collect = %+v, %v; want no error for two skipped seats", items, err)
		}
		if len(items) != 1 || items[0].Key != "seat-finished:gastown/opal" {
			t.Fatalf("items = %+v, want only the judged seat's item", items)
		}
		want := "attention: seat-finished: gastown/capable not judged: bd show: agent bead unavailable; " +
			"gastown/nux not judged: bd show: work bead unavailable; the others are judged"
		if got := seatFinishedLines(f.logs.String()); !slices.Equal(got, []string{want}) {
			t.Errorf("log lines = %q, want one line %q", got, want)
		}
	})

	t.Run("a tick with no broken seat logs none", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, keptSeatAttentionNow)
		finishedSeatTown(t, f,
			finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
			finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusOpen), state: "done", age: 3 * time.Hour},
		)
		c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

		items, err := c.collect(context.Background())
		if err != nil || len(items) != 1 {
			t.Fatalf("collect = %+v, %v; want the one judged seat's item", items, err)
		}
		if got := seatFinishedLines(f.logs.String()); len(got) != 0 {
			t.Errorf("log lines = %q, want none for a tick with no broken seat", got)
		}
	})
}

// seatFinishedLines is the log lines the seat-finished collector wrote; the
// daemon's other lines (the agent bead shadow) are left out.
func seatFinishedLines(logs string) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(line, "attention: seat-finished:") {
			lines = append(lines, line)
		}
	}
	return lines
}
