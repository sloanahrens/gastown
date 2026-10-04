package specdispatch

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
)

func TestEligible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		edit func(*Spec)
		// reserved is the labels the tick's seats reserve; nil is a tick with
		// no reserved seat, which is where the shared rule's hold on
		// needs-pro stands.
		reserved []string
		ok       bool
		why      string
	}{
		{func(s *Spec) {}, nil, true, ""},
		// The label spec and type feature are retired: neither admits or keeps
		// out a candidate (gt-mmsr2). The type whitelist is what admits the
		// feature bead, not the retired label.
		{func(s *Spec) { s.Labels = nil }, nil, true, ""},
		{func(s *Spec) { s.Type = "feature" }, nil, true, ""},
		{func(s *Spec) { s.Type = "bug" }, nil, true, ""},
		{func(s *Spec) { s.Type = "Feature" }, nil, true, ""},
		{func(s *Spec) { s.Status = "in_progress" }, nil, false, "status in_progress"},
		{func(s *Spec) { s.Status = "deferred" }, nil, false, "deferred"},
		{func(s *Spec) { s.Assignee = "gastown/polecats/ruby" }, nil, false, "assigned"},
		{func(s *Spec) { s.Type = "epic" }, nil, false, "not a work bead: type epic"},
		{func(s *Spec) { s.Type = "wisp" }, nil, false, "not a work bead: type wisp"},
		{func(s *Spec) { s.Type = "molecule"; s.Ephemeral = true }, nil, false, "not a work bead: wisp"},
		{func(s *Spec) { s.Type = "chore" }, nil, false, "type chore"},
		{func(s *Spec) { s.Type = "docs" }, nil, false, "type docs"},
		{func(s *Spec) { s.Labels = []string{"gt:agent"} }, nil, false, "not a work bead: label gt:agent"},
		{func(s *Spec) { s.Labels = append(s.Labels, "gt:ready-to-land") }, nil, false, "label gt:ready-to-land"},
		// A bead mid-submission wears the READY TO LAND block before it wears
		// the label gt done writes second, so the block is the signal that
		// survives a read the label write outran (gt-kr5xv). The field is the
		// bead's own record: no label, no assignee, and still no candidate.
		{func(s *Spec) { s.SubmittedForLanding = true }, nil, false, "submitted for landing"},
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-human") }, nil, false, "label needs-human"},
		{func(s *Spec) { s.Labels = append(s.Labels, "Needs-Mayor-Review") }, nil, false, "label needs-mayor-review"},
		// The shared hold rule (dispatch.DispatchHoldFields). gt:needs-human is
		// the spelling internal/land writes and the one excludedLabels never
		// carried, so the bare "needs-human" case above does not cover it
		// (gt-lxxo4).
		{func(s *Spec) { s.Labels = append(s.Labels, "gt:needs-human") }, nil, false, "label gt:needs-human"},
		// needs-pro is the shared rule's hold, but this dispatcher reserves a
		// seat for it (polecat_pool.pro_label): with the seat the bead is
		// routed, with none the hold stands (gt-lxxo4).
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-pro") }, nil, false, "label needs-pro"},
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-pro") }, []string{"needs-pro"}, true, ""},
		// The operator's mark on a bead nobody is assigned: the sling guard
		// refuses it late; the dispatcher must skip it here (gt-21pl0). A
		// reserved seat does not lift it.
		{func(s *Spec) { s.Labels = append(s.Labels, "operator") }, nil, false, "label operator"},
		{func(s *Spec) {
			s.Labels = append(s.Labels, "operator", "needs-pro")
		}, []string{"needs-pro"}, false, "label operator"},
		// A ruling in prose is a hold wherever it is written (gt-tq6l). The
		// marker has to begin the line, decoration aside.
		{func(s *Spec) { s.Design = "Some context.\n\nMAYOR DESIGN DECISION: park it." }, nil, false, "MAYOR DESIGN DECISION in design"},
		{func(s *Spec) { s.Notes = "- do not redispatch" }, nil, false, "do not redispatch in notes"},
		// A note that only quotes the wording back is not a ruling, and an
		// unassigned bead with no marker stays the dispatcher's (the control
		// case is the empty edit at the top of the table).
		{func(s *Spec) { s.Notes = "this note explains the do-not-redispatch marker" }, nil, true, ""},
		{func(s *Spec) { s.Priority = 2 }, nil, true, ""},
		{func(s *Spec) { s.Priority = 3 }, nil, false, "priority P3 outside the ceiling P2"},
		{func(s *Spec) { s.Priority = -1 }, nil, false, "priority P-1 outside the ceiling P2"},
	}
	for i, tc := range cases {
		s := goodSpec()
		tc.edit(&s)
		ok, why := Eligible(s, 2, tc.reserved)
		if ok != tc.ok || !strings.Contains(why, tc.why) {
			t.Errorf("case %d: Eligible = %v %q, want %v %q", i, ok, why, tc.ok, tc.why)
		}
	}
}

