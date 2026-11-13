package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/session"
)

// escalationCloseTown is one town the close nudge runs in: escalations and held
// beads in bd, seat sessions in tmux, and the sends the notifier recorded. A
// restart is a new Daemon over the same town.
type escalationCloseTown struct {
	t      *testing.T
	root   string
	bd     *workBD
	tmux   *fakeTmux
	notify *notifyfake.Recorder
}

func newEscalationCloseTown(t *testing.T) *escalationCloseTown {
	t.Helper()
	return &escalationCloseTown{
		t:      t,
		root:   t.TempDir(),
		bd:     newWorkBD(t),
		tmux:   newFakeTmux(clockwork.NewFakeClockAt(time.Date(2026, time.November, 13, 0, 0, 0, 0, time.UTC))),
		notify: notifyfake.New(),
	}
}

// daemon is the daemon a tick or a restart runs as.
func (w *escalationCloseTown) daemon() *Daemon {
	return &Daemon{
		config:        &Config{TownRoot: w.root},
		logger:        log.New(io.Discard, "", 0),
		tmux:          w.tmux,
		notifier:      w.notify,
		openWorkBeads: w.bd.open,
	}
}

// tick runs one patrol_scan cycle's close pass over gastown.
func (w *escalationCloseTown) tick() {
	d := w.daemon()
	d.nudgeClosedEscalations(&patrolScanHost{d: d}, []string{"gastown"})
}

// escalate files an escalation the seat raised for bead, open or closed with
// reason.
func (w *escalationCloseTown) escalate(id, raiser, bead, status, reason string) {
	w.bd.db.Seed(beads.Issue{ID: id, Status: status, Title: bead + ": a decision",
		Labels: []string{"gt:escalation"}, CloseReason: reason,
		Description: beads.FormatEscalationDescription(bead+": a decision", &beads.EscalationFields{
			Severity: "high", EscalatedBy: raiser, RelatedBead: bead})})
}

// hold makes bead the live work of seat.
func (w *escalationCloseTown) hold(bead, seat string) {
	w.bd.db.Seed(beads.Issue{ID: bead, Status: "hooked", Assignee: seat})
}

// session sets the seat's tmux session: absent when !live, at its prompt when
// idle.
func (w *escalationCloseTown) session(name string, live, idle bool) {
	sessionName := session.PolecatSessionName(w.daemon().prefixRegistry().PrefixForRig("gastown"), name)
	if live {
		w.tmux.addSession(sessionName, "", w.tmux.clock.Now())
		w.tmux.SetIdle(sessionName, idle)
	}
}

// close marks escalation id closed with reason.
func (w *escalationCloseTown) close(id, bead, raiser, reason string) {
	w.escalate(id, raiser, bead, "closed", reason)
}

// delivered is the nudges that went out, not the ones the notifier refused.
func (w *escalationCloseTown) delivered() []notifyfake.Call {
	var out []notifyfake.Call
	for _, c := range w.notify.Nudges() {
		if c.Err == nil {
			out = append(out, c)
		}
	}
	return out
}

const (
	closeTestBead   = "gt-work1"
	closeTestSeat   = "gastown/polecats/capable"
	closeTestEscID  = "hq-esc1"
	closeTestReason = "use the existing helper"
)

// The acceptance case: a live idle seat whose escalation closes between two
// ticks gets exactly one nudge with the close reason, however many ticks follow.
func TestEscalationCloseNudgesLiveSeatOnceAcrossTicks(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.session("capable", true, true)
	w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")

	w.tick()
	if got := w.delivered(); len(got) != 0 {
		t.Fatalf("tick while open: %d nudges, want none", len(got))
	}

	w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
	w.tick()
	got := w.delivered()
	if len(got) != 1 {
		t.Fatalf("tick after close: %d nudges, want 1", len(got))
	}
	wantMsg := "Your escalation hq-esc1 was closed: use the existing helper. Continue."
	if got[0].Target != "gastown/capable" || got[0].Message != wantMsg {
		t.Errorf("nudge = %q %q, want %q %q", got[0].Target, got[0].Message, "gastown/capable", wantMsg)
	}

	w.tick()
	w.tick()
	if got := w.delivered(); len(got) != 1 {
		t.Errorf("after more ticks: %d nudges, want still 1", len(got))
	}
}

