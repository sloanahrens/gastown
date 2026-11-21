package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// goListLog is an images job log: with the guard's note when degraded.
func goListLog(degraded bool) string {
	if degraded {
		return "pin-drift guard: rig devops pin 123456789abc: " + daemonimage.GoListNoteMarker + " in /src (go: no module); the static input list and the closure walk were used instead\n"
	}
	return "pin-drift guard: rig devops pin 123456789abc is 3 commits behind main, within the bounds\n"
}

// TestPairGoListStreak: the streak counts the newest images builds whose job log
// holds the guard's go list note, from the newest, and stops at the first that
// does not; a log that cannot be read returns the count so far and read false, so
// the caller neither raises nor clears on it.
func TestPairGoListStreak(t *testing.T) {
	t.Parallel()
	runs := []forgejo.ActionRun{{ID: 1}, {ID: 2}, {ID: 3}}
	tests := []struct {
		name       string
		degraded   []bool // the log of run 1, 2, 3
		noJobs     int64  // a run whose jobs the API does not return
		logErr     error
		wantStreak int
		wantRead   bool
	}{
		{name: "three logs with the note", degraded: []bool{true, true, true}, wantStreak: 3, wantRead: true},
		{name: "the newest two of three", degraded: []bool{true, true, false}, wantStreak: 2, wantRead: true},
		{name: "a clean newest log", degraded: []bool{false, true, true}, wantStreak: 0, wantRead: true},
		{name: "a run with no job counts so far and is unread", degraded: []bool{true, true, true}, noJobs: 2, wantStreak: 1, wantRead: false},
		{name: "a log that cannot be read", degraded: []bool{true, true, true}, logErr: errors.New("boom"), wantStreak: 0, wantRead: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := &fakePairAPI{jobs: map[int64][]forgejo.ActionRunJob{}, logs: map[int64]string{}, logErr: tt.logErr}
			for i, run := range runs {
				if run.ID == tt.noJobs {
					continue
				}
				api.jobs[run.ID] = []forgejo.ActionRunJob{{ID: run.ID * 10}}
				api.logs[run.ID*10] = goListLog(tt.degraded[i])
			}

			streak, read := goListNoteStreak(context.Background(), pairReadyRig{Rig: "gastown", API: api}, runs)

			if streak != tt.wantStreak || read != tt.wantRead {
				t.Errorf("goListNoteStreak = %d, %v; want %d, %v", streak, read, tt.wantStreak, tt.wantRead)
			}
		})
	}
}

// goListRig is a rig whose images runs 1..n are all successful, newest first,
// with the given logs.
func goListRig(t *testing.T, degraded ...bool) (pairReadyRig, *fakePairAPI) {
	t.Helper()
	api := &fakePairAPI{jobs: map[int64][]forgejo.ActionRunJob{}, logs: map[int64]string{}}
	for i, d := range degraded {
		id := int64(i + 1)
		api.runs = append(api.runs, forgejo.ActionRun{ID: id, Status: deployRunSucceeded, CommitSHA: strings.Repeat(string(rune('a'+i)), 40), WorkflowID: pairReadyImagesWorkflow})
		api.jobs[id] = []forgejo.ActionRunJob{{ID: id * 10}}
		api.logs[id*10] = goListLog(d)
	}
	return pairReadyRig{Rig: "gastown", API: api}, api
}

// goListPin is the bd pin the fake pin read names; the key carries its first
// twelve characters.
const goListPin = "0123456789abcdef0123456789abcdef01234567"

func goListPinAt(string) (string, error) { return goListPin, nil }