// The dispatcher holds exactly what the shared hold rule holds. Each marker
// below is asserted on a bead the dispatcher would otherwise take, and the
// shared rule has to confirm the hold before Eligible's refusal counts — so a
// marker the rule stops asserting, or a rule the dispatcher stops reading,
// fails here rather than silently re-slinging a parked bead (gt-lxxo4).
func TestEligibleFollowsTheSharedHoldRule(t *testing.T) {
	t.Parallel()
	markers := []struct {
		name string
		edit func(*Spec)
	}{
		{"gt:needs-human label", func(s *Spec) { s.Labels = []string{"gt:needs-human"} }},
		{"bare needs-human label", func(s *Spec) { s.Labels = []string{"needs-human"} }},
		{"needs-mayor-review label", func(s *Spec) { s.Labels = []string{"needs-mayor-review"} }},
		{"operator label", func(s *Spec) { s.Labels = []string{"operator"} }},
		{"pinned status", func(s *Spec) { s.Status = "pinned" }},
		{"design ruling", func(s *Spec) { s.Design = "MAYOR DESIGN DECISION: park it." }},
		{"notes ruling", func(s *Spec) { s.Notes = "do not redispatch" }},
	}
	for _, tc := range markers {
		s := goodSpec()
		tc.edit(&s)
		if want := dispatch.DispatchHoldFields(s.Status, s.Labels, s.Assignee, s.Design, s.Notes); want == "" {
			t.Fatalf("%s: the shared rule asserts no hold; this test is stale", tc.name)
		}
		if ok, why := Eligible(s, 2, nil); ok {
			t.Errorf("%s: Eligible = true (%q), want a hold", tc.name, why)
		}
	}
}

// The ceiling is the operator's (polecat_pool.max_priority), not a constant:
// the same bead is refused at P2 and taken at P4 (gt-h2kyc).
func TestEligibleHonorsTheConfiguredCeiling(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Priority = 4
	if ok, why := Eligible(s, 2, nil); ok {
		t.Errorf("P4 bead eligible at the default ceiling: %q", why)
	}
	if ok, why := Eligible(s, 4, nil); !ok {
		t.Errorf("P4 bead refused at a P4 ceiling: %q", why)
	}
}

// A reserved label is routed, not held: the seat that reserves it takes it, so
// the dispatcher must leave it in the candidate set. The exception is per
// label, not wholesale — a bead wearing a reserved label and a hold marker is
// still held.
func TestEligibleRoutesReservedLabelsToTheirSeat(t *testing.T) {
	t.Parallel()
	reserved := []string{"needs-pro", "needs-fast"}

	routed := goodSpec()
	routed.Labels = []string{"Needs-Pro"}
	if ok, why := Eligible(routed, 2, reserved); !ok {
		t.Errorf("needs-pro bead held with a pro seat configured: %q", why)
	}
	// The reservation matches however it was typed, as Seat.takes matches it.
	if ok, why := Eligible(routed, 2, []string{"NEEDS-PRO"}); !ok {
		t.Errorf("case-insensitive reservation not matched: %q", why)
	}

	unseated := goodSpec()
	unseated.Labels = []string{"needs-pro"}
	if ok, why := Eligible(unseated, 2, nil); ok {
		t.Errorf("needs-pro bead eligible with no seat reserving it: %q", why)
	}

	held := goodSpec()
	held.Labels = []string{"needs-pro", "gt:needs-human"}
	if ok, why := Eligible(held, 2, reserved); ok {
		t.Errorf("a hold marker was lifted by a reserved seat: %q", why)
	}
}

// ReservedLabels is the seat selectors Eligible is handed, read from the
// budget so the two cannot drift: a seat added later reserves its label with no
// second list to update (gt-lxxo4).
func TestReservedLabelsFollowsTheSeats(t *testing.T) {
	t.Parallel()
	var b Budget
	if got := b.ReservedLabels(); got != nil {
		t.Errorf("no seats = %v, want none", got)
	}
	b.Seats = []Seat{
		{Agent: "pool", Cap: 2},
		{Agent: "pro", Cap: 1, Label: "needs-pro"},
		{Agent: "pro-copy", Cap: 1, Label: "Needs-Pro"},
		{Agent: "fast", Cap: 1, Label: "needs-fast"},
	}
	got := b.ReservedLabels()
	if len(got) != 2 || got[0] != "needs-pro" || got[1] != "needs-fast" {
		t.Errorf("ReservedLabels = %v, want [needs-pro needs-fast]", got)
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

// TestBudgetPictureNamesDeadHookedSeats pins the operator surface gt-tldj4 adds:
// a seat that holds a crashed polecat — its session gone, its bead still
// hooked, its restart due — names that occupant, so a full seat showing fewer
// live sessions than its number has a stated reason in the tick line and in the
// refusal text. A seat with no dead-hooked occupant reads exactly as before.
func TestBudgetPictureNamesDeadHookedSeats(t *testing.T) {
	t.Parallel()
	b := baseBudget()
	b.SetLive(map[string]int{"deepseek-flash": 2, "claude-sonnet": 1})
	b.SetDeadHooked(map[string]int{"deepseek-flash": 1})
	if got, want := b.Picture(), "deepseek-flash 2/2 (1 dead-hooked), claude-sonnet 1/2"; got != want {
		t.Fatalf("Picture() = %q, want %q", got, want)
	}

	// The refusal a full town prints carries the same note.
	b.Seats[1].Live, b.Seats[1].Cap = 2, 2
	if got := ChooseSeat(b, goodSpec()); !got.Skip || got.Reason != "seats full: deepseek-flash 2/2 (1 dead-hooked), claude-sonnet 2/2" {
		t.Fatalf("ChooseSeat = %+v, want the dead-hooked seat named", got)
	}

	// SetLive resets the breakdown, so a fresh count reads as before.
	b.SetLive(map[string]int{"deepseek-flash": 2, "claude-sonnet": 2})
	if got, want := b.Picture(), "deepseek-flash 2/2, claude-sonnet 2/2"; got != want {
		t.Fatalf("Picture() = %q, want %q", got, want)
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
