package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// birdActivityRig wires a birdActivity to a fake store, a fake clock and a
// fake log, and records every read and every log line.
type birdActivityRig struct {
	now   time.Time
	reads []string
	logs  []string
	a     *birdActivity
}

func newBirdActivityRig(show func(id string) (*beads.Issue, error)) *birdActivityRig {
	r := &birdActivityRig{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	r.a = newBirdActivity(func(id string) (*beads.Issue, error) {
		r.reads = append(r.reads, id)
		return show(id)
	}, func() time.Time { return r.now }, func(format string, args ...any) {
		r.logs = append(r.logs, fmt.Sprintf(format, args...))
	})
	return r
}

// A bird's last activity is the update time of gt-<bird>, not the age of its
// daily report.
func TestDashboardBirdActivityAnswersTheHandoffBeadUpdateTime(t *testing.T) {
	t.Parallel()
	r := newBirdActivityRig(func(id string) (*beads.Issue, error) {
		return &beads.Issue{ID: id, UpdatedAt: "2026-11-26T10:00:00Z"}, nil
	})
	last, ok := r.a.Last("dunlin")
	if !ok {
		t.Fatal("Last(dunlin) answered false, want the handoff bead's update time")
	}
	if want := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC); !last.Equal(want) {
		t.Errorf("Last(dunlin) = %v, want %v", last, want)
	}
	if len(r.reads) != 1 || r.reads[0] != "gt-dunlin" {
		t.Errorf("reads = %v, want [gt-dunlin]", r.reads)
	}
}

// An answer is reused for 60 seconds on the clock; after that the bead is read
// again.
func TestDashboardBirdActivityCachesAnswerFor60Seconds(t *testing.T) {
	t.Parallel()
	r := newBirdActivityRig(func(id string) (*beads.Issue, error) {
		return &beads.Issue{ID: id, UpdatedAt: "2026-11-26T10:00:00Z"}, nil
	})
	r.a.Last("dunlin")
	r.now = r.now.Add(30 * time.Second)
	r.a.Last("dunlin")
	if len(r.reads) != 1 {
		t.Fatalf("reads after 30s = %d, want 1 (cached)", len(r.reads))
	}
	r.now = r.now.Add(31 * time.Second)
	r.a.Last("dunlin")
	if len(r.reads) != 2 {
		t.Errorf("reads after 61s = %d, want 2 (cache expired)", len(r.reads))
	}
}

// A missing bead is the store's ErrNotFound, bare or wrapped. It answers false,
// is cached, and is not logged: a bird with no handoff bead is not a failure.
func TestDashboardBirdActivityMissingBeadAnswersFalseWithoutLog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"bare", beads.ErrNotFound},
		{"wrapped", fmt.Errorf("show gt-rictus: %w", beads.ErrNotFound)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newBirdActivityRig(func(string) (*beads.Issue, error) {
				return nil, tc.err
			})
			if last, ok := r.a.Last("rictus"); ok || !last.IsZero() {
				t.Errorf("Last(rictus) = %v, %v; want zero time, false", last, ok)
			}
			r.a.Last("rictus")
			if len(r.reads) != 1 {
				t.Errorf("reads = %d, want 1 (missing answer cached)", len(r.reads))
			}
			if len(r.logs) != 0 {
				t.Errorf("logs = %q, want none for a missing bead", r.logs)
			}
		})
	}
}

// A read that fails for any reason other than ErrNotFound answers false and
// logs once. A store that answers no bead and no error is the same failure.
func TestDashboardBirdActivityFailedReadAnswersFalseAndLogs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue *beads.Issue
		err   error
	}{
		{"error", nil, errors.New("bd show: connection refused")},
		{"nil issue without error", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newBirdActivityRig(func(string) (*beads.Issue, error) {
				return tc.issue, tc.err
			})
			if last, ok := r.a.Last("rictus"); ok || !last.IsZero() {
				t.Errorf("Last(rictus) = %v, %v; want zero time, false", last, ok)
			}
			if len(r.logs) != 1 {
				t.Fatalf("logs = %q, want exactly one line", r.logs)
			}
			if !strings.Contains(r.logs[0], "gt-rictus") {
				t.Errorf("log = %q, want it to name gt-rictus", r.logs[0])
			}
		})
	}
}

// An update time that does not parse is not shown as a time: it answers false
// and logs once.
func TestDashboardBirdActivityUnparsedTimeAnswersFalse(t *testing.T) {
	t.Parallel()
	for _, updated := range []string{"", "yesterday"} {
		r := newBirdActivityRig(func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, UpdatedAt: updated}, nil
		})
		if last, ok := r.a.Last("rictus"); ok || !last.IsZero() {
			t.Errorf("updated_at %q: Last = %v, %v; want zero time, false", updated, last, ok)
		}
		if len(r.logs) != 1 {
			t.Errorf("updated_at %q: logs = %q, want exactly one line", updated, r.logs)
		}
	}
}

// The hub the command builds puts each bird's last activity on its row. dunlin's
// handoff bead was updated two hours ago, so its row carries that time; rictus
// has no handoff bead, so its row carries none (gt-o5bifr). The fake store
// reaches the hub through the seam's tailOptions, the same beads reader the
// birds' activity closure reads at each call.
func TestDashboardBirdsCarryLastActive(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	written := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, bird := range []string{"dunlin", "rictus"} {
		reports := filepath.Join(root, ".runtime", bird, "reports")
		if err := os.MkdirAll(reports, 0o755); err != nil {
			t.Fatal(err)
		}
		sidecar := fmt.Sprintf(`{"bird":%q,"written_at":%q,"title":"t","summary":"s"}`, bird, written)
		if err := os.WriteFile(filepath.Join(reports, "daily.json"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	updated := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	observe := func(opts tailOptions) {
		opts.beads.show = func(_, id string) (*beads.Issue, error) {
			if id == "gt-dunlin" {
				return &beads.Issue{ID: id, UpdatedAt: updated.Format(time.RFC3339)}, nil
			}
			return nil, beads.ErrNotFound
		}
	}

	hub, closeSources, err := newDashboardHub(root, time.Now(), time.UTC, "", nil, 0, 0, "", io.Discard, dashboardHubSeam{observe: observe})
	if err != nil {
		t.Fatalf("building the dashboard's hub: %v", err)
	}
	defer closeSources()

	rec := httptest.NewRecorder()
	hub.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/birds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/birds: status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var rows []dashboard.BirdReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decoding /api/birds: %v (body %q)", err, rec.Body.String())
	}
	byBird := map[string]dashboard.BirdReport{}
	for _, r := range rows {
		byBird[r.Bird] = r
	}
	if got := byBird["dunlin"].LastActive; !got.Equal(updated) {
		t.Errorf("dunlin LastActive = %v, want its handoff bead's update time %v", got, updated)
	}
	if got := byBird["rictus"].LastActive; !got.IsZero() {
		t.Errorf("rictus LastActive = %v, want zero for a bird with no handoff bead", got)
	}
}
