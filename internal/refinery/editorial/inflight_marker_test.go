package editorial

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/slot"
)

// inFlightMarkers returns the in-flight marker rows a town's gate report
// carries, read without the `docker ps` cross-check so no test depends on the
// host's Docker VM.
func inFlightMarkers(t *testing.T, townRoot string) []slot.SlotState {
	t.Helper()
	rep, err := slot.StatusPoolLocksOnly(townRoot, slot.Pool{Slots: 1})
	if err != nil {
		t.Fatalf("StatusPoolLocksOnly(%s): %v", townRoot, err)
	}
	var out []slot.SlotState
	for _, st := range rep.Slots {
		if st.Marker {
			out = append(out, st)
		}
	}
	return out
}

// mrArgFromGateArgs returns the MR id the gate script was invoked for.
func mrArgFromGateArgs(args []string) string {
	for i, a := range args {
		if a == "--mr" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestRun_HoldsTheInFlightMarkerWhileTheGateRuns is the report side of
// gt-97cm: while a review is at the backend, the town can see it — with the
// age and the role its readers key on — without the pool counting it as a
// container suite.
func TestRun_HoldsTheInFlightMarkerWhileTheGateRuns(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))

	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			entered <- struct{}{}
			<-proceed
			return "", 0, nil
		},
	}

	done := make(chan ReviewResult, 1)
	go func() { done <- Run(context.Background(), fixture.request(), deps) }()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the gate script never ran")
	}

	townRoot := filepath.Dir(fixture.rigDir)
	rep, err := slot.StatusPoolLocksOnly(townRoot, slot.Pool{Slots: 1})
	if err != nil {
		t.Fatalf("StatusPoolLocksOnly: %v", err)
	}
	rows := inFlightMarkers(t, townRoot)
	if len(rows) != 1 {
		t.Fatalf("want one in-flight marker while the gate runs, got %+v", rep.Slots)
	}
	m := rows[0]
	if m.Name != "om-review-gt-mr-1" {
		t.Errorf("marker name = %q, want it keyed on the MR id", m.Name)
	}
	if m.Owner == nil || m.Owner.Role != "gastown/om-review" || m.Owner.PID != os.Getpid() {
		t.Fatalf("marker row should name the review holding it: %+v", m.Owner)
	}
	if m.Owner.AcquiredAt.IsZero() {
		t.Error("marker row carries no acquisition time to report an age from")
	}
	if rep.Total != 1 || rep.HeldCount != 0 || rep.Held || rep.Busy() {
		t.Fatalf("a review in flight must not read as the pool being held: %+v", rep)
	}

	close(proceed)
	if res := <-done; res.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", res.Exit, res.Stderr, res.Class)
	}
	if rows := inFlightMarkers(t, townRoot); len(rows) != 0 {
		t.Fatalf("the marker outlived the review: %+v", rows)
	}
}

// TestRun_RefusesASecondReviewOfTheSameDiff: a duplicate invocation — the CLI
// racing a batch, or two batches overlapping — must refuse before the gate
// script runs, or the same diff is reviewed (and billed) twice.
func TestRun_RefusesASecondReviewOfTheSameDiff(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))

	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			entered <- struct{}{}
			<-proceed
			return "", 0, nil
		},
	}
	first := make(chan ReviewResult, 1)
	go func() { first <- Run(context.Background(), fixture.request(), deps) }()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first review never reached the gate script")
	}

	deps.Exec = func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
		t.Error("the gate script ran for a review the in-flight marker should have refused")
		return "", 0, nil
	}
	res := Run(context.Background(), fixture.request(), deps)
	if res.Exit != 2 || res.Class != ReviewInFlight {
		t.Fatalf("Exit/Class = %d/%q, want 2/%s", res.Exit, res.Class, ReviewInFlight)
	}
	if !strings.Contains(res.Stderr, "already held") || !strings.Contains(res.Stderr, "gastown/om-review") {
		t.Errorf("refusal should name the marker and its holder, got %q", res.Stderr)
	}

	// The refusal is the duplicate's alone: the review in flight still holds
	// the marker and still finishes.
	if rows := inFlightMarkers(t, filepath.Dir(fixture.rigDir)); len(rows) != 1 {
		t.Fatalf("the refusal disturbed the running review's marker: %+v", rows)
	}
	close(proceed)
	if got := <-first; got.Exit != 0 {
		t.Fatalf("the running review exited %d (stderr=%q class=%q)", got.Exit, got.Stderr, got.Class)
	}
}