// TestPairGoListDegraded: three consecutive successful images builds whose log
// holds the go list note raise one item keyed by the rig and the pin; two do not;
// a clean newest log clears a held item; a read that fails neither raises nor
// clears the degraded item and raises the check's unreadable item for the rig.
func TestPairGoListDegraded(t *testing.T) {
	t.Parallel()
	key := "deploy-pair-golist-degraded:gastown:0123456789ab"
	oldKey := "deploy-pair-golist-degraded:gastown:ffffffffffff"
	held := []attention.Item{{Key: key, Kind: attention.KindDeployPairGoListDegraded, Rig: "gastown"}}
	oldHeld := []attention.Item{{Key: oldKey, Kind: attention.KindDeployPairGoListDegraded, Rig: "gastown"}}
	unreadable := pairReadyUnreadableItem("gastown", errors.New("x")).Key
	tests := []struct {
		name      string
		degraded  []bool
		prev      []attention.Item
		logErr    error
		runErr    error
		nilList   bool
		pinErr    error
		emptyLogs []int    // run indexes (0 = newest) whose job log reads empty with no error
		want      []string // the keys, in order
	}{
		{name: "a full streak raises one item", degraded: []bool{true, true, true}, want: []string{key}},
		{name: "a streak of two raises none", degraded: []bool{true, true, false}},
		{name: "a full streak keeps the held item", degraded: []bool{true, true, true}, prev: held, want: []string{key}},
		{name: "a moved pin replaces the held item of the old pin", degraded: []bool{true, true, true}, prev: oldHeld, want: []string{key}},
		{name: "a clean newest log clears the held item", degraded: []bool{false, true, true}, prev: held},
		{name: "an unreadable log leaves the held item and raises the unreadable item", degraded: []bool{true, true, true}, prev: held, logErr: errors.New("boom"), want: []string{key, unreadable}},
		{name: "an unreadable log raises the unreadable item", degraded: []bool{true, true, true}, logErr: errors.New("boom"), want: []string{unreadable}},
		{name: "an unreadable listing raises the unreadable item", degraded: []bool{true, true, true}, runErr: errors.New("boom"), want: []string{unreadable}},
		{name: "an unreadable listing leaves the held item", degraded: []bool{true, true, true}, prev: held, runErr: errors.New("boom"), want: []string{key, unreadable}},
		{name: "a listing with no answer raises the unreadable item", degraded: []bool{true, true, true}, nilList: true, want: []string{unreadable}},
		{name: "an unreadable pin raises the unreadable item and no degraded one", degraded: []bool{true, true, true}, pinErr: errors.New("boom"), want: []string{unreadable}},
		{name: "an empty newest log leaves the held item and raises the unreadable item", degraded: []bool{true, true, true}, prev: held, emptyLogs: []int{0}, want: []string{key, unreadable}},
		{name: "an empty log inside the streak raises no degraded item and the unreadable one", degraded: []bool{true, true, true}, emptyLogs: []int{2}, want: []string{unreadable}},
		{name: "an unreadable pin leaves the held item", degraded: []bool{true, true, true}, prev: held, pinErr: errors.New("boom"), want: []string{key, unreadable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, api := goListRig(t, tt.degraded...)
			api.logErr, api.runErr, api.nilList = tt.logErr, tt.runErr, tt.nilList
			for _, i := range tt.emptyLogs {
				api.logs[int64(i+1)*10] = ""
			}
			pinAt := goListPinAt
			if tt.pinErr != nil {
				pinAt = func(string) (string, error) { return "", tt.pinErr }
			}

			items := goListDegradedItems(context.Background(), r, pinAt, tt.prev)

			var got []string
			for _, it := range items {
				got = append(got, it.Key)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("item keys = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPairGoListDegradedIsCollected: the item the check holds reaches the attention
// queue through its own collector.
func TestPairGoListDegradedIsCollected(t *testing.T) {
	t.Parallel()
	it := attention.Item{Key: "deploy-pair-golist-degraded:gastown", Kind: attention.KindDeployPairGoListDegraded}
	s := &attentionSources{pairReady: func() []attention.Item {
		return []attention.Item{it, {Key: "other", Kind: attention.KindDeployPairReady}}
	}}

	got, err := s.collectPairGoListDegraded(context.Background())

	if err != nil || len(got) != 1 || got[0].Key != it.Key {
		t.Fatalf("collectPairGoListDegraded = %+v, %v; want the one degraded item", got, err)
	}
}

// TestPairGoListDegradedSurvivesAnUnreadRig: a rig whose read fails keeps the
// degraded item it held (before the failure bound), as it keeps its commit items.
func TestPairGoListDegradedSurvivesAnUnreadRig(t *testing.T) {
	t.Parallel()
	it := attention.Item{Key: goListDegradedKey("gastown", goListPin), Kind: attention.KindDeployPairGoListDegraded, Rig: "gastown"}

	got := pairReadyKept([]attention.Item{it}, 1, pairReadyUnreadableItem("gastown", errors.New("boom")), pairReadyRefusal{}, "")

	for _, kept := range got {
		if kept.Key == it.Key {
			return
		}
	}
	t.Fatalf("pairReadyKept dropped the degraded item: %+v", got)
}

// TestPairGoListDegradedIgnoresRunsThatNeverReachedTheGuard: a newest run that is
// still running, or failed before the guard, has no note in its log and must not
// end the streak of the three completed builds behind it, or the item would clear
// while its cause persists.
func TestPairGoListDegradedIgnoresRunsThatNeverReachedTheGuard(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"running", "waiting", deployRunFailed, deployRunCancelled} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			r, api := goListRig(t, true, true, true)
			api.runs = append([]forgejo.ActionRun{{ID: 9, Status: status, CommitSHA: strings.Repeat("f", 40), WorkflowID: pairReadyImagesWorkflow}}, api.runs...)
			api.jobs[9] = []forgejo.ActionRunJob{{ID: 90}}
			api.logs[90] = goListLog(false)

			items := goListDegradedItems(context.Background(), r, goListPinAt, nil)

			if len(items) != 1 {
				t.Fatalf("items = %+v, want the degraded item: a %s newest run must not end the streak", items, status)
			}
			if q := api.runsQueries[len(api.runsQueries)-1]; len(q.Status) != 1 || q.Status[0] != deployRunSucceeded || q.WorkflowID != pairReadyImagesWorkflow {
				t.Errorf("the listing asked for %+v, want the successful images runs", q)
			}
		})
	}
}

// TestPairGoListDegradedCollectorIsRegistered: the daemon's attention sources list
// the degraded kind, so the item the check holds reaches the queue.
func TestPairGoListDegradedCollectorIsRegistered(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	for _, c := range f.src.collectors() {
		if c.kind == attention.KindDeployPairGoListDegraded {
			return
		}
	}
	t.Fatalf("no collector is registered for %s", attention.KindDeployPairGoListDegraded)
}

// TestPairGoListUnreadOnce: the streak's unreadable item is dropped when the rig's
// scan already raised an images-unknown item for the same failing run list, and
// kept when the scan raised none; the degraded item is never dropped.
func TestPairGoListUnreadOnce(t *testing.T) {
	t.Parallel()
	unread := pairReadyUnreadableItem("gastown", errors.New("x"))
	degraded := attention.Item{Key: "k", Kind: attention.KindDeployPairGoListDegraded}
	unknown := attention.Item{Key: "u", Kind: attention.KindImagesUnknown}

	if got := goListUnreadOnce([]attention.Item{unknown}, []attention.Item{degraded, unread}); !slices.Equal(got, []attention.Item{degraded}) {
		t.Errorf("with an unknown item the streak items = %+v, want only the degraded item", got)
	}
	if got := goListUnreadOnce(nil, []attention.Item{unread}); !slices.Equal(got, []attention.Item{unread}) {
		t.Errorf("with no unknown item the streak items = %+v, want the unreadable item kept", got)
	}
}

// TestPairGoListDegradedKeyCarriesThePinThroughThePass: the whole pass reads the
// bd pin the newest build's commit names and keys the item by rig and the first
// twelve characters of it.
func TestPairGoListDegradedKeyCarriesThePinThroughThePass(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.api.runs = nil
	fx.api.jobs, fx.api.logs = map[int64][]forgejo.ActionRunJob{}, map[int64]string{}
	for i := int64(1); i <= goListDegradedBuilds; i++ {
		fx.api.runs = append(fx.api.runs, forgejo.ActionRun{ID: i, Status: deployRunSucceeded, CommitSHA: strings.Repeat("e", 40), WorkflowID: pairReadyImagesWorkflow})
		fx.api.jobs[i] = []forgejo.ActionRunJob{{ID: i * 10}}
		fx.api.logs[i*10] = goListLog(true)
	}

	fx.run(pairT0)

	want := "deploy-pair-golist-degraded:gastown:" + pairBdPin[:12]
	for _, it := range fx.check.observed() {
		if it.Key == want {
			return
		}
	}
	t.Fatalf("no item keyed %s in %+v", want, fx.check.observed())
}
