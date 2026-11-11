package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// The tick releases a seat only for edges a sling would refuse the bead over,
// so it reads the dependencies through the dispatcher's own rule: a closed
// blocker or a non-blocking relation admits, and an edge whose status could not
// be read is a blocker (the dispatcher refuses that bead too).
func TestPatrolScanBlockingDeps(t *testing.T) {
	t.Parallel()
	bead := func(deps ...beads.IssueDep) map[string]*beads.Issue {
		return map[string]*beads.Issue{"myr-a": {ID: "myr-a", Status: "hooked", Dependencies: deps}}
	}
	cases := []struct {
		name    string
		show    map[string]*beads.Issue
		showErr error
		want    []string // "id=status"
		wantErr bool
	}{
		{
			name: "an open blocks dependency is the blocker",
			show: bead(beads.IssueDep{ID: "myr-b", Status: "in_progress", DependencyType: "blocks"}),
			want: []string{"myr-b=in_progress"},
		},
		{
			name: "a closed blocker is resolved",
			show: bead(beads.IssueDep{ID: "myr-b", Status: "closed", DependencyType: "blocks"}),
		},
		{
			name: "a non-blocking relation is not a blocker",
			show: bead(beads.IssueDep{ID: "myr-b", Status: "open", DependencyType: "tracks"}),
		},
		{
			name: "an unresolved edge with an unread status is still a blocker",
			show: bead(beads.IssueDep{ID: "myr-b", DependencyType: "blocks"}),
			want: []string{"myr-b="},
		},
		{
			name: "a repeated edge names its blocker once",
			show: bead(
				beads.IssueDep{ID: "myr-b", Status: "open", DependencyType: "blocks"},
				beads.IssueDep{ID: "myr-b", Status: "open", DependencyType: "blocks"},
			),
			want: []string{"myr-b=open"},
		},
		{
			name: "a bead bd says is gone has nothing blocking it",
			show: map[string]*beads.Issue{},
		},
		{
			name:    "an unreadable bead is an error",
			showErr: errors.New("bd show: connection refused"),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := wispHost(t, &wispReader{show: tc.show, showErr: tc.showErr})

			got, err := h.BlockingDeps("myr", "myr-a")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BlockingDeps = %v, nil; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BlockingDeps: %v", err)
			}
			names := make([]string, 0, len(got))
			for _, ref := range got {
				names = append(names, ref.ID+"="+ref.Status)
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("blockers = %v, want %v", names, tc.want)
			}
		})
	}
}

// A Show that answers no issue and no error is not "nothing blocks it": the
// read could not answer, so the tick acts on nothing rather than releasing or
// restarting the seat on it.
func TestPatrolScanBlockingDepsRefusesAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config:        &Config{TownRoot: t.TempDir()},
		openWorkBeads: func([]string, time.Duration) workBeadReader { return quietReader{} },
	}
	h := &patrolScanHost{d: d}

	if got, err := h.BlockingDeps("myr", "myr-a"); err == nil {
		t.Fatalf("BlockingDeps over a Show that answered nothing = %v, nil; want an error", got)
	}
}

