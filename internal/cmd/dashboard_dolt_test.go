package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// doltWiringKey numbers the section the wiring test registers, so a -count>1
// run registers a new key rather than panicking on the previous run's.
var doltWiringKey atomic.Int64

// One section failing, one panicking and one working leaves the panel with all
// three, ordered by key, and only the failures carry an Err.
func TestDashboardDoltIsolatesAndOrdersSections(t *testing.T) {
	t.Parallel()

	boom := errors.New("the row query failed")
	rows := dashboard.DoltSection{
		Title: "Rows",
		Head:  []string{"n"},
		Rows:  [][]string{{"7"}},
	}
	readers := []doltSectionReader{
		{key: "rows", read: func(context.Context, time.Time) (dashboard.DoltSection, error) {
			return rows, nil
		}},
		{key: "broken", read: func(context.Context, time.Time) (dashboard.DoltSection, error) {
			return dashboard.DoltSection{}, boom
		}},
		{key: "panics", read: func(context.Context, time.Time) (dashboard.DoltSection, error) {
			panic("the reader broke")
		}},
	}

	panel := doltPanel(readers, "/town", time.Minute)()
	if panel == nil {
		t.Fatal("doltPanel returned no panel")
	}
	keys := make([]string, len(panel.Sections))
	for i, s := range panel.Sections {
		keys[i] = s.Key
	}
	if got, want := strings.Join(keys, ","), "broken,panics,rows"; got != want {
		t.Errorf("sections are ordered %q, want %q", got, want)
	}
	if panel.At.IsZero() {
		t.Error("the panel has no read time")
	}

	broken, panics, fine := panel.Sections[0], panel.Sections[1], panel.Sections[2]
	if broken.Err != boom.Error() {
		t.Errorf("the failed section's Err is %q, want %q", broken.Err, boom.Error())
	}
	if !strings.Contains(panics.Err, "panicked") {
		t.Errorf("the panicking section's Err is %q, want it to name the panic", panics.Err)
	}
	if broken.Head != nil || broken.Rows != nil {
		t.Errorf("the failed section carries a table: Head=%v Rows=%v", broken.Head, broken.Rows)
	}
	if fine.Err != "" {
		t.Errorf("the working section has Err %q, want none", fine.Err)
	}
	if fine.Title != rows.Title || len(fine.Head) != 1 || len(fine.Rows) != 1 || fine.Rows[0][0] != "7" {
		t.Errorf("the working section was changed: %+v, want %+v", fine, rows)
	}
}

