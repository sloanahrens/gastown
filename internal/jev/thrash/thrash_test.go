package thrash

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// stamp renders the attached_at a placement carries, minute minutes after t0.
func stamp(minute int) string {
	return t0.Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
}

// placed makes an in_progress bead whose description carries an attached_at
// stamp, the agent var and the spec body.
func placed(id string, minute int, title string) *beads.Issue {
	return &beads.Issue{
		ID:     id,
		Title:  title,
		Status: "in_progress",
		Description: "attached_molecule: gt-wisp-" + id + "\n" +
			"attached_formula: mol-polecat-work\n" +
			"attached_at: " + stamp(minute) + "\n" +
			"attached_vars: [\"agent=polecat\"]\n" +
			"dispatched_by: daemon/spec-dispatch\n\n" + specBody,
	}
}

const specBody = "## Goal\n\nDo the thing.\n\n## Constraints\n\nNo network.\n\n" +
	"## Out of scope\n\nNothing.\n\n## Gate\n\nNarrow tests.\n\n" +
	"## Size\n\none worker, one landing\n\n## Acceptance\n\n- [ ] it works\n"

// fakeLister lists whatever beads it holds.
type fakeLister struct {
	all []*beads.Issue
}

func (f *fakeLister) Placed(time.Time) ([]*beads.Issue, error) { return f.all, nil }

// fakeAsker answers both questions with fixed probabilities and counts the
// calls it is asked to make. A state holding POISON is refused the way Jev
// refuses a bead's own request, and an asker that is down fails the way an
// unreachable Jev does. A nonzero status refuses every ask with that status.
type fakeAsker struct {
	calls  int
	down   bool
	status int
}

func (f *fakeAsker) Ask(_ context.Context, state any, _ map[string]jev.Question) (*jev.Answers, error) {
	f.calls++
	if f.down {
		return nil, errors.New("dial tcp: connection refused")
	}
	if f.status != 0 {
		return nil, &jev.StatusError{Status: f.status}
	}
	if s, _ := state.(string); strings.Contains(s, "POISON") {
		return nil, &jev.StatusError{Status: http.StatusBadRequest}
	}
	return &jev.Answers{
		Model: "jev-test",
		Noul:  map[string]float64{qWillRespawn: 0.7, qNeedsSplit: 0.2},
	}, nil
}

// newPass is one pass over a fake lister and asker, whose cursor sits one
// minute before every placement.
func newPass(t *testing.T, asker Asker, all ...*beads.Issue) *Pass {
	t.Helper()
	dir := t.TempDir()
	p := &Pass{
		Asker:      asker,
		Lister:     &fakeLister{all: all},
		LogPath:    filepath.Join(dir, "thrash.jsonl"),
		CursorPath: filepath.Join(dir, "thrash.cursor.json"),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return t0 },
	}
	if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: t0.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return p
}

// logLines reads every line the pass has written.
func logLines(t *testing.T, path string) []Line {
	t.Helper()
	var lines []Line
	if err := battery.ReadRecent(path, 100, &lines); err != nil {
		t.Fatal(err)
	}
	return lines
}

// A fixture dispatch logs one line per placement with both answers, and a
// second pass over the same placements logs nothing more.
func TestRunLogsOneLinePerPlacementOnce(t *testing.T) {
	t.Parallel()
	p := newPass(t, &fakeAsker{},
		placed("gt-a", 1, "First"), placed("gt-b", 2, "Second"), placed("gt-c", 3, "Third"))

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 3 || sum.Failed != 0 {
		t.Fatalf("first pass: asked %d, failed %d; want 3 and 0", sum.Asked, sum.Failed)
	}
	lines := logLines(t, p.LogPath)
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want 3", len(lines))
	}
	first := lines[0]
	if first.Bead != "gt-a" || first.Agent != "polecat" || first.Model != "jev-test" ||
		first.QuestionVersion != Version || !first.AttachedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("first line = %+v", first)
	}
	if first.Answers.WillRespawn != 0.7 || first.Answers.NeedsSplit != 0.2 {
		t.Errorf("answers = %+v, want will_respawn 0.7 and needs_split 0.2", first.Answers)
	}

	again, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.Asked != 0 {
		t.Errorf("second pass asked %d beads, want 0", again.Asked)
	}
	if n := len(logLines(t, p.LogPath)); n != 3 {
		t.Errorf("log has %d lines after a second pass, want 3", n)
	}
}