// The deferral is one guarded write: the note lands on the bead, the status
// becomes deferred and the assignment clears, and a bead another actor took
// since the read is left to that actor. An empty expected assignee is refused,
// because bd reads an empty guard as "still unassigned" (gt-otb5m3).
func TestPatrolScanDeferBlocked(t *testing.T) {
	t.Parallel()
	const seat = "myr/polecats/onyx"
	newHost := func() (*patrolScanHost, *beadsfake.Fake) {
		db := beadsfake.New()
		db.Seed(beads.Issue{ID: "myr-a", Status: "hooked", Assignee: seat})
		db.Seed(beads.Issue{ID: "myr-b", Status: "open"})
		h := &patrolScanHost{
			d:                 &Daemon{config: &Config{TownRoot: t.TempDir()}},
			openRecoveryBeads: func([]string) beads.Client { return db },
		}
		return h, db
	}

	t.Run("defers the bead to nobody with the blocker named", func(t *testing.T) {
		t.Parallel()
		h, db := newHost()
		note := patrolscan.DeferredBlockerNote(patrolscan.WorkRef{ID: "myr-b", Status: "open"}, patrolscan.ExitDeferred)

		moved, err := h.DeferBlocked("myr", "myr-a", seat, note)
		if err != nil || !moved {
			t.Fatalf("DeferBlocked = %v, %v; want true, nil", moved, err)
		}
		is, err := db.Show("myr-a")
		if err != nil {
			t.Fatal(err)
		}
		if is.Status != string(beads.StatusDeferred) || is.Assignee != "" {
			t.Fatalf("bead = %s/%q, want deferred and unassigned", is.Status, is.Assignee)
		}
		if !strings.Contains(is.Notes, "myr-b") || !strings.Contains(is.Notes, patrolscan.ExitDeferred) {
			t.Fatalf("notes = %q, want the blocker and the exit named", is.Notes)
		}
	})

	t.Run("a bead another actor holds is left to it", func(t *testing.T) {
		t.Parallel()
		h, db := newHost()
		pearl := "myr/polecats/pearl"
		if err := db.Update("myr-a", beads.UpdateOptions{Assignee: &pearl}); err != nil {
			t.Fatal(err)
		}

		moved, err := h.DeferBlocked("myr", "myr-a", seat, "")
		if err != nil || moved {
			t.Fatalf("DeferBlocked with a stale guard = %v, %v; want false, nil", moved, err)
		}
		is, err := db.Show("myr-a")
		if err != nil {
			t.Fatal(err)
		}
		if is.Status != "hooked" || is.Assignee != "myr/polecats/pearl" {
			t.Fatalf("bead = %s/%q, want the new holder's", is.Status, is.Assignee)
		}
	})

	t.Run("an empty expected assignee is refused", func(t *testing.T) {
		t.Parallel()
		h, db := newHost()

		if _, err := h.DeferBlocked("myr", "myr-a", "  ", "note"); err == nil {
			t.Fatal("DeferBlocked with no expected assignee = nil; want a refusal")
		}
		is, err := db.Show("myr-a")
		if err != nil {
			t.Fatal(err)
		}
		if is.Status != "hooked" || is.Notes != "" {
			t.Fatalf("bead = %s, notes %q; want it untouched", is.Status, is.Notes)
		}
	})

	t.Run("a failed note write leaves the bead hooked", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		db.Seed(beads.Issue{ID: "myr-a", Status: "hooked", Assignee: seat})
		h := &patrolScanHost{
			d: &Daemon{config: &Config{TownRoot: t.TempDir()}},
			openRecoveryBeads: func([]string) beads.Client {
				return deferringFake{Fake: db, notesErr: errors.New("bd update: exit status 1")}
			},
		}

		if _, err := h.DeferBlocked("myr", "myr-a", seat, "note"); err == nil {
			t.Fatal("DeferBlocked with a failed note write = nil; want an error")
		}
		is, err := db.Show("myr-a")
		if err != nil {
			t.Fatal(err)
		}
		if is.Status != "hooked" || is.Assignee != seat {
			t.Fatalf("bead = %s/%q, want it left hooked", is.Status, is.Assignee)
		}
	})

	t.Run("a failed status write is an error", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		db.Seed(beads.Issue{ID: "myr-a", Status: "hooked", Assignee: seat})
		h := &patrolScanHost{
			d: &Daemon{config: &Config{TownRoot: t.TempDir()}},
			openRecoveryBeads: func([]string) beads.Client {
				return deferringFake{Fake: db, transferErr: errors.New("bd update: exit status 1")}
			},
		}

		moved, err := h.DeferBlocked("myr", "myr-a", seat, "")
		if err == nil || moved {
			t.Fatalf("DeferBlocked with a failed transfer = %v, %v; want false and an error", moved, err)
		}
		is, err := db.Show("myr-a")
		if err != nil {
			t.Fatal(err)
		}
		if is.Status != "hooked" {
			t.Fatalf("bead status = %s, want it left hooked", is.Status)
		}
	})
}