// A seat is nudged only when it still holds the escalation's bead, has a live
// session, and is at its prompt. Each row opens the escalation, closes it, and
// ticks once.
func TestEscalationCloseNudgeSeatGates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup builds the seat and the escalation's bead before the open tick.
		setup func(w *escalationCloseTown)
		want  int
	}{
		{
			name: "live idle seat holding the bead is nudged",
			setup: func(w *escalationCloseTown) {
				w.hold(closeTestBead, closeTestSeat)
				w.session("capable", true, true)
			},
			want: 1,
		},
		{
			name: "no live session is not nudged",
			setup: func(w *escalationCloseTown) {
				w.hold(closeTestBead, closeTestSeat)
			},
			want: 0,
		},
		{
			name: "seat that has moved on to another bead is not nudged",
			setup: func(w *escalationCloseTown) {
				w.hold("gt-work2", closeTestSeat)
				w.session("capable", true, true)
			},
			want: 0,
		},
		{
			name: "seat holding no bead is not nudged",
			setup: func(w *escalationCloseTown) {
				w.session("capable", true, true)
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newEscalationCloseTown(t)
			tt.setup(w)
			w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")
			w.tick()
			w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
			w.tick()
			if got := len(w.delivered()); got != tt.want {
				t.Errorf("nudges = %d, want %d", got, tt.want)
			}
		})
	}
}

// A seat with no live session when its escalation closes is settled: the close
// is not owed. When the seat is started again later, idle, it is not nudged
// about a close it missed, since patrol_scan resumes an ended seat instead
// (gt-4hduq).
func TestEscalationCloseNudgeNoLiveSessionSettles(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")
	w.tick()
	w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
	w.tick()
	w.session("capable", true, true)
	w.tick()
	w.tick()
	if got := len(w.delivered()); got != 0 {
		t.Errorf("seat started after its close: %d nudges, want none", got)
	}
}

// An escalation for another bead than the one the seat holds is not its close,
// so the seat gets nothing, even though it is live and idle.
func TestEscalationCloseNudgeIgnoresOtherBeadEscalation(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.session("capable", true, true)
	w.escalate(closeTestEscID, closeTestSeat, "gt-work2", "open", "")
	w.tick()
	w.close(closeTestEscID, "gt-work2", closeTestSeat, closeTestReason)
	w.tick()
	if got := len(w.delivered()); got != 0 {
		t.Errorf("nudges = %d, want 0 for an escalation on another bead", got)
	}
}

// A busy seat is not nudged at once: the nudge waits for it to reach its prompt,
// and goes out on the first tick it is idle.
func TestEscalationCloseNudgeWaitsForIdleSeat(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.session("capable", true, false)
	w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")
	w.tick()
	w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
	w.tick()
	if got := len(w.delivered()); got != 0 {
		t.Fatalf("busy seat: %d nudges, want none yet", got)
	}

	w.tmux.SetIdle(session.PolecatSessionName(w.daemon().prefixRegistry().PrefixForRig("gastown"), "capable"), true)
	w.tick()
	w.tick()
	if got := len(w.delivered()); got != 1 {
		t.Errorf("after the seat is idle: %d nudges, want exactly 1", got)
	}
}

// The state file is what a restarted daemon starts from. An escalation that
// closed while the daemon was down is nudged once by the new daemon, and an
// escalation already announced is not nudged again after a further restart.
func TestEscalationCloseNudgeStateSurvivesRestart(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.session("capable", true, true)
	w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")
	w.tick()
	statePath := filepath.Join(w.root, "daemon", escalationCloseNudgeFile)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not written after the open tick: %v", err)
	}

	// Closed while the daemon was down: the restarted daemon sees the close.
	w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
	w.tick()
	if got := len(w.delivered()); got != 1 {
		t.Fatalf("restarted daemon: %d nudges for a close during downtime, want 1", got)
	}

	// A second restart after the announcement reloads it and nudges nothing.
	w.tick()
	if got := len(w.delivered()); got != 1 {
		t.Errorf("after a restart past the announcement: %d nudges, want still 1", got)
	}

	st, err := loadEscalationCloseState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Announced[closeTestEscID] {
		t.Errorf("persisted state does not announce %s: %+v", closeTestEscID, st)
	}
}

