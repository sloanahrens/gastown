package notes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// t0 is the clock every test starts from.
var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// The three fixture notes: one names a path that is gone, one names only things
// that exist, and one names nothing the label can check.
const (
	staleNote = "---\nname: old-handbook-path\n---\n\nThe stale pass lives in `internal/jev/stale/old.go`; run `gt done`.\n"

	currentNote = "---\nname: stale-pass\n---\n\nThe stale pass lives in `internal/jev/stale/stale.go`; run `gt done --status=X`.\n"

	prose = "---\nname: prefer-small-commits\n---\n\nSloan prefers small commits, with `nothing` checkable in them.\n"
)

// fakeAsker answers every file with the same answers and records each state.
type fakeAsker struct {
	mu     sync.Mutex
	states []state
	err    error
	// choice is the kind it picks.
	choice string
}

func (a *fakeAsker) Ask(_ context.Context, st any, _ map[string]jev.Question) (*jev.Answers, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.states = append(a.states, st.(state))
	if a.err != nil {
		return nil, a.err
	}
	choice := a.choice
	if choice == "" {
		choice = KindNone
	}
	return &jev.Answers{
		Model: "jev-test",
		Noul:  map[string]float64{QStillDescribes: 0.8, QNamesRemoved: 0.2},
		Choice: map[string]jev.ChoiceAnswer{QKind: {
			Choice:        choice,
			Probabilities: map[string]float64{choice: 0.9},
			Confidence:    0.9,
		}},
	}, nil
}

// rig is a pass over a temp directory of notes, with fakes for everything else.
type rig struct {
	dir     string
	logPath string
	asker   *fakeAsker
	repo    *fakeRepo
	allowed bool
	now     time.Time
	pass    *Pass
}

func newRig(t *testing.T, notes map[string]string) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		dir:     filepath.Join(dir, "memory"),
		logPath: filepath.Join(dir, "notes.jsonl"),
		asker:   &fakeAsker{},
		repo:    testRepo(),
		allowed: true,
		now:     t0,
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range notes {
		r.write(t, name, body)
	}
	r.pass = &Pass{
		Asker:   r.asker,
		Repo:    r.repo,
		Help:    testHelp(),
		LogPath: r.logPath,
		Allows:  func() (bool, error) { return r.allowed, nil },
		Now:     func() time.Time { return r.now },
		Files: func() ([]File, error) {
			entries, err := os.ReadDir(r.dir)
			if err != nil {
				return nil, err
			}
			var files []File
			for _, e := range entries {
				files = append(files, File{Source: SourceMemory, Name: e.Name(), Path: filepath.Join(r.dir, e.Name())})
			}
			return files, nil
		},
	}
	return r
}

func (r *rig) write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) lines(t *testing.T) []Line {
	t.Helper()
	return readLines(t, r.logPath)
}