// TestRun_ReleasesTheMarkerOnEveryFailurePath: the release is deferred, so a
// review that dies after the marker is taken must leave nothing behind —
// otherwise the next review of the same diff is refused forever by a hold
// nobody owns.
func TestRun_ReleasesTheMarkerOnEveryFailurePath(t *testing.T) {
	fakeBDForReview(t)

	t.Run("binary missing", func(t *testing.T) {
		townRoot := t.TempDir()
		rigDir := filepath.Join(townRoot, "gastown")
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatalf("mkdir rig dir: %v", err)
		}
		req := ReviewRequest{RigDir: rigDir, RepoDir: rigDir, MRID: "gt-mr-1", Rig: "gastown", Target: "main"}
		res := Run(context.Background(), req, Deps{Recorder: plugin.NewRecorder(t.TempDir())})
		if res.Exit != 2 || res.Class != BinaryMissing {
			t.Fatalf("Exit/Class = %d/%q, want 2/%s", res.Exit, res.Class, BinaryMissing)
		}
		if rows := inFlightMarkers(t, townRoot); len(rows) != 0 {
			t.Fatalf("a failed review left its marker held: %+v", rows)
		}
	})

	t.Run("version mismatch", func(t *testing.T) {
		fixture := newReviewFixture(t)
		if err := corruptManifestSHA(t, fixture.rigDir); err != nil {
			t.Fatalf("corrupt manifest: %v", err)
		}
		calls := 0
		res := Run(context.Background(), fixture.request(), Deps{
			Git:      git.NewGit(fixture.repoDir),
			Recorder: plugin.NewRecorder(t.TempDir()),
			Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
				calls++
				return "", 0, nil
			},
		})
		if calls != 0 || res.Class != VersionMismatch {
			t.Fatalf("calls/Class = %d/%q, want 0/%s", calls, res.Class, VersionMismatch)
		}
		if rows := inFlightMarkers(t, filepath.Dir(fixture.rigDir)); len(rows) != 0 {
			t.Fatalf("the version assert left the marker held: %+v", rows)
		}
	})

	t.Run("gate script launch failure", func(t *testing.T) {
		fixture := newReviewFixture(t)
		res := Run(context.Background(), fixture.request(), Deps{
			Git:      git.NewGit(fixture.repoDir),
			Recorder: plugin.NewRecorder(t.TempDir()),
			Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
				return "", -1, errors.New("gate script could not be launched")
			},
		})
		if res.Exit != 2 || res.Class != Tooling {
			t.Fatalf("Exit/Class = %d/%q, want 2/%s", res.Exit, res.Class, Tooling)
		}
		if rows := inFlightMarkers(t, filepath.Dir(fixture.rigDir)); len(rows) != 0 {
			t.Fatalf("a failed gate script left the marker held: %+v", rows)
		}
	})

	t.Run("gate script panics", func(t *testing.T) {
		fixture := newReviewFixture(t)
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("Run should carry the gate's panic out to its caller")
				}
			}()
			Run(context.Background(), fixture.request(), Deps{
				Git:      git.NewGit(fixture.repoDir),
				Recorder: plugin.NewRecorder(t.TempDir()),
				Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
					panic("gate script exploded")
				},
			})
		}()
		if rows := inFlightMarkers(t, filepath.Dir(fixture.rigDir)); len(rows) != 0 {
			t.Fatalf("a panicking review left the marker held: %+v", rows)
		}
	})
}

