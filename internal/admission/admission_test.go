package admission

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/abuse"
	"github.com/steveyegge/gastown/internal/adminplane/registry"
	"github.com/steveyegge/gastown/internal/keycheck"
	"github.com/steveyegge/gastown/internal/keystore"
	"github.com/steveyegge/gastown/internal/spendcap"
)

const (
	// ws is the workspace every case admits.
	ws = "ws-a"
	// keyValue is the key each case stores. It is distinctive, so a leak shows
	// up as a substring match.
	keyValue = "sk-test-KEYVALUE-0001"
)

// noon is the fixed now every case judges the spend day at, so no case sleeps.
func noon() time.Time { return time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC) }

// fixedClock keeps the limiter at noon for the whole case.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return noon() }

// fakeState answers State with the state or the error it holds.
type fakeState struct {
	state registry.State
	err   error
}

func (f fakeState) State(context.Context, string) (registry.State, error) { return f.state, f.err }

// fakeSpend answers Spend with the records, cap or error it holds.
type fakeSpend struct {
	records []spendcap.Record
	cap     spendcap.Cap
	err     error
}

func (f fakeSpend) Spend(context.Context, string) ([]spendcap.Record, spendcap.Cap, error) {
	return f.records, f.cap, f.err
}

// countProvider answers every Verify with err and counts the calls, so a case
// can prove the provider was never asked.
type countProvider struct {
	err   error
	calls int
}

func (p *countProvider) Verify(context.Context, keystore.Secret) error {
	p.calls++
	return p.err
}

// newDeps returns Deps that admit one dispatch in ws: an active workspace, a day
// under its cap, one dispatch token and a key the provider accepts. It also
// returns the provider so a case can count calls and set its answer.
func newDeps(t *testing.T) (Deps, *countProvider) {
	t.Helper()
	store := keystore.NewFake()
	if _, err := store.Put(context.Background(), ws, keycheck.KeyName, []byte(keyValue)); err != nil {
		t.Fatalf("Put the test key: %v", err)
	}
	prov := &countProvider{}
	return Deps{
		State:    fakeState{state: registry.Active},
		Spend:    fakeSpend{cap: spendcap.Cap{Workspace: ws, DailyUSD: 10}},
		Limiter:  abuse.New(abuse.Config{DispatchRate: 1, DispatchBurst: 1}, fixedClock{}),
		Store:    store,
		Provider: prov,
		Now:      noon,
	}, prov
}

func TestAdmitRefusesNonActiveStates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		state  registry.State
		reason string
	}{
		{"paused", registry.Paused, ReasonStatePaused},
		{"suspended", registry.Suspended, ReasonStateSuspended},
		{"closed", registry.Closed, ReasonStateClosed},
		{"unknown", registry.State("archived"), ReasonStateUnknown},
	}
	// seen records the reason Admit returned for each state, not the table's
	// expected reason, so a state refused under another state's reason is
	// caught even when both expected values name the same constant. The
	// subtests below are not parallel, so seen needs no lock.
	seen := make(map[string]registry.State, len(cases))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, prov := newDeps(t)
			d.State = fakeState{state: c.state}

			got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if got.Allow {
				t.Errorf("a %s workspace was admitted", c.state)
			}
			if got.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, c.reason)
			}
			if other, ok := seen[got.Reason]; ok {
				t.Errorf("Admit refuses %q and %q with the same reason %q", other, c.state, got.Reason)
			}
			seen[got.Reason] = c.state
			if got.State != "" {
				t.Errorf("State = %q, want no state move", got.State)
			}
			if prov.calls != 0 {
				t.Errorf("provider called %d times, want 0", prov.calls)
			}
		})
	}
}