func readLines(t *testing.T, path string) []Line {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []Line
	for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if raw == "" {
			continue
		}
		var l Line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

func fixtureNotes() map[string]string {
	return map[string]string{"stale.md": staleNote, "current.md": currentNote, "prose.md": prose}
}

// The first pass logs one line per file, each with the mechanical label its
// tokens earn: stale for the missing path, current when every token resolves,
// none when the note names nothing checkable.
func TestPassLogsOneLinePerFileWithItsLabel(t *testing.T) {
	t.Parallel()
	r := newRig(t, fixtureNotes())

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 3 || sum.Unanswered != 0 || sum.Deferred != 0 {
		t.Errorf("summary = %+v, want 3 asked", sum)
	}
	if r.repo.synced != 1 {
		t.Errorf("clone synced %d times, want once", r.repo.synced)
	}

	byFile := map[string]Line{}
	for _, l := range r.lines(t) {
		byFile[l.File] = l
	}
	if len(byFile) != 3 {
		t.Fatalf("log has %d files, want 3: %+v", len(byFile), byFile)
	}
	if got := byFile["stale.md"]; got.Label != LabelStale || len(got.Missing) != 1 || got.Missing[0] != (Token{KindPath, "internal/jev/stale/old.go"}) {
		t.Errorf("stale.md = %+v, want stale for the missing path", got)
	}
	if got := byFile["current.md"]; got.Label != LabelCurrent || len(got.Missing) != 0 || got.Checked != 3 {
		t.Errorf("current.md = %+v, want current with 3 tokens checked", got)
	}
	if got := byFile["prose.md"]; got.Label != "" || got.Checked != 0 {
		t.Errorf("prose.md = %+v, want unlabeled", got)
	}
	for name, l := range byFile {
		if l.Source != SourceMemory || l.Model != "jev-test" || l.QuestionVersion != Version || l.Hash == "" {
			t.Errorf("%s line = %+v, want memory source, model, version and hash", name, l)
		}
		if l.Answers[QKind].Choice != KindNone || l.Answers[QStillDescribes].Noul == nil || *l.Answers[QStillDescribes].Noul != 0.8 {
			t.Errorf("%s answers = %+v, want the three questions answered", name, l.Answers)
		}
	}
}

// A file whose content has not changed is not asked on the next pass; one whose
// content changed is; and once 30 days pass, every file is.
func TestPassSkipsAnUnchangedFileUntilItChangesOrThirtyDaysPass(t *testing.T) {
	t.Parallel()
	r := newRig(t, fixtureNotes())
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.asker.states) != 3 {
		t.Fatalf("first pass asked %d times, want 3", len(r.asker.states))
	}

	r.now = t0.Add(7 * 24 * time.Hour)
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || len(r.asker.states) != 3 || len(r.lines(t)) != 3 {
		t.Fatalf("second pass: summary %+v, %d asks, %d lines; want nothing asked", sum, len(r.asker.states), len(r.lines(t)))
	}

	r.write(t, "prose.md", prose+"\nA new sentence.\n")
	if sum, err = r.pass.Run(context.Background()); err != nil || sum.Asked != 1 {
		t.Fatalf("third pass: summary %+v, err %v; want only the changed file asked", sum, err)
	}
	if got := r.asker.states[len(r.asker.states)-1]; got.File != "prose.md" || !strings.Contains(got.Text, "A new sentence.") {
		t.Errorf("third pass asked %+v, want prose.md with its new text", got)
	}

	// The two files asked on day 0 are 31 days old; prose.md was asked on day 7.
	r.now = t0.Add(31 * 24 * time.Hour)
	if sum, err = r.pass.Run(context.Background()); err != nil || sum.Asked != 2 {
		t.Fatalf("pass after 31 days: summary %+v, err %v; want the two files asked 31 days ago asked again", sum, err)
	}
}

// A call that got no reply is logged without answers and does not count, so
// the next pass asks the file again. A reply the pass cannot read does count.
func TestPassAsksAgainAfterAFailedCallButNotAfterABadReply(t *testing.T) {
	t.Parallel()
	r := newRig(t, map[string]string{"current.md": currentNote})
	r.asker.err = errors.New("jev down")
	sum, err := r.pass.Run(context.Background())
	if err != nil || sum.Unanswered != 1 {
		t.Fatalf("summary %+v, err %v; want one unanswered", sum, err)
	}
	if ls := r.lines(t); len(ls) != 1 || ls[0].Answers != nil || ls[0].Reason != ReasonAskFailed || ls[0].Label != LabelCurrent {
		t.Fatalf("log = %+v, want one line with the label, null answers and ask_failed", ls)
	}

	r.asker.err = nil
	if sum, err = r.pass.Run(context.Background()); err != nil || sum.Asked != 1 {
		t.Fatalf("next pass: summary %+v, err %v; want the file asked again", sum, err)
	}

	r.write(t, "current.md", currentNote+"\nMore.\n")
	r.asker.err = jev.ErrBadResponse
	if sum, err = r.pass.Run(context.Background()); err != nil || sum.Unanswered != 1 {
		t.Fatalf("bad-reply pass: summary %+v, err %v", sum, err)
	}
	r.asker.err = nil
	if sum, err = r.pass.Run(context.Background()); err != nil || sum.Asked != 0 {
		t.Fatalf("pass after a bad reply: summary %+v, err %v; want the file left alone", sum, err)
	}
}

