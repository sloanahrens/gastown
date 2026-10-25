//go:build integration

package loadtest

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/server"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

const (
	seats      = 12 // concurrent clients, each with its own token and actor
	rounds     = 25 // iterations of the mixed workload per seat
	contested  = 20 // shared issues every seat tries to claim
	maxRuntime = 3 * time.Minute
	// stallBound is far above the milliseconds seen on a healthy run; it catches
	// a lock queue or a stall, not a slow machine.
	stallBound = 2 * time.Second
)

type sample struct {
	op string
	d  time.Duration
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[min(len(ds)-1, int(float64(len(ds))*p))]
}

// Integration tier: one Postgres clone, the real server, twelve seats. Cost is
// a few seconds. It asserts that concurrent work neither errors nor deadlocks,
// that exactly one seat wins each contested claim, and that nothing is lost.
func TestIntegrationManySeatsWorkTogetherWithoutLosingOrDoubling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), maxRuntime)
	defer cancel()
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	srv := httptest.NewServer(server.New(server.Config{Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel()}).Handler())
	defer srv.Close()
	newClient := func(actor string) *client.Client {
		tok, err := pg.IssueToken(ctx, actor, nil)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(client.Config{BaseURL: srv.URL, Token: tok, SchemaLevel: store.SchemaLevel()})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	admin := newClient("town/crew/loadtest")

	// The contested pool: unassigned work every seat will try to claim.
	var pool0 []string
	for i := 0; i < contested; i++ {
		is, err := admin.Create(ctx, api.CreateParams{Rig: "gt", Title: fmt.Sprintf("contested %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		pool0 = append(pool0, is.ID)
	}

	var (
		mu       sync.Mutex
		samples  []sample
		wins     = map[string]int{} // contested id -> seats that claimed it
		refused  int                // claims the guard turned away
		created  int
		unexpect []error
	)
	record := func(op string, start time.Time, err error, expected ...*api.Error) {
		d := time.Since(start)
		mu.Lock()
		defer mu.Unlock()
		samples = append(samples, sample{op, d})
		if err == nil {
			return
		}
		for _, e := range expected {
			if errors.Is(err, e) {
				return
			}
		}
		unexpect = append(unexpect, fmt.Errorf("%s: %w", op, err))
	}

	// Every client is made here, on the test goroutine: the workers below only
	// record, they never call t.Fatal.
	clients := make([]*client.Client, seats)
	for s := range clients {
		clients[s] = newClient(fmt.Sprintf("town/polecats/seat-%02d", s))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for s := 0; s < seats; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actor := fmt.Sprintf("town/polecats/seat-%02d", s)
			c := clients[s]
			<-start
			for r := 0; r < rounds; r++ {
				t0 := time.Now()
				mine, err := c.Create(ctx, api.CreateParams{Rig: "gt", Title: fmt.Sprintf("%s round %d", actor, r), Labels: []string{"load"}})
				record("create", t0, err)
				if err != nil {
					continue
				}
				mu.Lock()
				created++
				mu.Unlock()

				// Contested claim: the guard lets exactly one seat through.
				id := pool0[(s+r)%contested]
				none, inProg := "", "in_progress"
				t0 = time.Now()
				err = c.Update(ctx, id, api.UpdatePatch{Assignee: &actor, Status: &inProg}, api.Guard{IfAssignee: &none})
				record("claim", t0, err, api.ErrGuardFailed)
				mu.Lock()
				if err == nil {
					wins[id]++
				} else if errors.Is(err, api.ErrGuardFailed) {
					refused++
				}
				mu.Unlock()

				t0 = time.Now()
				record("comment", t0, c.AddComment(ctx, mine.ID, "", "working"))
				t0 = time.Now()
				record("label", t0, c.Update(ctx, mine.ID, api.UpdatePatch{AddLabels: []string{"touched"}}, api.Guard{}))
				t0 = time.Now()
				_, err = c.Ready(ctx, api.ReadyFilter{Rig: "gt", Unassigned: true, Limit: 50})
				record("ready", t0, err)
				t0 = time.Now()
				_, err = c.ShowMany(ctx, []string{mine.ID, id})
				record("show-many", t0, err)
				t0 = time.Now()
				_, err = c.DispatchBoard(ctx, api.DispatchBoardRequest{Rigs: []string{"gt"}, HeldStatuses: []string{"in_progress"}})
				record("board", t0, err)
				t0 = time.Now()
				record("close", t0, c.Close(ctx, []string{mine.ID}, "done", false))
			}
		}()
	}
	t0 := time.Now()
	close(start)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("the workload did not finish within %v: a deadlock or a stall", maxRuntime)
	}
	elapsed := time.Since(t0)

	for _, e := range unexpect {
		t.Errorf("unexpected error: %v", e)
	}
	// Every contested issue is attempted by several seats (seats x rounds is
	// well past the pool), so each must have exactly one winner and the rest
	// must have been turned away by the guard: a loop that never claimed
	// anything, or claimed everything, fails here.
	for _, id := range pool0 {
		if wins[id] != 1 {
			t.Errorf("contested issue %s was claimed by %d seats; the guard must let exactly one through", id, wins[id])
		}
	}
	attempts := seats * rounds
	if refused != attempts-contested {
		t.Errorf("the guard turned away %d claims, want %d (%d attempts, %d winners)", refused, attempts-contested, attempts, contested)
	}
	held, err := admin.List(ctx, api.ListFilter{Rig: "gt", Statuses: []string{"in_progress"}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Issues) != contested {
		t.Errorf("%d issues are in progress, want the %d contested ones, each held by its winner", len(held.Issues), contested)
	}
	for _, is := range held.Issues {
		if wins[is.ID] != 1 || is.Assignee == "" {
			t.Errorf("in-progress issue %s has %d winners and assignee %q", is.ID, wins[is.ID], is.Assignee)
		}
	}
	st, err := admin.Stats(ctx, "gt")
	if err != nil {
		t.Fatal(err)
	}
	if want := contested + created; st.Total != want {
		t.Errorf("stats total = %d, want %d (the contested pool plus %d created): an issue was lost or doubled", st.Total, want, created)
	}
	if st.ByStatus["closed"] != created {
		t.Errorf("closed = %d, want %d", st.ByStatus["closed"], created)
	}

	byOp := map[string][]time.Duration{}
	for _, s := range samples {
		byOp[s.op] = append(byOp[s.op], s.d)
	}
	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	t.Logf("%d seats x %d rounds, %d requests in %v (%.0f req/s)", seats, rounds, len(samples), elapsed.Round(time.Millisecond), float64(len(samples))/elapsed.Seconds())
	for _, op := range ops {
		ds := byOp[op]
		if p99 := percentile(ds, 0.99); p99 > stallBound {
			t.Errorf("%s: p99 %v is past %v: the service stalled under load", op, p99, stallBound)
		}
		t.Logf("  %-10s n=%-4d p50=%-9v p99=%-9v max=%v", op, len(ds), percentile(ds, 0.50).Round(time.Microsecond), percentile(ds, 0.99).Round(time.Microsecond), percentile(ds, 1.0).Round(time.Microsecond))
	}
}
