package specdispatch

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
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
		// A bead mid-submission wears the READY TO LAND block before it wears
		// the label gt done writes second, so the block is the signal that
		// survives a read the label write outran (gt-kr5xv). The field is the
		// bead's own record: no label, no assignee, and still no candidate.
		{func(s *Spec) { s.SubmittedForLanding = true }, false, "submitted for landing"},
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-human") }, false, "label needs-human"},
		// The hand-applied crew-only label: a person lands those beads,
		// so no seat takes one (gt-k1g3e). The label is read before the ceiling,
		// so the refusal names the label even for a P3.
		{func(s *Spec) { s.Labels = append(s.Labels, CrewOnlyLabel) }, false, "label crew-only"},
		{func(s *Spec) { s.Labels = append(s.Labels, "Crew-Only"); s.Priority = 3 }, false, "label crew-only"},
		// crew-only's sibling, and the label a landing rejection leaves in
		// place: the bead's branch comes from a person, so the bead is not a
		// seat's to take (gt-me800). rework beside it does not readmit the
		// bead — that label alone is ordinary work back to a polecat, while
		// this one is a hold the dispatcher names even for a P3 (the crew-only
		// cases above).
		{func(s *Spec) { s.Labels = append(s.Labels, dispatch.HumanOnlyLabel) }, false, "label human-only"},
		{func(s *Spec) {
			s.Labels = append(s.Labels, "Human-Only", "rework")
			s.Priority = 3
		}, false, "label human-only"},
		// The reviewer label was retired with the role (gt-rwp7z.11): a bead
		// still carrying it is held by nothing, so it dispatches as ordinary
		// ready work.
		{func(s *Spec) { s.Labels = append(s.Labels, "Needs-Mayor-Review") }, true, ""},
		// The shared hold rule (dispatch.DispatchHoldFields). gt:needs-human is
		// the spelling internal/land writes and the one excludedLabels never
		// carried, so the bare "needs-human" case above does not cover it
		// (gt-lxxo4).
		{func(s *Spec) { s.Labels = append(s.Labels, "gt:needs-human") }, false, "label gt:needs-human"},
		// needs-pro was the retired pro seat's selector. With no reserved seat
		// left, the shared rule's hold on it stands (gt-lxxo4).
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-pro") }, false, "label needs-pro"},
		// The operator's mark on a bead nobody is assigned: the sling guard
		// refuses it late; the dispatcher must skip it here (gt-21pl0).
		{func(s *Spec) { s.Labels = append(s.Labels, "operator") }, false, "label operator"},
		{func(s *Spec) {
			s.Labels = append(s.Labels, "operator", "needs-pro")
		}, false, "label operator"},
		// A ruling in prose is a hold wherever it is written (gt-tq6l). The
		// marker has to begin the line, decoration aside. The ruling the
		// retired role wrote names its author, not a marker the dispatcher
		// still reads, so that spelling no longer holds a bead (gt-rwp7z.11).
		{func(s *Spec) { s.Design = "Some context.\n\n- do not redispatch: park it." }, false, "do not redispatch in design"},
		{func(s *Spec) { s.Design = "## MAYOR DESIGN DECISION: park it." }, true, ""},
		{func(s *Spec) { s.Notes = "- do not redispatch" }, false, "do not redispatch in notes"},
		// A note that only quotes the wording back is not a ruling, and an
		// unassigned bead with no marker stays the dispatcher's (the control
		// case is the empty edit at the top of the table).
		{func(s *Spec) { s.Notes = "this note explains the do-not-redispatch marker" }, true, ""},
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
		{"human-only label", func(s *Spec) { s.Labels = []string{dispatch.HumanOnlyLabel} }},
		{"operator label", func(s *Spec) { s.Labels = []string{"operator"} }},
		{"pinned status", func(s *Spec) { s.Status = "pinned" }},
		{"design ruling", func(s *Spec) { s.Design = "do not redispatch: park it." }},
		{"notes ruling", func(s *Spec) { s.Notes = "do not redispatch" }},
	}
	for _, tc := range markers {
		s := goodSpec()
		tc.edit(&s)
		if want := dispatch.DispatchHoldFields(s.Status, s.Labels, s.Assignee, s.Design, s.Notes); want == "" {
			t.Fatalf("%s: the shared rule asserts no hold; this test is stale", tc.name)
		}
		if ok, why := Eligible(s, 2); ok {
			t.Errorf("%s: Eligible = true (%q), want a hold", tc.name, why)
		}
	}
}