// The day's cap stops the pass before an Ask; the files it did not reach wait.
func TestPassStopsAtTheDailyCap(t *testing.T) {
	t.Parallel()
	r := newRig(t, fixtureNotes())
	r.allowed = false
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Deferred != 3 || sum.Asked != 0 || len(r.asker.states) != 0 {
		t.Errorf("summary = %+v with %d asks, want the cap reached with 3 deferred", sum, len(r.asker.states))
	}
	if len(r.lines(t)) != 0 {
		t.Errorf("log has %d lines, want none", len(r.lines(t)))
	}
}

// An empty or unreadable file is skipped and counted, and the pass still asks
// the rest.
func TestPassSkipsEmptyFiles(t *testing.T) {
	t.Parallel()
	r := newRig(t, map[string]string{"empty.md": " \n", "current.md": currentNote})
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Unread != 1 {
		t.Errorf("summary = %+v, want 1 asked and 1 unread", sum)
	}
}

// flakyTree fails its first tree read and serves the fixture's after it, the
// way a transient clone failure hits the first file of a pass and no other.
type flakyTree struct {
	*fakeRepo
	failures int
}

func (f *flakyTree) Tree(ctx context.Context) ([]string, error) {
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("git ls-tree: bad object")
	}
	return f.fakeRepo.Tree(ctx)
}

// logFiles names the files the log has lines for, oldest first.
func logFiles(lines []Line) []string {
	var names []string
	for _, l := range lines {
		names = append(names, l.File)
	}
	return names
}

// A label read that fails on one file does not abandon the batch behind it: the
// pass asks the later files, and the failed file earns no line, so the next
// pass asks it again. Before the fix Run returned at the first failure and
// every file behind it waited a week for a pass that counted as done.
func TestRunKeepsGoingAfterAnUnreadableMemoryFile(t *testing.T) {
	t.Parallel()
	r := newRig(t, fixtureNotes())
	r.pass.Repo = &flakyTree{fakeRepo: testRepo(), failures: 1}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v, want the pass to go on after an unreadable file", err)
	}
	if sum.Failed != 1 || sum.Asked != 2 || sum.Deferred != 0 {
		t.Errorf("summary = %+v, want 1 failed and 2 asked", sum)
	}
	if want := []string{"prose.md", "stale.md"}; !reflect.DeepEqual(logFiles(r.lines(t)), want) {
		t.Errorf("log = %v, want %v: the files behind the failure were abandoned", logFiles(r.lines(t)), want)
	}
	if got := len(r.asker.states); got != 2 {
		t.Errorf("asked %d files, want the 2 behind the failure", got)
	}

	// The failed file has no line, so the next pass asks it.
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"prose.md", "stale.md", "current.md"}; !reflect.DeepEqual(logFiles(r.lines(t)), want) {
		t.Errorf("log after the second pass = %v, want %v", logFiles(r.lines(t)), want)
	}
}

// fetchLockedRepo is the fake repo with its fetch routed through the shared
// fetch-lock wrapper the way wire.go routes the clone's.
type fetchLockedRepo struct {
	Repo
	fetch submission.FetchLocked
}

func (r fetchLockedRepo) Sync(ctx context.Context) error { return r.fetch.Sync(ctx) }

func lockedFake(repo *fakeRepo, town string) Repo {
	return fetchLockedRepo{Repo: repo, fetch: submission.FetchLocked{Cloner: repo, TownRoot: town}}
}