// A bead whose request Jev refuses gets no line and does not hold the pass:
// the next placement is asked, and the cursor ends past both.
func TestRunSkipsABeadJevRefusesAndMovesOn(t *testing.T) {
	t.Parallel()
	p := newPass(t, &fakeAsker{},
		placed("gt-a", 1, "First"), placed("gt-p", 2, "POISON"), placed("gt-c", 3, "Third"))

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 2 || sum.Failed != 1 {
		t.Fatalf("asked %d, failed %d; want 2 and 1", sum.Asked, sum.Failed)
	}
	if got := sum.Failures[0].Bead; got != "gt-p" {
		t.Errorf("failure names %s, want gt-p", got)
	}
	lines := logLines(t, p.LogPath)
	if len(lines) != 2 || lines[1].Bead != "gt-c" {
		t.Errorf("log beads = %v, want gt-a and gt-c", lines)
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != "gt-c" {
		t.Errorf("cursor at %s, want gt-c", cur.ID)
	}
}

// Jev out of reach stops the pass, writes no line and leaves the cursor before
// the placement, so the next pass asks about it.
func TestRunStopsWhenJevIsOutOfReach(t *testing.T) {
	t.Parallel()
	p := newPass(t, &fakeAsker{down: true}, placed("gt-a", 1, "First"))

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("Run returned no error with Jev unreachable")
	}
	if n := len(logLines(t, p.LogPath)); n != 0 {
		t.Errorf("log has %d lines, want 0", n)
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != "" || !cur.Time.Equal(t0.Add(-time.Minute)) {
		t.Errorf("cursor moved to %+v, want it unmoved", cur)
	}
}

// An account refusal (402 or 403) says nothing about the bead: the pass stops
// at the first placement without asking about the second, with the cursor where
// it started, no line and no Failed count, so the placements are asked again
// once the account is back. Any other 4xx is still the bead's own fault,
// counted and passed over.
func TestRunStopsOnAccountRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		wantStops bool
	}{
		{"402 payment required", http.StatusPaymentRequired, true},
		{"403 forbidden", http.StatusForbidden, true},
		{"400 bad request", http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			asker := &fakeAsker{status: tc.status}
			p := newPass(t, asker, placed("gt-a", 1, "First"), placed("gt-b", 2, "Second"))

			sum, err := p.Run(context.Background())
			if n := len(logLines(t, p.LogPath)); n != 0 {
				t.Errorf("log has %d lines, want 0", n)
			}
			cur, _, rerr := battery.ReadCursor(p.CursorPath)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if tc.wantStops {
				var se *jev.StatusError
				if !errors.As(err, &se) || se.Status != tc.status {
					t.Errorf("Run error = %v, want a StatusError with status %d", err, tc.status)
				}
				if err != nil && !strings.Contains(err.Error(), "gt-a") {
					t.Errorf("Run error %q does not name the placement it stopped at, gt-a", err)
				}
				if asker.calls != 1 {
					t.Errorf("asked %d times, want 1: the pass must halt, not skip to gt-b", asker.calls)
				}
				if sum.Failed != 0 {
					t.Errorf("failed %d, want 0", sum.Failed)
				}
				if cur.ID != "" || !cur.Time.Equal(t0.Add(-time.Minute)) {
					t.Errorf("cursor moved to %+v, want it unmoved", cur)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if asker.calls != 2 {
				t.Errorf("asked %d times, want 2", asker.calls)
			}
			if sum.Failed != 2 {
				t.Errorf("failed %d, want 2", sum.Failed)
			}
			if cur.ID != "gt-b" {
				t.Errorf("cursor at %s, want gt-b", cur.ID)
			}
		})
	}
}

