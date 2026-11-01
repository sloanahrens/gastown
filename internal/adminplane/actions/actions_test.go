package actions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
)

// at is the time every test's clock reports, far enough from the wall clock
// that a call the package makes to time.Now instead is visible.
var at = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fixedClock always reports at.
func fixedClock() time.Time { return at }

// sameEntry reports whether a and b are one entry. At is compared with Equal
// rather than ==, which also weighs the time's location and monotonic reading
// and so calls two names for one instant unequal.
func sameEntry(a, b ActionEntry) bool {
	return a.Actor == b.Actor && a.Workspace == b.Workspace &&
		a.From == b.From && a.To == b.To && a.At.Equal(b.At)
}

// errStale is the refusal fakeStore gives a Save whose workspace moved after Load.
var errStale = errors.New("fakeStore: workspace changed since it was loaded")

// fakeStore is Store in memory, recording every save so a test can assert a
// refused move wrote nothing. It holds Store's compare-and-swap: Load records the
// state it returned, and Save refuses while the stored state differs from that.
type fakeStore struct {
	workspaces map[string]registry.Workspace
	loaded     map[string]registry.State
	saves      []registry.Workspace
	loadErr    error
	saveErr    error
	// afterLoad, when set, runs once after Load has recorded its state and
	// before it returns, standing in for a move that lands in between.
	afterLoad func(s *fakeStore)
}

// newFakeStore returns a store holding one workspace, ws-1, in state.
func newFakeStore(state registry.State) *fakeStore {
	return &fakeStore{
		workspaces: map[string]registry.Workspace{
			"ws-1": {ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1", State: state, CreatedAt: at},
		},
		loaded: map[string]registry.State{},
	}
}

func (s *fakeStore) Load(_ context.Context, id string) (registry.Workspace, error) {
	if s.loadErr != nil {
		return registry.Workspace{}, s.loadErr
	}
	w, ok := s.workspaces[id]
	if !ok {
		return registry.Workspace{}, errors.New("fakeStore: no workspace " + id)
	}
	s.loaded[id] = w.State
	if hook := s.afterLoad; hook != nil {
		s.afterLoad = nil
		hook(s)
	}
	return w, nil
}

func (s *fakeStore) Save(_ context.Context, w registry.Workspace) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.workspaces[w.ID].State != s.loaded[w.ID] {
		return errStale
	}
	s.saves = append(s.saves, w)
	s.workspaces[w.ID] = w
	return nil
}

// fakeLog is ActionLog in memory, failing instead of appending while err is set.
type fakeLog struct {
	entries []ActionEntry
	err     error
}

func (l *fakeLog) Append(_ context.Context, e ActionEntry) error {
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, e)
	return nil
}

func TestPauseSavesAndLogsActor(t *testing.T) {
	t.Parallel()
	store, log := newFakeStore(registry.Active), &fakeLog{}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	if err := Pause(context.Background(), d, "ws-1", "operator"); err != nil {
		t.Fatalf("Pause returned %v, want nil", err)
	}
	if got := store.workspaces["ws-1"].State; got != registry.Paused {
		t.Errorf("stored state = %q, want %q", got, registry.Paused)
	}
	if len(log.entries) != 1 {
		t.Fatalf("logged %d entries, want 1: %+v", len(log.entries), log.entries)
	}
	got := log.entries[0]
	if got.Actor != "operator" || got.Workspace != "ws-1" || got.From != registry.Active || got.To != registry.Paused || !got.At.Equal(at) {
		t.Errorf("entry = %+v, want actor operator, workspace ws-1, %q to %q at %v", got, registry.Active, registry.Paused, at)
	}
}