// A fetch into the submission battery's shared clone waits for a fetch in
// flight instead of racing it: two fetches into one clone fail each other, and
// that failure lost the whole week's pass. Before the fix the fetch ran under
// no lock at all.
func TestNotesFetchTakesTheCloneLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	r := newRig(t, map[string]string{"current.md": currentNote})
	r.pass.Repo = lockedFake(r.repo, town)

	hold, err := battery.Lock(submission.FetchLockPath(town))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := r.pass.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run with the clone locked = %v, want the wait to end with the ctx", err)
	}
	if r.repo.synced != 0 {
		t.Errorf("the pass fetched while another fetch held the clone")
	}

	// Once the other fetch lets go, the same pass fetches and finishes.
	hold()
	sum, err := r.pass.Run(context.Background())
	if err != nil || sum.Asked != 1 || r.repo.synced != 1 {
		t.Errorf("Run after the holder finished = %+v, %v with %d fetch(es); want the fetch to go ahead",
			sum, err, r.repo.synced)
	}
}

// A fetch does not wait out a submission pass. The pass holds its run lock for
// its whole run, which can be ten minutes of asks, and the fetch needs only the
// clone's fetch lock. Before the fix the fetch took the pass's run lock and
// stalled behind it, and the fetch then held that lock against the pass.
func TestNotesFetchDoesNotWaitOnASubmissionPass(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	r := newRig(t, map[string]string{"current.md": currentNote})
	r.pass.Repo = lockedFake(r.repo, town)

	pass, err := battery.Lock(submission.CursorPath(town) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer pass()

	sum, err := r.pass.Run(context.Background())
	if err != nil || sum.Asked != 1 || r.repo.synced != 1 {
		t.Errorf("Run during a submission pass = %+v, %v with %d fetch(es); want the fetch to go ahead",
			sum, err, r.repo.synced)
	}
}

// With nothing due, the pass does not fetch the clone.
func TestPassFetchesNothingWhenNothingIsDue(t *testing.T) {
	t.Parallel()
	r := newRig(t, map[string]string{"current.md": currentNote})
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.repo.synced != 1 {
		t.Errorf("clone synced %d times over two passes, want once", r.repo.synced)
	}
}

// The log holds the file's name and a hash, never its text.
func TestLogNeverHoldsTheNoteText(t *testing.T) {
	t.Parallel()
	r := newRig(t, map[string]string{"current.md": currentNote})
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stale pass lives") {
		t.Errorf("log holds the note's text: %s", raw)
	}
	if !strings.Contains(string(raw), HashContent(currentNote)) {
		t.Errorf("log does not hold the note's hash: %s", raw)
	}
}

// A held run lock is ErrBusy, and a canceled context leaves the file for the
// next pass without a line.
func TestPassBusyAndCanceled(t *testing.T) {
	t.Parallel()
	r := newRig(t, map[string]string{"current.md": currentNote})
	release, err := battery.Lock(r.logPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("Run with the lock held = %v, want ErrBusy", err)
	}
	release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.pass.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run with a canceled context = %v, want context.Canceled", err)
	}
	if len(r.lines(t)) != 0 {
		t.Errorf("log has lines after a canceled pass: %+v", r.lines(t))
	}
}

// The state quotes the text, marks a cut, and names the file and the dates.
func TestStateQuotesTheTextAndMarksATruncation(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", MaxTextRunes+10)
	r := newRig(t, map[string]string{"long.md": long})
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.asker.states[0]
	if !got.Truncated || got.File != "long.md" || got.Source != "memory note" || got.Today != "2026-10-21" || got.Modified == "" {
		t.Errorf("state = %+v, want a truncated memory note with its dates", got)
	}
	if n := len([]rune(got.Text)); n != MaxTextRunes+1 {
		t.Errorf("text is %d runes, want %d and an ellipsis", n, MaxTextRunes)
	}
}