// The ready query filters the excluded labels the tick does not have to
// report, and carries the rest: a board-carried label's bead reaches the tick
// so its skip line names the label, and every other excluded label is filtered
// server-side. A label in neither list would be fetched and refused for
// nothing; one in both would never reach the skip line (gt-q6zoo, gt-k1g3e).
func TestExcludedLabelsSplitIntoFilteredAndCarried(t *testing.T) {
	t.Parallel()
	carried := BoardCarriedLabels()
	ready := ReadyQueryLabels()
	for _, l := range ExcludedLabels() {
		inCarried := hasLabel(carried, l)
		inReady := hasLabel(ready, l)
		if inCarried == inReady {
			t.Errorf("%s: board-carried %v, ready-query filtered %v; want exactly one", l, inCarried, inReady)
		}
	}
	for _, want := range []string{CrewOnlyLabel, dispatch.HumanOnlyLabel} {
		if !hasLabel(carried, want) {
			t.Errorf("carried = %v, want %s: the tick has to name the refusal", carried, want)
		}
	}
	if len(carried)+len(ready) != len(ExcludedLabels()) {
		t.Errorf("carried %v + ready %v do not make up ExcludedLabels %v", carried, ready, ExcludedLabels())
	}
}

// A board-carried label is what makes a refusal reportable, and only a bead
// wearing one: the read has to leave every other refusal a silent drop
// (gt-k1g3e).
func TestBoardCarriedFollowsTheLabels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		labels []string
		want   bool
	}{
		{nil, false},
		{[]string{"review-deep"}, false},
		{[]string{CrewOnlyLabel}, true},
		{[]string{"Crew-Only"}, true},
		{[]string{dispatch.HumanOnlyLabel}, true},
		{[]string{"Human-Only"}, true},
		{[]string{"needs-human", CrewOnlyLabel}, true},
		{[]string{DispatchFailedLabel}, true},
	} {
		s := goodSpec()
		s.Labels = tc.labels
		if got := BoardCarried(s); got != tc.want {
			t.Errorf("BoardCarried(%v) = %v, want %v", tc.labels, got, tc.want)
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

// randForIndex returns a source whose next draw among n is want, so a test
// can name the seat the dispatcher's random draw takes without depending on
// the package's own source.
func randForIndex(want, n int) *rand.Rand {
	for seed := int64(0); ; seed++ {
		if rand.New(rand.NewSource(seed)).Intn(n) == want {
			return rand.New(rand.NewSource(seed))
		}
	}
}

func baseBudget() Budget {
	return Budget{
		Seats: []Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "claude-sonnet", Cap: 2},
		},
		Now:  time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		Rand: randForIndex(0, 2),
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
		{name: "takes a free seat", agent: "deepseek-flash", reason: "seat deepseek-flash 1/2"},
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

// The draw is uniform among the free seats of the models in the mix: an
// unlabelled bead does not always take the first seat in the list, and every
// free model's seat is reachable. A seeded source makes the choice
// reproducible, which is how the acceptance is pinned (gt-kr91q).
func TestChooseSeatDrawsAmongFreeSeats(t *testing.T) {
	t.Parallel()
	b := baseBudget()
	seen := map[string]bool{}
	for seed := int64(0); seed < 20; seed++ {
		b.Rand = rand.New(rand.NewSource(seed))
		got := ChooseSeat(b, goodSpec())
		if got.Skip {
			t.Fatalf("seed %d: no seat drawn: %s", seed, got.Reason)
		}
		seen[got.Agent] = true
	}
	for _, agent := range []string{"deepseek-flash", "claude-sonnet"} {
		if !seen[agent] {
			t.Errorf("20 seeds never drew %s: the draw is not among the mix's free seats", agent)
		}
	}

	// A full seat leaves the draw to the other one, and a pinned bead's label
	// narrows the draw to that agent's seat (no fallback).
	b.SetLive(map[string]int{"deepseek-flash": 2})
	if got := ChooseSeat(b, goodSpec()); got.Agent != "claude-sonnet" {
		t.Errorf("the only free seat was not drawn: %+v", got)
	}
	pinned := goodSpec()
	pinned.Labels = []string{"agent:claude-sonnet"}
	b.SetLive(map[string]int{})
	if got := ChooseSeat(b, pinned); got.Skip || got.Agent != "claude-sonnet" {
		t.Errorf("pinned bead drew %+v, want the claude-sonnet seat", got)
	}
}

// TestChooseSeatTownTotal pins the cap beside the per-agent ones (gt-bcpj5):
// max_total bounds the seats together, so two agents each inside their own cap
// are still refused when the town is at its total — the 5 live polecats two
// agents under their own caps produced where 3 was wanted. A town without a
// total dispatches exactly as before.
func TestChooseSeatTownTotal(t *testing.T) {
	t.Parallel()
	b := baseBudget() // flash at 2, sonnet at 2: room on both seats
	b.MaxTotal = 2
	b.SetLive(map[string]int{"deepseek-flash": 1, "claude-sonnet": 1})
	got := ChooseSeat(b, goodSpec())
	if !got.Skip {
		t.Fatalf("two seats under their own caps filled the town total: %+v", got)
	}
	if want := "over the town total: 2/2 live polecats across every seat"; got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}
	if got, want := b.Picture(), "deepseek-flash 1/2, claude-sonnet 1/2, total 2/2"; got != want {
		t.Errorf("Picture() = %q, want %q", got, want)
	}

	// A seat frees: the total is the only thing holding the dispatch.
	b.SetLive(map[string]int{"deepseek-flash": 1})
	if got := ChooseSeat(b, goodSpec()); got.Skip {
		t.Fatalf("a town under its total dispatches: %+v", got)
	}

	// The count is the town's, not the seats': a polecat on an agent no seat
	// names fills the total like any other.
	b.SetLive(map[string]int{"gpt-5": 1, "deepseek-flash": 1})
	if got := ChooseSeat(b, goodSpec()); !got.Skip || !strings.Contains(got.Reason, "2/2") {
		t.Fatalf("a live polecat off the seats must count toward the town's total: %+v", got)
	}

	// max_total 0 is the pool's silence, not a cap of zero.
	b.MaxTotal = 0
	b.SetLive(map[string]int{"deepseek-flash": 1, "claude-sonnet": 1})
	if got := ChooseSeat(b, goodSpec()); got.Skip {
		t.Fatalf("max_total 0 must not cap the town: %+v", got)
	}

	// Bump moves the total with the seat, so a tick that slings twice counts
	// two polecats and not one.
	b.MaxTotal = 3
	b.SetLive(map[string]int{})
	b.Bump("deepseek-flash", b.Now)
	b.Bump("claude-sonnet", b.Now)
	if b.LiveTotal != 2 {
		t.Fatalf("LiveTotal = %d after two bumps, want 2", b.LiveTotal)
	}
	if got := ChooseSeat(b, goodSpec()); got.Skip {
		t.Fatalf("two polecats under a total of 3: %+v", got)
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

// A budget with no Rand is what the production tick runs: the draw falls back
// to the package source, which must still reach every free seat.
func TestChooseSeatDefaultSourceReachesEveryFreeSeat(t *testing.T) {
	t.Parallel()
	b := baseBudget()
	b.Rand = nil
	seen := map[string]bool{}
	// Each draw lands on a given seat with probability 1/2, so missing one seat
	// in 200 draws has probability 2^-199.
	for range 200 {
		seen[ChooseSeat(b, goodSpec()).Agent] = true
	}
	if !seen["deepseek-flash"] || !seen["claude-sonnet"] || len(seen) != 2 {
		t.Errorf("200 unseeded draws reached %v, want both free seats", seen)
	}
}

// A bead pinned with an agent: label is slung only onto a seat of that agent. A
// free seat of another agent is not a fallback: the bead waits, and the skip
// says which seat it waits for.
func TestChooseSeatPinnedAgent(t *testing.T) {
	t.Parallel()
	b := Budget{
		Seats: []Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "claude-sonnet", Cap: 2},
		},
		Now:  time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		Rand: randForIndex(0, 2),
	}
	pinned := goodSpec()
	pinned.Labels = []string{"scheduled:deep-review-risk", "agent:claude-sonnet"}

	// Both seats are free: the draw reaches only the Claude seat, because the
	// pin is that agent's.
	if got := ChooseSeat(b, pinned); got.Skip || got.Agent != "claude-sonnet" {
		t.Errorf("pinned bead took %+v, want the claude-sonnet seat", got)
	}
	// A bead without the label draws among both free seats.
	b.Rand = randForIndex(0, 2)
	if got := ChooseSeat(b, goodSpec()); got.Agent != "deepseek-flash" {
		t.Errorf("unpinned bead took %+v, want the drawn free seat", got)
	}

	// The pinned agent's seat is full and the flash seat is free: wait, never fall back.
	b.Seats[1].Live = 2
	got := ChooseSeat(b, pinned)
	if !got.Skip || got.Agent != "" || !strings.Contains(got.Reason, "waiting for a claude-sonnet seat (free seats left alone: deepseek-flash)") {
		t.Errorf("pinned bead with its seat full = %+v, want a skip that names the seat and the free seat it left alone", got)
	}

	// An agent that has no seat in the pool at all is a different, louder skip.
	orphan := goodSpec()
	orphan.Labels = []string{"agent:deepseek-pro"}
	got = ChooseSeat(b, orphan)
	if !got.Skip || !strings.Contains(got.Reason, "deepseek-pro, which has no seat in the pool (free seats left alone: deepseek-flash)") {
		t.Errorf("bead pinned to an agent with no seat = %+v", got)
	}

	// Two different pins conflict and nothing is guessed; the same pin twice, or in
	// another case or padded, is one pin.
	two := goodSpec()
	two.Labels = []string{"agent:claude-sonnet", "agent:deepseek-flash"}
	b.Seats[1].Live = 0
	if got := ChooseSeat(b, two); !got.Skip || !strings.Contains(got.Reason, "conflicting agent: labels") {
		t.Errorf("conflicting pins = %+v, want a skip", got)
	}
	same := goodSpec()
	same.Labels = []string{"agent:claude-sonnet", " Agent:claude-sonnet "}
	if got := ChooseSeat(b, same); got.Skip || got.Agent != "claude-sonnet" {
		t.Errorf("a repeated pin = %+v, want the claude-sonnet seat", got)
	}

	// The town total still caps a pinned bead like any other.
	b.MaxTotal, b.LiveTotal = 2, 2
	if got := ChooseSeat(b, pinned); !got.Skip || !strings.Contains(got.Reason, "over the town total") {
		t.Errorf("pinned bead over the town total = %+v", got)
	}
}

// A pinned-only seat is never drawn for an unlabelled bead, nor for a bead
// pinned to another agent; a bead labeled for its agent still takes it within
// its cap (gt-zor86).
func TestChooseSeatPinnedOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	flash := func(live int) Seat { return Seat{Agent: "deepseek-flash", Cap: 1, Live: live, PinnedOnly: true} }
	sonnet := Seat{Agent: "claude-sonnet", Cap: 2}

	// The pinned-only seat is the only free seat, so an unlabelled bead must
	// skip rather than draw it.
	unlabelled := Budget{Seats: []Seat{flash(0)}, Now: now, Rand: randForIndex(0, 1)}
	if got := ChooseSeat(unlabelled, goodSpec()); !got.Skip || got.Agent != "" || got.Reason != "seats full: deepseek-flash 0/1" {
		t.Errorf("unlabelled bead drew a pinned-only seat, or skipped for another reason: %+v", got)
	}

	// With an open seat of another agent, the unlabelled bead takes that one.
	mixed := Budget{Seats: []Seat{flash(0), sonnet}, Now: now, Rand: randForIndex(0, 1)}
	if got := ChooseSeat(mixed, goodSpec()); got.Skip || got.Agent != "claude-sonnet" {
		t.Errorf("unlabelled bead with a pinned-only and an open seat = %+v, want claude-sonnet", got)
	}

	// A bead pinned to another agent never reaches the pinned-only seat.
	other := goodSpec()
	other.Labels = []string{"agent:claude-sonnet"}
	if got := ChooseSeat(mixed, other); got.Skip || got.Agent != "claude-sonnet" {
		t.Errorf("claude-sonnet bead = %+v, want the claude-sonnet seat", got)
	}
	onlyFlash := Budget{Seats: []Seat{flash(0), {Agent: "claude-sonnet", Cap: 2, Live: 2}}, Now: now}
	if got := ChooseSeat(onlyFlash, other); !got.Skip || got.Agent != "" {
		t.Errorf("claude-sonnet bead drew the pinned-only seat: %+v", got)
	}

	// A bead labelled for the pinned-only seat's agent takes it, within its cap.
	pinned := goodSpec()
	pinned.Labels = []string{"agent:deepseek-flash"}
	if got := ChooseSeat(unlabelled, pinned); got.Skip || got.Agent != "deepseek-flash" {
		t.Errorf("bead pinned to the pinned-only seat = %+v, want deepseek-flash", got)
	}
	full := Budget{Seats: []Seat{flash(1)}, Now: now}
	if got := ChooseSeat(full, pinned); !got.Skip || !strings.Contains(got.Reason, "waiting for a deepseek-flash seat") {
		t.Errorf("pinned bead with its pinned-only seat full = %+v, want a wait", got)
	}

	// The window still governs a pinned-only seat: shut at 21:00, the pinned
	// bead waits for it to open rather than taking it.
	windowed := Seat{Agent: "deepseek-flash", Cap: 1, PinnedOnly: true, Window: nightWindow()}
	shut := Budget{Seats: []Seat{windowed}, Now: time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)}
	if got := ChooseSeat(shut, pinned); !got.Skip || got.Agent != "" || !strings.Contains(got.Reason, "closed until 08:00") {
		t.Errorf("pinned bead on a shut pinned-only seat = %+v, want a closed-until skip", got)
	}
	open := Budget{Seats: []Seat{windowed}, Now: now}
	if got := ChooseSeat(open, pinned); got.Skip || got.Agent != "deepseek-flash" {
		t.Errorf("pinned bead on an open pinned-only seat = %+v, want deepseek-flash", got)
	}
}

