package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
)

// purgeStub is the store the purge route and the reaper loop run against: an
// in-memory list of issues answering the reads the policy makes. It records the
// closed-before cutoffs it was asked for and the ids it deletes. Any method not
// listed panics on the nil embedded Store.
type purgeStub struct {
	store.Store
	mu           sync.Mutex
	issues       []api.Issue
	deleted      []string
	closedBefore []time.Time
	deleteErr    error
	listErr      error
	// beforeList, when set, runs first in every list, as a test's hook into
	// the pass that is reading.
	beforeList func()
}

func (p *purgeStub) setListErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listErr = err
}

// ListIssues answers like the Postgres store: a list whose context has ended
// fails with that context's error.
func (p *purgeStub) ListIssues(ctx context.Context, f api.ListFilter) (api.IssuePage, error) {
	if p.beforeList != nil {
		p.beforeList()
	}
	if err := ctx.Err(); err != nil {
		return api.IssuePage{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return api.IssuePage{}, p.listErr
	}
	if !f.ClosedBefore.IsZero() {
		p.closedBefore = append(p.closedBefore, f.ClosedBefore)
	}
	var out []api.Issue
	for _, is := range p.issues {
		if is.Ephemeral != f.Ephemeral {
			continue
		}
		if f.IssueType != "" && is.Type != f.IssueType {
			continue
		}
		if !statusMatches(is.Status, f.Statuses) {
			continue
		}
		if !f.ClosedBefore.IsZero() && (is.ClosedAt == nil || !is.ClosedAt.Before(f.ClosedBefore)) {
			continue
		}
		out = append(out, is)
	}
	return api.IssuePage{Issues: out}, nil
}

// statusMatches is the default status set (anything but closed) when statuses
// is empty, and membership otherwise.
func statusMatches(status string, statuses []string) bool {
	switch {
	case len(statuses) == 0:
		return status != "closed"
	case slices.Contains(statuses, "all"):
		return true
	}
	return slices.Contains(statuses, status)
}

func (p *purgeStub) GetIssues(_ context.Context, ids []string) (map[string]api.Issue, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]api.Issue{}
	for _, is := range p.issues {
		if slices.Contains(ids, is.ID) {
			out[is.ID] = is
		}
	}
	return out, nil
}

func (p *purgeStub) DeleteIssues(_ context.Context, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleteErr != nil {
		return p.deleteErr
	}
	p.deleted = append(p.deleted, ids...)
	var kept []api.Issue
	for _, is := range p.issues {
		if !slices.Contains(ids, is.ID) {
			kept = append(kept, is)
		}
	}
	p.issues = kept
	return nil
}

func (p *purgeStub) deletedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.deleted)
}

// closedWisp is an ephemeral wisp closed at closed.
func closedWisp(id string, closed time.Time) api.Issue {
	return api.Issue{ID: id, Type: "task", Status: "closed", Ephemeral: true, CreatedAt: closed.Add(-time.Hour), ClosedAt: &closed}
}

func TestPurgeWispsRouteNeedsAnAdminTokenAndAnswersWhatItKept(t *testing.T) {
	t.Parallel()
	now := time.Now()
	st := &purgeStub{issues: []api.Issue{
		closedWisp("w-old", now.Add(-200*time.Hour)),
		closedWisp("w-young", now.Add(-time.Hour)),
	}}
	ts := adminServer(t, st)
	url := ts.URL + "/v1/wisps:purge"

	code, body := adminCallAs(t, http.MethodPost, url, `{"ids":["w-old","w-young"]}`, "admin-token")
	if code != http.StatusOK {
		t.Fatalf("admin purge = %d %s, want 200", code, body)
	}
	var got api.PurgeWispsResult
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if !slices.Equal(got.Purged, []string{"w-old"}) {
		t.Errorf("purged = %v, want [w-old]", got.Purged)
	}
	if want := []api.KeptWisp{{ID: "w-young", Reason: "closed within purge-age"}}; fmt.Sprint(got.Kept) != fmt.Sprint(want) {
		t.Errorf("kept = %v, want %v", got.Kept, want)
	}
	if d := st.deletedIDs(); !slices.Equal(d, []string{"w-old"}) {
		t.Errorf("store deleted %v, want [w-old]", d)
	}

	if code, _ := adminCallAs(t, http.MethodPost, url, `{"ids":["w-old"]}`, "good-token"); code != http.StatusForbidden {
		t.Errorf("a seat token purging = %d, want 403", code)
	}
	if code, _ := adminCallAs(t, http.MethodPost, url, `{"ids":["w-old"]}`, ""); code != http.StatusUnauthorized {
		t.Errorf("no token purging = %d, want 401", code)
	}
	if code, _ := adminCallAs(t, http.MethodPost, url, `{"ids":[]}`, "admin-token"); code != http.StatusBadRequest {
		t.Errorf("an empty batch = %d, want 400", code)
	}
	if code, _ := adminCallAs(t, http.MethodPost, url, `{"ids":[""]}`, "admin-token"); code != http.StatusBadRequest {
		t.Errorf("an empty id = %d, want 400", code)
	}
}