func TestSuspendSavesAndLogsActor(t *testing.T) {
	t.Parallel()
	store, log := newFakeStore(registry.Active), &fakeLog{}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	if err := Suspend(context.Background(), d, "ws-1", "operator"); err != nil {
		t.Fatalf("Suspend returned %v, want nil", err)
	}
	if got := store.workspaces["ws-1"].State; got != registry.Suspended {
		t.Errorf("stored state = %q, want %q", got, registry.Suspended)
	}
	if len(log.entries) != 1 {
		t.Fatalf("logged %d entries, want 1: %+v", len(log.entries), log.entries)
	}
	got := log.entries[0]
	if got.Actor != "operator" || got.Workspace != "ws-1" || got.From != registry.Active || got.To != registry.Suspended || !got.At.Equal(at) {
		t.Errorf("entry = %+v, want actor operator, workspace ws-1, %q to %q at %v", got, registry.Active, registry.Suspended, at)
	}
}

func TestIllegalMoveWritesNothing(t *testing.T) {
	t.Parallel()
	store, log := newFakeStore(registry.Closed), &fakeLog{}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	err := Suspend(context.Background(), d, "ws-1", "operator")
	var refusal *registry.TransitionError
	if !errors.As(err, &refusal) {
		t.Fatalf("Suspend of a closed workspace returned %v, want *registry.TransitionError", err)
	}
	if refusal.From != registry.Closed || refusal.To != registry.Suspended {
		t.Errorf("refusal names %q to %q, want %q to %q", refusal.From, refusal.To, registry.Closed, registry.Suspended)
	}
	if len(store.saves) != 0 {
		t.Errorf("refused move saved %d times: %+v", len(store.saves), store.saves)
	}
	if len(log.entries) != 0 {
		t.Errorf("refused move logged %d entries: %+v", len(log.entries), log.entries)
	}
	if got := store.workspaces["ws-1"].State; got != registry.Closed {
		t.Errorf("stored state changed to %q, want %q", got, registry.Closed)
	}
}

func TestLogFailureAfterSaveIsReported(t *testing.T) {
	t.Parallel()
	boom := errors.New("log unavailable")
	store, log := newFakeStore(registry.Active), &fakeLog{err: boom}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	err := Pause(context.Background(), d, "ws-1", "operator")
	if err == nil {
		t.Fatal("Pause with a failing log returned nil, want an error")
	}
	if !errors.Is(err, ErrLogNotWritten) {
		t.Errorf("error %v is not ErrLogNotWritten", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the append failure %v", err, boom)
	}
	var failure *LogFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *LogFailure, so the caller cannot retry the entry", err)
	}
	want := ActionEntry{Actor: "operator", Workspace: "ws-1", From: registry.Active, To: registry.Paused, At: at}
	if !sameEntry(failure.Action, want) {
		t.Errorf("failure carries entry %+v, want %+v", failure.Action, want)
	}
	if got := store.workspaces["ws-1"].State; got != registry.Paused {
		t.Errorf("stored state = %q, want %q: the save precedes the append", got, registry.Paused)
	}

	retry := &fakeLog{}
	if err := retry.Append(context.Background(), failure.Action); err != nil {
		t.Fatalf("appending the failure's entry returned %v, want nil", err)
	}
	if len(retry.entries) != 1 || !sameEntry(retry.entries[0], want) {
		t.Errorf("retried log holds %+v, want exactly %+v", retry.entries, want)
	}
}

// TestEmptyActorRefused keeps a move from leaving an entry that names nobody.
func TestEmptyActorRefused(t *testing.T) {
	t.Parallel()
	store, log := newFakeStore(registry.Active), &fakeLog{}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	moves := map[string]func(context.Context, Deps, string, string) error{"Pause": Pause, "Suspend": Suspend}
	for name, move := range moves {
		if err := move(context.Background(), d, "ws-1", ""); !errors.Is(err, ErrNoActor) {
			t.Errorf("%s with no actor returned %v, want ErrNoActor", name, err)
		}
	}
	if len(store.saves) != 0 {
		t.Errorf("a move with no actor saved %d times: %+v", len(store.saves), store.saves)
	}
	if len(log.entries) != 0 {
		t.Errorf("a move with no actor logged %d entries: %+v", len(log.entries), log.entries)
	}
	if got := store.workspaces["ws-1"].State; got != registry.Active {
		t.Errorf("stored state = %q, want %q unchanged", got, registry.Active)
	}
}