// nightWindow is a seat window shut from 20:00 until 08:00 UTC, so a budget
// clock at 21:00 reads it closed and a clock at noon reads it open.
func nightWindow() *config.Window {
	return &config.Window{StartMin: 20 * 60, EndMin: 8 * 60, Loc: time.UTC}
}

func TestClosedSeatTakesNoSpawn(t *testing.T) {
	t.Parallel()
	shut := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	closed := Seat{Agent: "deepseek-flash", Cap: 3, Live: 1, Window: nightWindow()}
	if closed.FreeAt(shut) {
		t.Fatal("a closed seat with room must not be free")
	}
	b := Budget{
		Seats: []Seat{{Agent: "claude-sonnet", Cap: 1, Live: 1}, closed},
		Now:   shut,
		Rand:  randForIndex(0, 2),
	}
	got := ChooseSeat(b, goodSpec())
	if !got.Skip || got.Agent != "" {
		t.Fatalf("every open seat full and one closed: %+v, want a skip", got)
	}
	if want := "deepseek-flash 1/3 (closed until 08:00)"; !strings.Contains(got.Reason, want) {
		t.Errorf("reason %q lacks the closed seat in the picture %q", got.Reason, want)
	}
	// An unpinned bead goes to the open seat, never the closed one.
	b.Seats[0].Live = 0
	if got := ChooseSeat(b, goodSpec()); got.Skip || got.Agent != "claude-sonnet" {
		t.Errorf("unpinned bead took %+v, want the open claude-sonnet seat", got)
	}
}