func TestPurgeWispsRouteSaysNothingWasPurgedWhenTheDeleteFails(t *testing.T) {
	t.Parallel()
	st := &purgeStub{
		issues:    []api.Issue{closedWisp("w-old", time.Now().Add(-200*time.Hour))},
		deleteErr: errors.New("connection reset"),
	}
	ts := adminServer(t, st)
	code, body := adminCallAs(t, http.MethodPost, ts.URL+"/v1/wisps:purge", `{"ids":["w-old"]}`, "admin-token")
	if code == http.StatusOK {
		t.Fatalf("a failed delete answered 200 with %s; it must not report a purge", body)
	}
	if d := st.deletedIDs(); len(d) != 0 {
		t.Errorf("deleted %v after a failed delete", d)
	}
}

func TestClientPurgeWispsAgreesWithTheRoute(t *testing.T) {
	t.Parallel()
	st := &purgeStub{issues: []api.Issue{closedWisp("w-old", time.Now().Add(-200*time.Hour))}}
	ts := adminServer(t, st)
	// The server's own client, as in TestClientAdminAgreesWithTheRoutes.
	hc := ts.Client()
	hc.Timeout = adminHTTP.Timeout
	seat, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(client.Config{BaseURL: ts.URL, Token: "admin-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := seat.PurgeWisps(ctx, []string{"w-old"}); !errors.Is(err, api.ErrForbidden) {
		t.Errorf("a seat client purging = %v, want api.ErrForbidden", err)
	}
	got, err := admin.PurgeWisps(ctx, []string{"w-old"})
	if err != nil {
		t.Fatalf("admin PurgeWisps: %v", err)
	}
	if !slices.Equal(got.Purged, []string{"w-old"}) || len(got.Kept) != 0 {
		t.Errorf("PurgeWisps = %+v, want w-old purged and nothing kept", got)
	}
}

// testWait bounds every wait a loop test makes, so a loop that never answers
// fails the test instead of hanging it.
const testWait = 5 * time.Second

// passLog hands each log line to the test. Its buffer holds the lines of one
// test, so the loop never blocks on the log.
type passLog chan string

func (l passLog) Printf(format string, args ...any) { l <- fmt.Sprintf(format, args...) }

// nextLine waits for the next log line.
func nextLine(t *testing.T, lines passLog) string {
	t.Helper()
	select {
	case got := <-lines:
		return got
	case <-time.After(testWait):
		t.Fatal("no log line within the test wait")
		return ""
	}
}

// passCounts reads the counts a pass logs.
func passCounts(t *testing.T, line string) (closed, purged int) {
	t.Helper()
	if _, err := fmt.Sscanf(line, "reaper pass: closed=%d purged=%d", &closed, &purged); err != nil {
		t.Fatalf("pass logged %q, want closed and purged counts: %v", line, err)
	}
	return closed, purged
}

// sendTick hands the loop one tick, or fails if the loop takes none in time.
func sendTick(t *testing.T, ticks chan<- time.Time, at time.Time) {
	t.Helper()
	select {
	case ticks <- at:
	case <-time.After(testWait):
		t.Fatal("the loop took no tick within the test wait")
	}
}

// waitStopped waits for the loop to return.
func waitStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("the loop did not stop within the test wait")
	}
}