// A read that outlives the timeout leaves its section an Err naming the
// timeout, and the panel is returned while the read is still blocked.
func TestDashboardDoltTimesOutOneSection(t *testing.T) {
	t.Parallel()

	short := 100 * time.Millisecond
	// The slow read reports the deadline its context was given and then blocks
	// until the test ends, so the panel must return without it.
	type bound struct {
		ok   bool
		span time.Duration
	}
	bounds := make(chan bound, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	readers := []doltSectionReader{
		{key: "slow", read: func(ctx context.Context, now time.Time) (dashboard.DoltSection, error) {
			deadline, ok := ctx.Deadline()
			bounds <- bound{ok: ok, span: deadline.Sub(now)}
			<-release
			return dashboard.DoltSection{Title: "Slow"}, nil
		}},
		{key: "fast", read: func(context.Context, time.Time) (dashboard.DoltSection, error) {
			return dashboard.DoltSection{Title: "Fast"}, nil
		}},
	}

	panel := doltPanel(readers, "/town", short)()
	if len(panel.Sections) != 2 {
		t.Fatalf("the panel has %d sections, want 2", len(panel.Sections))
	}
	fast, slow := panel.Sections[0], panel.Sections[1]
	if fast.Key != "fast" || fast.Err != "" || fast.Title != "Fast" {
		t.Errorf("the fast section is %+v, want it intact", fast)
	}
	if slow.Key != "slow" {
		t.Fatalf("the panel's second section is %q, want slow", slow.Key)
	}
	if !strings.Contains(slow.Err, "timed out") || !strings.Contains(slow.Err, short.String()) {
		t.Errorf("the slow section's Err is %q, want it to name the %v timeout", slow.Err, short)
	}
	if slow.Title != "" {
		t.Errorf("the timed-out section kept the read's title %q", slow.Title)
	}
	got := <-bounds
	if !got.ok {
		t.Fatal("the read's context had no deadline, want the section timeout")
	}
	// The read's own deadline is the bound plus the goroutine's start delay: a
	// panel built with another bound would leave the read running past the time
	// its Err names.
	if got.span < short || got.span >= 10*short {
		t.Errorf("the read's deadline is %v after the panel's read time, want about %v", got.span, short)
	}
}

// The town's dashboard gets its panel from dashboardDolt, which hands the
// registered sections, the town root and doltSectionTimeout to the reader: the
// section registered here appears, its read finds the root, and its deadline is
// the package's bound.
func TestDashboardDoltWiresRegistrationsTheTownRootAndTheBound(t *testing.T) {
	t.Parallel()

	type seen struct {
		ok   bool
		root string
		span time.Duration
	}
	got := make(chan seen, 1)
	key := fmt.Sprintf("gt-3trzy0-wiring-%d", doltWiringKey.Add(1))
	registerDoltSection(key, func(ctx context.Context, now time.Time) (dashboard.DoltSection, error) {
		root, hasRoot := doltTownRoot(ctx)
		deadline, hasDeadline := ctx.Deadline()
		got <- seen{ok: hasRoot && hasDeadline, root: root, span: deadline.Sub(now)}
		return dashboard.DoltSection{Title: "Wiring"}, nil
	})

	panel := dashboardDolt("/town/wiring")()
	var mine *dashboard.DoltSection
	for i := range panel.Sections {
		if panel.Sections[i].Key == key {
			mine = &panel.Sections[i]
		}
	}
	if mine == nil {
		t.Fatalf("the panel has no %q section: the registered sections did not reach it", key)
	}
	if mine.Title != "Wiring" || mine.Err != "" {
		t.Errorf("the wiring section is %+v, want the read's title and no Err", *mine)
	}
	s := <-got
	if !s.ok {
		t.Fatal("the read got no town root or no deadline")
	}
	if s.root != "/town/wiring" {
		t.Errorf("the read saw town root %q, want %q", s.root, "/town/wiring")
	}
	if s.span < doltSectionTimeout || s.span >= 2*doltSectionTimeout {
		t.Errorf("the read's deadline is %v after the panel's read time, want about %v", s.span, doltSectionTimeout)
	}
}

// A registration that cannot name and read one section is refused at init.
func TestRegisterDoltSectionRefusesBadRegistrations(t *testing.T) {
	t.Parallel()

	count := func(context.Context, time.Time) (dashboard.DoltSection, error) {
		return dashboard.DoltSection{}, nil
	}
	cases := []struct {
		name  string
		key   string
		read  doltSectionRead
		prior bool
		want  string
	}{
		{name: "no key", key: "", read: count, want: "no key"},
		{name: "no read", key: "s", read: nil, want: "has no read"},
		{name: "duplicate key", key: "s", read: count, prior: true, want: "registered twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDoltSectionRegistry()
			kept := 0
			if tc.prior {
				r.register("s", count)
				kept = 1
			}
			var got string
			func() {
				defer func() {
					if p := recover(); p != nil {
						got = fmt.Sprint(p)
					}
				}()
				r.register(tc.key, tc.read)
			}()
			if !strings.Contains(got, tc.want) {
				t.Errorf("registering a section with %s panicked %q, want it to say %q", tc.name, got, tc.want)
			}
			if n := len(r.snapshot()); n != kept {
				t.Errorf("the refused registration left %d sections, want %d", n, kept)
			}
		})
	}
}