func TestPinnedBeadWaitsOnClosedSeat(t *testing.T) {
	t.Parallel()
	b := Budget{
		Seats: []Seat{
			{Agent: "claude-sonnet", Cap: 3},
			{Agent: "deepseek-flash", Cap: 3, Window: nightWindow()},
		},
		Now:  time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC),
		Rand: randForIndex(0, 2),
	}
	pinned := goodSpec()
	pinned.Labels = []string{AgentLabelPrefix + "deepseek-flash"}
	got := ChooseSeat(b, pinned)
	if !got.Skip || got.Agent != "" {
		t.Fatalf("pinned bead took %+v; it must wait", got)
	}
	if want := "waiting for a deepseek-flash seat, closed until 08:00 (free seats left alone: claude-sonnet)"; !strings.Contains(got.Reason, want) {
		t.Errorf("reason = %q, want it to contain %q", got.Reason, want)
	}
}

// A seat's window is read at choice time, not snapshotted when the budget was
// built: a tick whose board read and first sling carry it past 20:00 must not
// sling onto the flash seat it saw open at 19:59:58 (gt-t8new). The seating is
// pinned to the flash seat so the closed seat is the only one either choice can
// report; the pinned half is the wait the closed seat already owes.
func TestChooseSeatRechecksTheWindowAtChoiceTime(t *testing.T) {
	t.Parallel()
	b := Budget{
		Seats: []Seat{{Agent: "deepseek-flash", Cap: 2, Window: nightWindow()}},
		Now:   time.Date(2026, 10, 19, 19, 59, 58, 0, time.UTC),
	}
	spec := goodSpec()
	spec.Labels = []string{AgentLabelPrefix + "deepseek-flash"}
	if got := ChooseSeat(b, spec); got.Skip || got.Agent != "deepseek-flash" {
		t.Fatalf("before the boundary = %+v, want the flash seat", got)
	}

	// The board read and contention retries carry the tick across the boundary.
	b.Now = time.Date(2026, 10, 19, 20, 0, 1, 0, time.UTC)
	got := ChooseSeat(b, spec)
	if !got.Skip || got.Agent != "" {
		t.Fatalf("a seat that shut mid-tick took %+v, want a skip", got)
	}
	if want := "closed until 08:00"; !strings.Contains(got.Reason, want) {
		t.Errorf("reason = %q, want it to name the seat closed (%q)", got.Reason, want)
	}

	// An unpinned bead is not drawn onto it either, on the same clock.
	got = ChooseSeat(b, goodSpec())
	if !got.Skip || got.Agent != "" || !strings.Contains(got.Reason, "closed until 08:00") {
		t.Fatalf("unpinned bead at the shut seat = %+v, want a skip naming the window", got)
	}

	// The window is read both ways: a seat shut when the budget was built and
	// open by choice time takes the bead.
	b.Now = time.Date(2026, 10, 20, 8, 0, 1, 0, time.UTC)
	if got := ChooseSeat(b, spec); got.Skip || got.Agent != "deepseek-flash" {
		t.Fatalf("after the window opened = %+v, want the flash seat", got)
	}
}

// A budget with no clock cannot read a windowed seat, so it must not draw one.
// The Closed/OpensAt snapshot made a zero Budget.Now harmless; carrying the
// window must not quietly read it at year one and depend on the hour that
// happens to name (gt-t8new).
func TestChooseSeatZeroClockDoesNotDrawAWindowedSeat(t *testing.T) {
	t.Parallel()
	b := Budget{Seats: []Seat{{Agent: "deepseek-flash", Cap: 2, Window: nightWindow()}}}
	if got := ChooseSeat(b, goodSpec()); !got.Skip || got.Agent != "" || !strings.Contains(got.Reason, "no clock") {
		t.Fatalf("unpinned bead with no clock = %+v, want a skip naming the missing clock", got)
	}
	spec := goodSpec()
	spec.Labels = []string{AgentLabelPrefix + "deepseek-flash"}
	if got := ChooseSeat(b, spec); !got.Skip || got.Agent != "" || !strings.Contains(got.Reason, "closed, no clock") {
		t.Fatalf("pinned bead with no clock = %+v, want a skip naming the missing clock", got)
	}
	if pic := b.Picture(); !strings.Contains(pic, "closed, no clock") {
		t.Errorf("picture = %q, want it to name the window the zero clock cannot read", pic)
	}
}
