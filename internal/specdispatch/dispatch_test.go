package specdispatch

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestEligible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		edit func(*Spec)
		ok   bool
		why  string
	}{
		{func(s *Spec) {}, true, ""},
		// The label spec and type feature are retired: neither admits or keeps
		// out a candidate (gt-mmsr2). The type whitelist is what admits the
		// feature bead, not the retired label.
		{func(s *Spec) { s.Labels = nil }, true, ""},
		{func(s *Spec) { s.Type = "feature" }, true, ""},
		{func(s *Spec) { s.Type = "bug" }, true, ""},
		{func(s *Spec) { s.Type = "Feature" }, true, ""},
		{func(s *Spec) { s.Status = "in_progress" }, false, "status in_progress"},
		{func(s *Spec) { s.Status = "deferred" }, false, "deferred"},
		{func(s *Spec) { s.Assignee = "gastown/polecats/ruby" }, false, "assigned"},
		{func(s *Spec) { s.Type = "epic" }, false, "not a work bead: type epic"},
		{func(s *Spec) { s.Type = "wisp" }, false, "not a work bead: type wisp"},
		{func(s *Spec) { s.Type = "molecule"; s.Ephemeral = true }, false, "not a work bead: wisp"},
		{func(s *Spec) { s.Type = "chore" }, false, "type chore"},
		{func(s *Spec) { s.Type = "docs" }, false, "type docs"},
		{func(s *Spec) { s.Labels = []string{"gt:agent"} }, false, "not a work bead: label gt:agent"},
		{func(s *Spec) { s.Labels = append(s.Labels, "gt:ready-to-land") }, false, "label gt:ready-to-land"},
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-human") }, false, "label needs-human"},
		{func(s *Spec) { s.Labels = append(s.Labels, "Needs-Mayor-Review") }, false, "label needs-mayor-review"},
		{func(s *Spec) { s.Priority = 2 }, true, ""},
		{func(s *Spec) { s.Priority = 3 }, false, "priority P3 outside the ceiling P2"},
		{func(s *Spec) { s.Priority = -1 }, false, "priority P-1 outside the ceiling P2"},
	}
	for i, tc := range cases {
		s := goodSpec()
		tc.edit(&s)
		ok, why := Eligible(s, 2)
		if ok != tc.ok || !strings.Contains(why, tc.why) {
			t.Errorf("case %d: Eligible = %v %q, want %v %q", i, ok, why, tc.ok, tc.why)
		}
	}
}

// The ceiling is the operator's (polecat_pool.max_priority), not a constant:
// the same bead is refused at P2 and taken at P4 (gt-h2kyc).
func TestEligibleHonorsTheConfiguredCeiling(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Priority = 4
	if ok, why := Eligible(s, 2); ok {
		t.Errorf("P4 bead eligible at the default ceiling: %q", why)
	}
	if ok, why := Eligible(s, 4); !ok {
		t.Errorf("P4 bead refused at a P4 ceiling: %q", why)
	}
}

func TestIsDispatchType(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		typ  string
		want bool
	}{{"task", true}, {"bug", true}, {"feature", true}, {"Bug", true}, {" task ", true}, {"epic", false}, {"chore", false}, {"", false}} {
		if got := IsDispatchType(tt.typ); got != tt.want {
			t.Errorf("IsDispatchType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestOrderIsPriorityThenCreatedThenID(t *testing.T) {
	t.Parallel()
	specs := []Spec{
		{ID: "gt-c", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
		{ID: "gt-b", Priority: 1, CreatedAt: "2026-09-29T12:00:00Z"},
		{ID: "gt-a", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
		{ID: "gt-d", Priority: 1, CreatedAt: "2026-09-29T11:00:00Z"},
		{ID: "gt-e", Priority: 2, CreatedAt: "garbage"},
		{ID: "gt-f", Priority: 2, CreatedAt: "2026-09-28T10:00:00Z"},
	}
	Order(specs)
	var got []string
	for _, s := range specs {
		got = append(got, s.ID)
	}
	if want := "gt-d gt-b gt-f gt-a gt-c gt-e"; strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

func baseBudget() Budget {
	return Budget{
		Seats: []Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "claude-sonnet", Cap: 2},
		},
		Now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
}

func TestChooseSeat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		live   map[string]int
		edit   func(*Budget)
		agent  string
		skip   bool
		reason string
	}{
		{name: "takes the first free seat", agent: "deepseek-flash", reason: "seat deepseek-flash 1/2"},
		{name: "first seat full", live: map[string]int{"deepseek-flash": 2}, agent: "claude-sonnet"},
		{name: "over cap counts as full", live: map[string]int{"deepseek-flash": 5, "claude-sonnet": 9}, skip: true, reason: "seats full"},
		{name: "cap zero is closed", edit: func(b *Budget) { b.Seats[0].Cap = 0 }, agent: "claude-sonnet"},
		{name: "min spawn gap", edit: func(b *Budget) { b.MinSpawnGap = 4 * time.Minute; b.NewestSpawn = b.Now.Add(-time.Minute) }, skip: true, reason: "min_spawn_gap"},
		{name: "gap elapsed", edit: func(b *Budget) { b.MinSpawnGap = 4 * time.Minute; b.NewestSpawn = b.Now.Add(-5 * time.Minute) }, agent: "deepseek-flash"},
		{name: "no seats", edit: func(b *Budget) { b.Seats = nil }, skip: true, reason: "no seats"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := baseBudget()
			b.SetLive(tc.live)
			if tc.edit != nil {
				tc.edit(&b)
			}
			got := ChooseSeat(b, goodSpec())
			if got.Skip != tc.skip || got.Agent != tc.agent || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("ChooseSeat = %+v, want agent %q skip %v reason ~%q", got, tc.agent, tc.skip, tc.reason)
			}
		})
	}
}