// A request timeout (408) is retryable, so it says nothing about the bead: the
// pass stops at the first placement with no line and the cursor where it
// started, and the placement is asked again on the next pass.
func TestRunStopsOnRequestTimeout(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{status: http.StatusRequestTimeout}
	p := newPass(t, asker, placed("gt-a", 1, "First"), placed("gt-b", 2, "Second"))

	sum, err := p.Run(context.Background())
	var se *jev.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusRequestTimeout {
		t.Fatalf("Run error = %v, want a StatusError with status 408", err)
	}
	if asker.calls != 1 {
		t.Errorf("asked %d times, want 1: the pass must halt, not skip to gt-b", asker.calls)
	}
	if sum.Failed != 0 {
		t.Errorf("failed %d, want 0", sum.Failed)
	}
	if n := len(logLines(t, p.LogPath)); n != 0 {
		t.Errorf("log has %d lines, want 0", n)
	}
	cur, _, rerr := battery.ReadCursor(p.CursorPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if cur.ID != "" || !cur.Time.Equal(t0.Add(-time.Minute)) {
		t.Errorf("cursor moved to %+v, want it unmoved", cur)
	}
}

// beadFault blames the bead only for a 4xx its own request caused. Authorization,
// a timeout, a rate limit and an account refusal are not the bead's fault.
func TestBeadFaultTransientStatuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
		{http.StatusUnauthorized, false},
		{http.StatusPaymentRequired, false},
		{http.StatusForbidden, false},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			err := &jev.StatusError{Status: tc.status}
			if got := beadFault(err); got != tc.want {
				t.Errorf("beadFault(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// The daily cap stops the pass before it asks, and the placements wait.
func TestRunStopsAtTheDailyCap(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{}
	p := newPass(t, asker, placed("gt-a", 1, "First"))
	p.Allows = func() (bool, error) { return false, nil }

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Asked != 0 {
		t.Errorf("capReached %v, asked %d; want true and 0", sum.CapReached, sum.Asked)
	}
	if asker.calls != 0 {
		t.Errorf("Jev was called %d times past the cap", asker.calls)
	}
}

// The state quotes the spec sections and the agent, and the bead's stamp and
// attachment fields stay out of it.
func TestBuildStateQuotesTheSpecOnly(t *testing.T) {
	t.Parallel()
	state := buildState(placed("gt-a", 1, "First"))
	for _, want := range []string{`title: "First"`, `goal: "Do the thing."`, `agent: "polecat"`} {
		if !strings.Contains(state, want) {
			t.Errorf("state lacks %s:\n%s", want, state)
		}
	}
	if strings.Contains(state, "attached_at") || strings.Contains(state, "dispatched_by") {
		t.Errorf("state carries attachment fields:\n%s", state)
	}
}

// The open wiring refuses a missing price and opens the log beside the town's
// runtime directory.
func TestOpenRequiresAPriceAndNamesTheLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, RigName, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	newClient := func(opts ...jev.Option) (*jev.Client, error) {
		return jev.NewClient(append(opts, jev.WithKey("test-key"))...)
	}
	listerOf := func(string) beadLister { return nil }

	if _, err := open(town, Settings{}, nil, newClient, listerOf); err == nil {
		t.Error("open accepted a missing price")
	}
	p, err := open(town, Settings{USDPerBillionTokens: 4}, nil, newClient, listerOf)
	if err != nil {
		t.Fatal(err)
	}
	if p.LogPath != LogPath(town) || p.CursorPath != CursorPath(town) {
		t.Errorf("paths = %s, %s; want %s, %s", p.LogPath, p.CursorPath, LogPath(town), CursorPath(town))
	}
}