// startLoop runs the reaper loop of a server over st at now, with its own ticks.
func startLoop(t *testing.T, st *purgeStub, now time.Time, lines passLog) (ctx context.Context, cancel context.CancelFunc, ticks chan time.Time, done chan struct{}) {
	t.Helper()
	s := New(Config{
		Auth: fakeAuth{}, Health: fakeHealth{}, SchemaLevel: 3,
		Store: st, Log: lines,
		Reap: store.ReapPolicy{PurgeAge: 48 * time.Hour},
		Now:  func() time.Time { return now },
	})
	ctx, cancel = context.WithCancel(context.Background())
	ticks = make(chan time.Time)
	done = make(chan struct{})
	go func() {
		defer close(done)
		s.reapLoop(ctx, ticks)
	}()
	return ctx, cancel, ticks, done
}

// TestReapLoopRunsOnePassPerTickOnTheInjectedClock drives the loop with its own
// ticks and clock: each tick is one pass that logs its counts, each pass
// measures the purge cutoff from the injected now, and nothing runs once the
// context ends.
func TestReapLoopRunsOnePassPerTickOnTheInjectedClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// One wisp closed 200h ago, past the 48h purge age: the first pass purges
	// it and the second finds nothing left to purge.
	st := &purgeStub{issues: []api.Issue{closedWisp("w-old", now.Add(-200*time.Hour))}}
	lines := make(passLog, 16)
	_, cancel, ticks, done := startLoop(t, st, now, lines)
	defer cancel()

	wantPurged := []int{1, 0}
	for i, want := range wantPurged {
		sendTick(t, ticks, now)
		if _, purged := passCounts(t, nextLine(t, lines)); purged != want {
			t.Fatalf("pass %d purged %d, want %d", i+1, purged, want)
		}
	}
	cancel()
	waitStopped(t, done)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.closedBefore) != len(wantPurged) {
		t.Fatalf("%d purge reads for %d ticks, want one pass per tick", len(st.closedBefore), len(wantPurged))
	}
	for i, cut := range st.closedBefore {
		if want := now.Add(-48 * time.Hour); !cut.Equal(want) {
			t.Errorf("pass %d purge cutoff = %v, want %v from the injected clock", i+1, cut, want)
		}
	}
}

// A pass that fails is logged as failed, and the loop goes on: the next tick
// runs a real pass.
func TestReapLoopLogsAFailedPassAndRetriesOnTheNextTick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &purgeStub{
		issues:  []api.Issue{closedWisp("w-old", now.Add(-200*time.Hour))},
		listErr: errors.New("connection reset"),
	}
	lines := make(passLog, 16)
	_, cancel, ticks, done := startLoop(t, st, now, lines)
	defer cancel()

	sendTick(t, ticks, now)
	if got := nextLine(t, lines); !strings.HasPrefix(got, "reaper pass: failed: ") {
		t.Fatalf("a failed pass logged %q, want a failed line", got)
	}

	st.setListErr(nil)
	sendTick(t, ticks, now)
	if _, purged := passCounts(t, nextLine(t, lines)); purged != 1 {
		t.Fatalf("the retry purged %d, want the one expired wisp", purged)
	}
	cancel()
	waitStopped(t, done)
}

// A pass that shutdown cuts short is not a failed pass: the loop logs nothing
// for it. The hook ends the context while the pass is reading.
func TestReapLoopLogsNothingForAPassCutShortByShutdown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &purgeStub{}
	lines := make(passLog, 16)
	ctx, cancel, ticks, done := startLoop(t, st, now, lines)
	defer cancel()
	st.beforeList = cancel

	sendTick(t, ticks, now)
	waitStopped(t, done)
	select {
	case got := <-lines:
		t.Fatalf("shutdown logged %q for the pass it cut short, want nothing", got)
	default:
	}
	if ctx.Err() == nil {
		t.Fatal("the context did not end")
	}
}

func TestStartReaperWithNoIntervalStartsNothing(t *testing.T) {
	t.Parallel()
	s := New(Config{Auth: fakeAuth{}, Health: fakeHealth{}, SchemaLevel: 3, Store: &purgeStub{}, Log: passLog(make(chan string))})
	select {
	case <-s.StartReaper(context.Background(), 0):
	default:
		t.Fatal("StartReaper with a zero interval did not return a closed channel")
	}
}