// deferringFake is the fake store with the deferral's two writes failing on
// demand: a bd that errors on its update is a branch the fake store has no
// script for.
type deferringFake struct {
	*beadsfake.Fake
	notesErr, transferErr error
}

func (f deferringFake) AppendNotes(id, note string) error {
	if f.notesErr != nil {
		return f.notesErr
	}
	return f.Fake.AppendNotes(id, note)
}

func (f deferringFake) TransferIfAssignee(id, expected, status, assignee string) (bool, error) {
	if f.transferErr != nil {
		return false, f.transferErr
	}
	return f.Fake.TransferIfAssignee(id, expected, status, assignee)
}

// quietReader answers every read with nothing at all — not a not-found, but a
// bd that printed no issue, which BlockingDeps must refuse rather than read as
// "nothing blocks it".
type quietReader struct{}

func (quietReader) Show(string) (*beads.Issue, error)              { return nil, nil }
func (quietReader) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, nil }

// The release leaves the seat free for the pool: the bead is deferred to
// nobody, so the seat's own work read answers none, and the seat's record asks
// for no session and names no work bead, so a sling allocates the seat again
// (gt-otb5m3 acceptance).
func TestPatrolScanReleaseLeavesTheSeatFree(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "myr-a", Status: "hooked", Assignee: "myr/polecats/onyx"})
	db.Seed(beads.Issue{ID: "myr-b", Status: "in_progress"})
	if err := db.AddDependency("myr-a", "myr-b"); err != nil {
		t.Fatal(err)
	}
	h := &patrolScanHost{
		d: &Daemon{
			config:        &Config{TownRoot: town},
			openWorkBeads: func([]string, time.Duration) workBeadReader { return db },
		},
		openRecoveryBeads: func([]string) beads.Client { return db },
	}

	work, err := h.AssignedWork("myr", "onyx")
	if err != nil || work == nil {
		t.Fatalf("AssignedWork before the release = %+v, %v; want the hooked bead", work, err)
	}
	blockers, err := h.BlockingDeps("myr", work.ID)
	if err != nil || len(blockers) != 1 || blockers[0].ID != "myr-b" {
		t.Fatalf("BlockingDeps = %+v, %v; want myr-b", blockers, err)
	}

	moved, err := h.DeferBlocked("myr", work.ID, work.Assignee,
		patrolscan.DeferredBlockerNote(blockers[0], patrolscan.ExitDeferred))
	if err != nil || !moved {
		t.Fatalf("DeferBlocked = %v, %v; want true, nil", moved, err)
	}
	released, err := h.MarkIdle("myr", "onyx")
	if err != nil || !released {
		t.Fatalf("MarkIdle = %v, %v; want true, nil", released, err)
	}

	// The record the pool reads: nothing asks for a session any more.
	rec, err := intent.Read(town, supervisor.IntentSeat(h.seat("myr", "onyx")))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Desired != intent.DesiredStop || rec.WorkBead != "" {
		t.Fatalf("intent record = %q with work bead %q, want stop and none", rec.Desired, rec.WorkBead)
	}
	// The seat holds no work, and no listing names the bead as active work.
	if work, err := h.AssignedWork("myr", "onyx"); err != nil || work != nil {
		t.Fatalf("AssignedWork after the release = %+v, %v; want none", work, err)
	}
	active, err := h.ActiveWork("myr")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range active {
		if w.ID == "myr-a" {
			t.Fatalf("active work still names %s: %+v", w.ID, active)
		}
	}
	// The bead waits on nobody, and says why it sits.
	is, err := db.Show("myr-a")
	if err != nil {
		t.Fatal(err)
	}
	if is.Status != string(beads.StatusDeferred) || is.Assignee != "" || !strings.Contains(is.Notes, "myr-b") {
		t.Fatalf("bead = %s/%q, notes %q; want deferred to nobody with the blocker named", is.Status, is.Assignee, is.Notes)
	}
}