// The pass judges each close against the state it starts from. A close is
// announced only when its id was watched open; an id already announced is never
// nudged again, even if the state still watches it.
func TestRunEscalationCloseNudgesJudgesAgainstState(t *testing.T) {
	t.Parallel()
	closed := patrolscan.Escalation{ID: closeTestEscID, EscalatedBy: closeTestSeat,
		RelatedBead: closeTestBead, CloseReason: closeTestReason}
	idleSeat := func(sent *string) escalationCloseSeams {
		return escalationCloseSeams{
			escalations: func(string) ([]patrolscan.Escalation, error) {
				return []patrolscan.Escalation{closed}, nil
			},
			holding: func(string, string) (string, error) { return closeTestBead, nil },
			seat:    func(string, string) (escalationSeat, error) { return escalationSeat{Live: true, Idle: true}, nil },
			nudge: func(target, msg string) error {
				*sent = msg
				return nil
			},
			logf: func(string, ...any) {},
		}
	}
	tests := []struct {
		name  string
		state escalationCloseState
		want  bool
	}{
		{"closed and watched is announced", escalationCloseState{Watch: map[string]bool{closeTestEscID: true}}, true},
		{"closed and never seen open is not",
			escalationCloseState{}, false},
		{"announced is not nudged again even while watched",
			escalationCloseState{Watch: map[string]bool{closeTestEscID: true}, Announced: map[string]bool{closeTestEscID: true}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var sent string
			next, ok := runEscalationCloseNudges(idleSeat(&sent), tt.state, []string{"gastown"})
			if !ok {
				t.Fatal("pass reported an unreadable tick")
			}
			if got := sent != ""; got != tt.want {
				t.Errorf("nudged = %v, want %v (message %q)", got, tt.want, sent)
			}
			if !next.Announced[closeTestEscID] && tt.want {
				t.Errorf("next state does not announce %s after a nudge: %+v", closeTestEscID, next)
			}
		})
	}
}

// An escalated_by whose polecat name is not one path element names no seat, so
// it is settled even when every other read would nudge it: the seam here holds
// the bead and reports the session live and idle for any name.
func TestRunEscalationCloseNudgesRejectsMalformedSeatName(t *testing.T) {
	t.Parallel()
	closed := patrolscan.Escalation{ID: closeTestEscID, EscalatedBy: "gastown/polecats/capable/x",
		RelatedBead: closeTestBead, CloseReason: closeTestReason}
	var sent string
	seams := escalationCloseSeams{
		escalations: func(string) ([]patrolscan.Escalation, error) { return []patrolscan.Escalation{closed}, nil },
		holding:     func(string, string) (string, error) { return closeTestBead, nil },
		seat:        func(string, string) (escalationSeat, error) { return escalationSeat{Live: true, Idle: true}, nil },
		nudge: func(target, msg string) error {
			sent = target
			return nil
		},
		logf: func(string, ...any) {},
	}
	st := escalationCloseState{Watch: map[string]bool{closeTestEscID: true}}
	next, ok := runEscalationCloseNudges(seams, st, []string{"gastown"})
	if !ok {
		t.Fatal("pass reported an unreadable tick")
	}
	if sent != "" {
		t.Errorf("nudged %q for a malformed seat name, want none", sent)
	}
	if !next.Announced[closeTestEscID] {
		t.Errorf("malformed name not settled: next state %+v", next)
	}
}

// A closed escalation the seat is owed a nudge for is kept owed when the send
// fails, and sent on the next tick that succeeds.
func TestEscalationCloseNudgeRetriesFailedSend(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.session("capable", true, true)
	w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")
	w.tick()
	w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
	w.notify.Fail(notifyfake.KindNudge, errors.New("gt nudge: refused"))
	w.tick()
	if got := len(w.delivered()); got != 0 {
		t.Fatalf("refused send: %d delivered nudges, want none", got)
	}

	w.notify.Fail(notifyfake.KindNudge, nil)
	w.tick()
	w.tick()
	if got := len(w.delivered()); got != 1 {
		t.Errorf("after the send recovers: %d delivered nudges, want exactly 1", got)
	}
}