// TestRun_ParallelBatchMembersEachHoldTheirOwnMarker: the batch reviews up to
// ReviewParallelism members at once, so the marker has to be keyed per review
// rather than per rig — three members in flight are three holders, and the
// report names each one.
func TestRun_ParallelBatchMembersEachHoldTheirOwnMarker(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)

	ids := []string{"gt-mr-1", "gt-mr-2", "gt-mr-3"}
	var notesMu sync.Mutex
	entered := make(chan string, len(ids))
	proceed := make(chan struct{})

	results := make([]ReviewResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		// Each member gets its own beads store: the batch's real members have
		// one MR bead each, and a shared in-process store would be raced by
		// their concurrent SetMRFields writes rather than exercising the
		// markers.
		deps := Deps{
			Git:      git.NewGit(fixture.repoDir),
			Beads:    beads.NewWithStore(fixture.repoDir, newReviewStore(mrIssue(id, fixture.request().Branch, "main", "gt-real", "gastown", "marble"))),
			Recorder: plugin.NewRecorder(t.TempDir()),
			NotesMu:  &notesMu,
			Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
				writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
				entered <- mrArgFromGateArgs(args)
				<-proceed
				return "", 0, nil
			},
		}
		req := fixture.request()
		req.MRID = id
		wg.Add(1)
		go func(i int, req ReviewRequest) {
			defer wg.Done()
			results[i] = Run(context.Background(), req, deps)
		}(i, req)
	}

	// Every member has to be at the gate script at once: if the markers
	// collided, the later members would be refused instead of running.
	for range ids {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("not every batch member reached the gate script together")
		}
	}

	townRoot := filepath.Dir(fixture.rigDir)
	rows := inFlightMarkers(t, townRoot)
	if len(rows) != len(ids) {
		t.Fatalf("want %d in-flight markers, got %+v", len(ids), rows)
	}
	for _, id := range ids {
		var found bool
		for _, r := range rows {
			if r.Name == "om-review-"+id {
				found = true
			}
		}
		if !found {
			t.Errorf("no marker named for %s in %+v", id, rows)
		}
	}

	close(proceed)
	wg.Wait()
	for i, res := range results {
		if res.Exit != 0 {
			t.Fatalf("member %s: Exit = %d, want 0 (stderr=%q class=%q)", ids[i], res.Exit, res.Stderr, res.Class)
		}
	}
	if rows := inFlightMarkers(t, townRoot); len(rows) != 0 {
		t.Fatalf("markers outlived the batch: %+v", rows)
	}
}

// TestReviewMarkerName_KeysOnTheWorkReviewed: the lock name has to identify one
// review, not one rig — the batch's members share a rig.
func TestReviewMarkerName_KeysOnTheWorkReviewed(t *testing.T) {
	tests := []struct {
		name string
		req  ReviewRequest
		want string
	}{
		{
			name: "mr review",
			req:  ReviewRequest{MRID: "gt-mr-7", Branch: "polecat/x/gt-7"},
			want: "om-review-gt-mr-7",
		},
		{
			name: "landed review without an mr id",
			req:  ReviewRequest{Landed: &LandedRange{Commit: "abc123"}},
			want: "om-review-abc123",
		},
		{
			name: "branch review without an mr id",
			req:  ReviewRequest{Branch: "polecat/x/gt-7"},
			want: "om-review-polecat/x/gt-7",
		},
		{
			name: "neither",
			req:  ReviewRequest{},
			want: "om-review-unkeyed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewMarkerName(tt.req); got != tt.want {
				t.Errorf("reviewMarkerName = %q, want %q", got, tt.want)
			}
			if got := reviewMarkerRole("gastown"); got != "gastown/om-review" {
				t.Errorf("reviewMarkerRole = %q, want gastown/om-review", got)
			}
		})
	}
}

// corruptManifestSHA rewrites rigDir's harness manifest with a recorded om
// binary sha the binary cannot match, so AssertVersion fails.
func corruptManifestSHA(t *testing.T, rigDir string) error {
	t.Helper()
	m, err := LoadManifest(rigDir)
	if err != nil {
		return err
	}
	m.OMBinary.SHA256 = strings.Repeat("0", 64)
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644)
}