// The kind Choice has the seven options, and the question ids and options are
// the ones the version stamps.
func TestQuestions(t *testing.T) {
	t.Parallel()
	qs := Questions()
	if len(qs) != 3 {
		t.Fatalf("%d questions, want 3", len(qs))
	}
	for _, id := range []string{QStillDescribes, QNamesRemoved} {
		if qs[id].Type != "noul" || qs[id].True == "" || qs[id].False == "" {
			t.Errorf("%s = %+v, want a described noul", id, qs[id])
		}
	}
	kind := qs[QKind]
	if kind.Type != "choice" || len(kind.Options) != 7 {
		t.Fatalf("kind = %+v, want a choice of 7 options", kind)
	}
	for _, opt := range []string{KindPath, KindCommand, KindFlag, KindConfigKey, KindProcess, KindNone, KindUnclear} {
		if kind.Options[opt] == "" {
			t.Errorf("kind has no option %q", opt)
		}
	}
	for id, q := range qs {
		if !strings.Contains(q.Instructions, "not instructions") {
			t.Errorf("%s does not tell Jev the state is data", id)
		}
	}
}

// replyTo is a transport that records each request body and answers with
// reply, so the test drives the real Jev client without listening on a port.
type replyTo struct {
	reply    string
	requests [][]byte
}

func (rt *replyTo) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.requests = append(rt.requests, body)
	rec := httptest.NewRecorder()
	_, _ = io.WriteString(rec, rt.reply)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// jevNotesReply answers the three questions for one file. The kind is path.
func jevNotesReply(probabilities string) string {
	return `{
  "model": "jev-1.13.0",
  "answers": {
    "still_describes_the_town": {"type": "noul", "noul": 0.1},
    "names_something_removed": {"type": "noul", "noul": 0.9},
    "kind": {"type": "choice", "choice": "path", "confidence": 0.8, "probabilities": ` + probabilities + `}
  },
  "usage": {"input_tokens": 100, "output_tokens": 20}
}`
}

// The kind answer is the option with the highest probability, read by name,
// whichever order Jev sends the probabilities in and whichever order the
// options were listed.
func TestKindChoiceIgnoresOptionOrder(t *testing.T) {
	t.Parallel()
	orders := []struct {
		name, probabilities string
	}{
		{"as listed", `{"path": 0.7, "command": 0.05, "flag": 0.05, "config_key": 0.05, "process": 0.05, "none": 0.05, "unclear": 0.05}`},
		{"sorted keys", `{"command": 0.05, "config_key": 0.05, "flag": 0.05, "none": 0.05, "path": 0.7, "process": 0.05, "unclear": 0.05}`},
		{"reversed keys", `{"unclear": 0.05, "process": 0.05, "path": 0.7, "none": 0.05, "flag": 0.05, "config_key": 0.05, "command": 0.05}`},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			rt := &replyTo{reply: jevNotesReply(order.probabilities)}
			client, err := jev.NewClient(jev.WithBaseURL("http://jev.invalid/v1/systemone"), jev.WithKey("test-key"),
				jev.WithHTTPClient(&http.Client{Transport: rt}))
			if err != nil {
				t.Fatal(err)
			}
			r := newRig(t, map[string]string{"stale.md": staleNote})
			r.pass.Asker = client

			if sum, err := r.pass.Run(context.Background()); err != nil || sum.Asked != 1 {
				t.Fatalf("summary %+v, err %v", sum, err)
			}
			lines := r.lines(t)
			if len(lines) != 1 {
				t.Fatalf("log = %+v, want one line", lines)
			}
			ans := lines[0].Answers[QKind]
			if ans.Choice != "path" || ans.Probabilities["path"] != 0.7 {
				t.Errorf("kind = %+v, want path at 0.7", ans)
			}
			if len(rt.requests) != 1 || !strings.Contains(string(rt.requests[0]), "old.go") {
				t.Errorf("request does not carry the note text: %q", rt.requests)
			}
		})
	}
}