// When the escalations cannot be read, nothing is judged and the state the
// daemon keeps is left as it was, so the close is still owed when they read.
func TestEscalationCloseNudgeReadFailureKeepsState(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	w.hold(closeTestBead, closeTestSeat)
	w.session("capable", true, true)
	w.escalate(closeTestEscID, closeTestSeat, closeTestBead, "open", "")
	w.tick()
	statePath := filepath.Join(w.root, "daemon", escalationCloseNudgeFile)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	w.close(closeTestEscID, closeTestBead, closeTestSeat, closeTestReason)
	w.bd.listErr = errors.New("bd list: store unavailable")
	w.tick()
	if got := len(w.delivered()); got != 0 {
		t.Fatalf("unreadable escalations: %d nudges, want none", got)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("state changed on an unreadable tick:\n before %s\n after  %s", before, after)
	}

	w.bd.listErr = nil
	w.tick()
	if got := len(w.delivered()); got != 1 {
		t.Errorf("after the reads recover: %d nudges, want 1", got)
	}
}

// An escalation closed with no reason says so, and a reason is trimmed and cut
// at escalationCloseReasonMax characters, never inside a character.
func TestEscalationCloseMessage(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", escalationCloseReasonMax+50)
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{"no reason", "", "Your escalation hq-esc1 was closed: no close reason was given. Continue."},
		{"blank reason", "   ", "Your escalation hq-esc1 was closed: no close reason was given. Continue."},
		{"reason is trimmed", "  use the helper  ", "Your escalation hq-esc1 was closed: use the helper. Continue."},
		{"long reason is cut", long, "Your escalation hq-esc1 was closed: " +
			strings.Repeat("é", escalationCloseReasonMax) + ". Continue."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := escalationCloseMessage(escalationCloseEsc(tt.reason))
			if got != tt.want {
				t.Errorf("message = %q, want %q", got, tt.want)
			}
		})
	}
}

// The production seat read: no session is not live; a session is live, idle
// when it sits at its prompt.
func TestEscalationSeatStateReadsSession(t *testing.T) {
	t.Parallel()
	w := newEscalationCloseTown(t)
	d := w.daemon()
	got, err := d.escalationSeatState("gastown", "capable")
	if err != nil || got != (escalationSeat{}) {
		t.Fatalf("no session: %+v, %v; want not live", got, err)
	}

	w.session("capable", true, false)
	got, err = d.escalationSeatState("gastown", "capable")
	if err != nil || got != (escalationSeat{Live: true, Idle: false}) {
		t.Errorf("busy session: %+v, %v; want live and busy", got, err)
	}

	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig("gastown"), "capable")
	w.tmux.SetIdle(sessionName, true)
	got, err = d.escalationSeatState("gastown", "capable")
	if err != nil || got != (escalationSeat{Live: true, Idle: true}) {
		t.Errorf("idle session: %+v, %v; want live and idle", got, err)
	}
}

// A failed read of the session, of the state file, or a failed write of it is
// returned, never taken for a seat that is not live or an empty state.
func TestEscalationCloseFailedReadsAndWritesAreReturned(t *testing.T) {
	t.Parallel()
	t.Run("session read fails", func(t *testing.T) {
		t.Parallel()
		w := newEscalationCloseTown(t)
		w.tmux.hasErr = errors.New("tmux: server unavailable")
		if _, err := w.daemon().escalationSeatState("gastown", "capable"); err == nil {
			t.Error("escalationSeatState: nil error for a failed session read")
		}
	})
	t.Run("state path is a directory", func(t *testing.T) {
		t.Parallel()
		if _, err := loadEscalationCloseState(t.TempDir()); err == nil {
			t.Error("loadEscalationCloseState: nil error reading a directory")
		}
	})
	t.Run("state file is not JSON", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), escalationCloseNudgeFile)
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadEscalationCloseState(path); err == nil {
			t.Error("loadEscalationCloseState: nil error for a file that is not JSON")
		}
	})
	t.Run("state directory cannot be made", func(t *testing.T) {
		t.Parallel()
		blocker := filepath.Join(t.TempDir(), "daemon")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := saveEscalationCloseState(filepath.Join(blocker, escalationCloseNudgeFile), escalationCloseState{}); err == nil {
			t.Error("saveEscalationCloseState: nil error when its directory is a file")
		}
	})
}

// escalationCloseEsc is a closed escalation carrying reason, for the message.
func escalationCloseEsc(reason string) patrolscan.Escalation {
	return patrolscan.Escalation{ID: closeTestEscID, CloseReason: reason}
}