func TestAdmitSpendAndRateLimit(t *testing.T) {
	t.Parallel()
	t.Run("spend level", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			cost   float64
			reason string
		}{
			{"at", 10, ReasonSpendAt},
			{"over", 10.5, ReasonSpendOver},
			{"unknown", math.NaN(), ReasonSpendUnknown},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				d, prov := newDeps(t)
				d.Spend = fakeSpend{
					records: []spendcap.Record{{Workspace: ws, EndedAt: noon().Add(-time.Hour), CostUSD: c.cost}},
					cap:     spendcap.Cap{Workspace: ws, DailyUSD: 10},
				}

				got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
				if err != nil {
					t.Fatalf("Admit: %v", err)
				}
				if got.Allow {
					t.Errorf("a spend at %q was admitted", c.name)
				}
				if got.Reason != c.reason {
					t.Errorf("Reason = %q, want %q", got.Reason, c.reason)
				}
				if prov.calls != 0 {
					t.Errorf("provider called %d times, want 0", prov.calls)
				}
			})
		}
	})
	t.Run("empty rate bucket", func(t *testing.T) {
		t.Parallel()
		d, prov := newDeps(t)
		if v := d.Limiter.Allow(abuse.Dispatch, ws, spendcap.Under); !v.Allowed {
			t.Fatalf("draining the bucket: first dispatch refused (%q)", v.Reason)
		}

		got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if got.Allow {
			t.Error("an empty dispatch bucket admitted")
		}
		if got.Reason != ReasonRate {
			t.Errorf("Reason = %q, want %q", got.Reason, ReasonRate)
		}
		if prov.calls != 0 {
			t.Errorf("provider called %d times, want 0", prov.calls)
		}
	})
}

func TestAdmitKeyRefusalPauses(t *testing.T) {
	t.Parallel()
	t.Run("refused key", func(t *testing.T) {
		t.Parallel()
		d, prov := newDeps(t)
		prov.err = fmt.Errorf("%w: revoked", keycheck.ErrRefused)

		got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if got.Allow {
			t.Error("a refused key admitted")
		}
		if got.State != registry.Paused {
			t.Errorf("State = %q, want the move to %q", got.State, registry.Paused)
		}
		if got.Reason != keycheck.ReasonPausedKey {
			t.Errorf("Reason = %q, want %q", got.Reason, keycheck.ReasonPausedKey)
		}
		if got.Warning != "" {
			t.Errorf("Warning = %q, want none on a refusal", got.Warning)
		}
		if prov.calls != 1 {
			t.Errorf("provider called %d times, want 1", prov.calls)
		}
		assertNoKey(t, got)
	})
	t.Run("transient provider error", func(t *testing.T) {
		t.Parallel()
		d, prov := newDeps(t)
		prov.err = errors.New("dial tcp 10.0.0.1:443: i/o timeout")

		got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if !got.Allow {
			t.Errorf("a provider outage refused dispatch: %q", got.Reason)
		}
		if got.Warning != keycheck.WarningUnverified {
			t.Errorf("Warning = %q, want %q", got.Warning, keycheck.WarningUnverified)
		}
		if got.State != "" || got.Reason != "" {
			t.Errorf("State = %q, Reason = %q; a warning moves nothing", got.State, got.Reason)
		}
		if prov.calls != 1 {
			t.Errorf("provider called %d times, want 1", prov.calls)
		}
		assertNoKey(t, got)
	})
}

// assertNoKey checks that no part of the stored key reached the Decision.
func assertNoKey(t *testing.T, d Decision) {
	t.Helper()
	if strings.Contains(fmt.Sprintf("%+v", d), keyValue) {
		t.Errorf("Decision %+v carries the key value", d)
	}
}