func TestLoadFailureWritesNothing(t *testing.T) {
	t.Parallel()
	loadErr := errors.New("workspace store unavailable")
	store := newFakeStore(registry.Active)
	store.loadErr = loadErr
	log := &fakeLog{}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	err := Pause(context.Background(), d, "ws-1", "operator")
	if !errors.Is(err, loadErr) {
		t.Fatalf("Pause with a failing load returned %v, want the load error", err)
	}
	if len(store.saves) != 0 {
		t.Errorf("failed load saved %d times: %+v", len(store.saves), store.saves)
	}
	if len(log.entries) != 0 {
		t.Errorf("failed load logged %d entries: %+v", len(log.entries), log.entries)
	}
}

func TestSaveFailureIsNotLogged(t *testing.T) {
	t.Parallel()
	saveErr := errors.New("write refused")
	store := newFakeStore(registry.Active)
	store.saveErr = saveErr
	log := &fakeLog{}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	err := Pause(context.Background(), d, "ws-1", "operator")
	if !errors.Is(err, saveErr) {
		t.Fatalf("Pause with a failing save returned %v, want the save error", err)
	}
	if len(log.entries) != 0 {
		t.Errorf("failed save logged %d entries, want none: the state did not change", len(log.entries))
	}
}

// TestSaveRefusedWhenStateMovedAfterLoad holds Store's compare-and-swap: a move
// whose workspace changed between Load and Save is refused, so it neither
// overwrites the move that landed first nor leaves an entry for its own.
func TestSaveRefusedWhenStateMovedAfterLoad(t *testing.T) {
	t.Parallel()
	store, log := newFakeStore(registry.Active), &fakeLog{}
	store.afterLoad = func(s *fakeStore) {
		w := s.workspaces["ws-1"]
		w.State = registry.Suspended
		s.workspaces["ws-1"] = w
	}
	d := Deps{Store: store, Log: log, Clock: fixedClock}

	err := Pause(context.Background(), d, "ws-1", "operator")
	if !errors.Is(err, errStale) {
		t.Fatalf("Pause after the state moved between load and save returned %v, want errStale", err)
	}
	if got := store.workspaces["ws-1"].State; got != registry.Suspended {
		t.Errorf("stored state = %q, want %q: the stale save overwrote the move that landed first", got, registry.Suspended)
	}
	if len(store.saves) != 0 {
		t.Errorf("stale save stored %d workspaces, want none: %+v", len(store.saves), store.saves)
	}
	if len(log.entries) != 0 {
		t.Errorf("stale move logged %d entries, want none: %+v", len(log.entries), log.entries)
	}
}

func TestNilClockWritesNothing(t *testing.T) {
	t.Parallel()
	store, log := newFakeStore(registry.Active), &fakeLog{}
	d := Deps{Store: store, Log: log}

	err := Pause(context.Background(), d, "ws-1", "operator")
	if !errors.Is(err, registry.ErrNoClock) {
		t.Fatalf("Pause with no clock returned %v, want registry.ErrNoClock", err)
	}
	if len(store.saves) != 0 {
		t.Errorf("refused move saved %d times: %+v", len(store.saves), store.saves)
	}
	if len(log.entries) != 0 {
		t.Errorf("refused move logged %d entries: %+v", len(log.entries), log.entries)
	}
}

func TestNoStoreRefused(t *testing.T) {
	t.Parallel()
	d := Deps{Log: &fakeLog{}, Clock: fixedClock}
	if err := Pause(context.Background(), d, "ws-1", "operator"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("Pause with no store returned %v, want ErrNoStore", err)
	}
}

func TestNoLogRefused(t *testing.T) {
	t.Parallel()
	store := newFakeStore(registry.Active)
	d := Deps{Store: store, Clock: fixedClock}
	if err := Pause(context.Background(), d, "ws-1", "operator"); !errors.Is(err, ErrNoLog) {
		t.Fatalf("Pause with no log returned %v, want ErrNoLog", err)
	}
	if len(store.saves) != 0 {
		t.Errorf("move without a log saved %d times, want none", len(store.saves))
	}
}