// The pro seat: a bead carrying pro_label goes only to the seat that reserved
// it, and a plain bead never goes to that seat (gt-tq6l, carried over from
// seat-refill).
func TestChooseSeatReservedLabel(t *testing.T) {
	t.Parallel()
	b := Budget{
		Seats: []Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "deepseek-pro", Cap: 1, Label: "needs-pro"},
		},
		Now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
	plain := goodSpec()
	if got := ChooseSeat(b, plain); got.Agent != "deepseek-flash" {
		t.Errorf("plain bead took %+v, want the pool seat", got)
	}

	pro := goodSpec()
	pro.Labels = []string{"needs-pro"}
	if got := ChooseSeat(b, pro); got.Agent != "deepseek-pro" {
		t.Errorf("needs-pro bead took %+v, want the pro seat", got)
	}

	// With the pool seat full, a plain bead still does not reach the pro seat:
	// it is a no-seat skip, not a dispatch to the reserved seat.
	b.Seats[0].Live = 2
	got := ChooseSeat(b, plain)
	if !got.Skip || !strings.Contains(got.Reason, "no free seat takes this bead") {
		t.Errorf("plain bead with the pool full = %+v, want a skip", got)
	}
	if got := ChooseSeat(b, pro); got.Agent != "deepseek-pro" {
		t.Errorf("needs-pro bead took %+v, want the pro seat", got)
	}

	// The label matcher ignores case and padding, like every other label read.
	pro.Labels = []string{" Needs-Pro "}
	if got := ChooseSeat(b, pro); got.Agent != "deepseek-pro" {
		t.Errorf("padded needs-pro bead took %+v", got)
	}
}

func TestBudgetBumpAndPicture(t *testing.T) {
	t.Parallel()
	b := baseBudget()
	b.SetLive(map[string]int{"claude-sonnet": 1})
	b.Bump("claude-sonnet", b.Now)
	if b.Seats[1].Live != 2 || !b.NewestSpawn.Equal(b.Now) {
		t.Fatalf("bump: %+v", b)
	}
	if got := b.Picture(); got != "deepseek-flash 0/2, claude-sonnet 2/2" {
		t.Errorf("picture = %q", got)
	}
}

func TestRetryOnContention(t *testing.T) {
	t.Parallel()
	contention := errors.New("bd mol bond: Error 1213 (40001): serialization failure: this transaction conflicts")
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }
	jitter := rand.New(rand.NewSource(1))

	t.Run("succeeds after contention", func(t *testing.T) {
		slept = nil
		calls := 0
		n, err := RetryOnContention(RetryAttempts, sleep, jitter, func() error {
			calls++
			if calls < 3 {
				return contention
			}
			return nil
		})
		if err != nil || n != 3 || len(slept) != 2 {
			t.Fatalf("n=%d err=%v slept=%v", n, err, slept)
		}
		for _, d := range slept {
			if d < RetryBackoffMin/2 || d > RetryBackoffMax {
				t.Errorf("backoff %v outside bounds", d)
			}
		}
	})
	t.Run("gives up after attempts", func(t *testing.T) {
		slept = nil
		calls := 0
		n, err := RetryOnContention(RetryAttempts, sleep, jitter, func() error { calls++; return contention })
		if !IsSerializationFailure(err) || n != RetryAttempts || calls != RetryAttempts || len(slept) != RetryAttempts-1 {
			t.Fatalf("n=%d calls=%d err=%v slept=%d", n, calls, err, len(slept))
		}
	})
	t.Run("other errors do not retry", func(t *testing.T) {
		slept = nil
		calls := 0
		n, err := RetryOnContention(RetryAttempts, sleep, jitter, func() error { calls++; return errors.New("connection refused") })
		if err == nil || n != 1 || calls != 1 || len(slept) != 0 {
			t.Fatalf("n=%d calls=%d err=%v", n, calls, err)
		}
	})
}