func TestAdmitFailsClosedOnSourceError(t *testing.T) {
	t.Parallel()
	stateErr := errors.New("state database down")
	spendErr := errors.New("usage query failed")
	cases := []struct {
		name   string
		mutate func(*Deps)
		reason string
		want   error
	}{
		{
			name:   "state source error",
			mutate: func(d *Deps) { d.State = fakeState{err: stateErr} },
			reason: ReasonStateError,
			want:   stateErr,
		},
		{
			name:   "spend source error",
			mutate: func(d *Deps) { d.Spend = fakeSpend{err: spendErr} },
			reason: ReasonSpendError,
			want:   spendErr,
		},
		{
			name:   "cap for another workspace",
			mutate: func(d *Deps) { d.Spend = fakeSpend{cap: spendcap.Cap{Workspace: "ws-b", DailyUSD: 10}} },
			reason: ReasonSpendForeign,
			want:   ErrCapWorkspace,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, prov := newDeps(t)
			c.mutate(&d)

			got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
			if got.Allow {
				t.Errorf("an unreadable source admitted (%q)", got.Reason)
			}
			if got.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, c.reason)
			}
			if prov.calls != 0 {
				t.Errorf("provider called %d times, want 0", prov.calls)
			}
		})
	}
}

func TestAdmitRefusesIncompleteInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(*Deps)
		workspace string
		noTick    bool
		reason    string
		want      error
	}{
		{"no workspace", func(*Deps) {}, "", false, ReasonNoWorkspace, ErrNoWorkspace},
		{"no state source", func(d *Deps) { d.State = nil }, ws, false, ReasonNoState, ErrNoState},
		{"no spend source", func(d *Deps) { d.Spend = nil }, ws, false, ReasonNoSpend, ErrNoSpend},
		{"no limiter", func(d *Deps) { d.Limiter = nil }, ws, false, ReasonNoLimiter, ErrNoLimiter},
		{"no clock", func(d *Deps) { d.Now = nil }, ws, false, ReasonNoClock, ErrNoClock},
		{"no key store", func(d *Deps) { d.Store = nil }, ws, false, ReasonKeyError, keycheck.ErrNoStore},
		{"no provider", func(d *Deps) { d.Provider = nil }, ws, false, ReasonKeyError, keycheck.ErrNoProvider},
		{"no tick", func(*Deps) {}, ws, true, ReasonKeyError, keycheck.ErrNoTick},
		{"key never stored", func(d *Deps) { d.Store = keystore.NewFake() }, ws, false, ReasonKeyError, keystore.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, prov := newDeps(t)
			c.mutate(&d)
			tick := keycheck.NewTick()
			if c.noTick {
				tick = nil
			}

			got, err := Admit(context.Background(), d, c.workspace, tick)
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
			if got.Allow {
				t.Errorf("incomplete input admitted (%q)", got.Reason)
			}
			if got.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, c.reason)
			}
			if prov.calls != 0 {
				t.Errorf("provider called %d times, want 0", prov.calls)
			}
		})
	}
}

func TestAdmitRefusesCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, prov := newDeps(t)

	got, err := Admit(ctx, d, ws, keycheck.NewTick())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got.Allow {
		t.Error("a canceled caller admitted")
	}
	if got.Reason != ReasonCanceled {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonCanceled)
	}
	if prov.calls != 0 {
		t.Errorf("provider called %d times, want 0", prov.calls)
	}
}

func TestAdmitAllowsActiveUnderCap(t *testing.T) {
	t.Parallel()
	d, prov := newDeps(t)

	got, err := Admit(context.Background(), d, ws, keycheck.NewTick())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !got.Allow {
		t.Fatalf("an active workspace under its cap was refused: %q", got.Reason)
	}
	if got.State != "" || got.Reason != "" || got.Warning != "" {
		t.Errorf("Decision = %+v, want a clean admission", got)
	}
	if prov.calls != 1 {
		t.Errorf("provider called %d times, want 1", prov.calls)
	}
}

func TestAdmitChecksTheKeyOncePerTick(t *testing.T) {
	t.Parallel()
	d, prov := newDeps(t)
	d.Limiter = abuse.New(abuse.Config{DispatchRate: 1, DispatchBurst: 2}, fixedClock{})
	tick := keycheck.NewTick()

	for i := range 2 {
		got, err := Admit(context.Background(), d, ws, tick)
		if err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
		if !got.Allow {
			t.Fatalf("Admit %d: refused (%q)", i, got.Reason)
		}
	}
	if prov.calls != 1 {
		t.Errorf("provider called %d times, want the tick's one check", prov.calls)
	}
}
