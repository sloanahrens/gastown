package drift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// t0 is the cursor every test starts from; landings are stamped after it.
var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// recentFlagPatch is a diff that renames the flag a landing-battery usage line
// names: --recent becomes --last. Its doc is the section that still names it.
const recentFlagPatch = `diff --git a/internal/cmd/jev_landing.go b/internal/cmd/jev_landing.go
--- a/internal/cmd/jev_landing.go
+++ b/internal/cmd/jev_landing.go
@@ -12 +12 @@
-Read it with ` + "`gt jev landing --recent N`" + `.
+Read it with ` + "`gt jev landing --last N`" + `.
`

// landingDoc names --recent in its own section and nothing else the patch
// touches, so the grep fallback has exactly one section to pick.
const landingDoc = `# Jev batteries

## Landing battery

The pass asks Jev about each landing. Read it with ` + "`gt jev landing --recent N`" + `.

## Scorecard

The scorecard reads every battery's log against its labels.
`

// fakeLandings returns the records it holds and records each cursor it was
// asked from.
type fakeLandings struct {
	recs  []land.LandingRecord
	err   error
	since []time.Time
}

func (f *fakeLandings) Since(t time.Time) ([]land.LandingRecord, error) {
	f.since = append(f.since, t)
	return f.recs, f.err
}

// fakeRepo serves a scripted diff per head and a fixed set of docs at every
// revision. It records each sync.
type fakeRepo struct {
	syncErr error
	synced  int
	stat    map[string]string
	patch   map[string]string
	docs    map[string]string
	listErr error
}

func (f *fakeRepo) Sync(context.Context) error {
	f.synced++
	return f.syncErr
}

func (f *fakeRepo) Numstat(_ context.Context, _, head string) (string, error) {
	return f.stat[head], nil
}

func (f *fakeRepo) Patch(_ context.Context, _, head string) (string, error) {
	return f.patch[head], nil
}

func (f *fakeRepo) ListDocs(context.Context, string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var paths []string
	for p := range f.docs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (f *fakeRepo) ReadDoc(_ context.Context, _, path string) (string, error) {
	return f.docs[path], nil
}

// fakeAsker answers every Ask with answers, or fails with err, and records every
// state it was sent. Its first unusable Asks get an unusable response instead.
type fakeAsker struct {
	answers  *jev.Answers
	err      error
	unusable int
	states   []string
}

func (f *fakeAsker) Ask(_ context.Context, state any, _ map[string]jev.Question) (*jev.Answers, error) {
	f.states = append(f.states, state.(string))
	if f.unusable > 0 {
		f.unusable--
		return nil, jev.ErrBadResponse
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.answers, nil
}

// fakeRanker names the docs it is given, or fails.
type fakeRanker struct {
	paths []string
	err   error
}

func (f fakeRanker) Closest(context.Context, string) ([]string, error) {
	return f.paths, f.err
}

// goodAnswers answers every drift question: the section is stale and the
// drift is a flag.
func goodAnswers() *jev.Answers {
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul: map[string]float64{
			QDocNowStale:     0.9,
			QNeedsNewSection: 0.2,
		},
		Choice: map[string]jev.ChoiceAnswer{
			QDriftKind: {Choice: "flag", Probabilities: map[string]float64{"flag": 0.8, "none": 0.2}},
		},
	}
}

// rig is one pass over fakes, with its log and cursor under a temp dir.
type rig struct {
	pass     *Pass
	landings *fakeLandings
	git      *fakeRepo
	asker    *fakeAsker
	ranker   Ranker
	logPath  string
	cursor   string
	allowed  bool
}

func newRig(t *testing.T, recs ...land.LandingRecord) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		landings: &fakeLandings{recs: recs},
		git: &fakeRepo{
			stat:  map[string]string{},
			patch: map[string]string{},
			docs:  map[string]string{"docs/reference.md": landingDoc},
		},
		asker:   &fakeAsker{answers: goodAnswers()},
		logPath: filepath.Join(dir, "drift.jsonl"),
		cursor:  filepath.Join(dir, "drift.cursor.json"),
		allowed: true,
	}
	r.pass = &Pass{
		Asker:      r.asker,
		Landings:   r.landings,
		Git:        r.git,
		LogPath:    r.logPath,
		CursorPath: r.cursor,
		Allows:     func() (bool, error) { return r.allowed, nil },
		Now:        func() time.Time { return t0.Add(time.Hour) },
	}
	return r
}

// landed is a landing record for bead, landed after the given time, whose diff
// the rig serves under its head.
func landed(bead string, after time.Duration) land.LandingRecord {
	return land.LandingRecord{
		BeadID:   bead,
		Rig:      RigName,
		Branch:   "polecat/x/" + bead,
		Head:     "head-" + bead,
		Target:   "main",
		Base:     "base-" + bead,
		LandedAt: t0.Add(after),
	}
}

// serve scripts a landing's numstat and patch.
func (r *rig) serve(rec land.LandingRecord, stat, patch string) {
	r.git.stat[rec.Head] = stat
	r.git.patch[rec.Head] = patch
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
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		var line Line
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("bad log line %q: %v", l, err)
		}
		out = append(out, line)
	}
	return out
}

// TestJevDriftFirstPassOnlyStartsTheCursor: the first pass sets the cursor and
// asks about nothing, so a town that turns the patrol on does not ask about
// its whole history.
func TestJevDriftFirstPassOnlyStartsTheCursor(t *testing.T) {
	t.Parallel()
	r := newRig(t, landed("gt-a", time.Minute))
	r.serve(r.landings.recs[0], "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Started || sum.Asked != 0 || r.asker.states != nil {
		t.Errorf("first pass summary = %+v, asked %d calls, want Started only", sum, len(r.asker.states))
	}
	if r.git.synced != 0 {
		t.Errorf("first pass synced the clone %d times, want 0", r.git.synced)
	}
}

// TestJevDriftFlagRenameFixtureLogsTheMatchingSection is the acceptance fixture
// through the real Jev client: a landing whose diff renames --recent to --last
// produces one drift line that names the doc section still using --recent,
// with that section's answers.
func TestJevDriftFlagRenameFixtureLogsTheMatchingSection(t *testing.T) {
	t.Parallel()
	rt := &replyTo{reply: jevDriftReply()}
	client, err := jev.NewClient(jev.WithBaseURL("http://jev.invalid/v1/systemone"), jev.WithKey("test-key"),
		jev.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatal(err)
	}
	rec := landed("gt-flag", 2*time.Minute)
	r := newRig(t, rec)
	r.pass.Asker = client
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	// The cursor starts before the landing, so the pass asks about it.
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 {
		t.Fatalf("summary = %+v, want one landing asked", sum)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 1 {
		t.Fatalf("drift log has %d lines, want 1", len(lines))
	}
	line := lines[0]
	if line.Bead != "gt-flag" || line.QuestionVersion != Version || line.Model != "jev-1.13.0" {
		t.Errorf("line = %+v, want bead gt-flag, version %d, model jev-1.13.0", line, Version)
	}
	if len(line.Sections) != 1 {
		t.Fatalf("sections = %+v, want exactly the landing-battery section", line.Sections)
	}
	s := line.Sections[0]
	if s.Doc != "docs/reference.md" || s.Heading != "Landing battery" {
		t.Errorf("section = %s / %q, want docs/reference.md / Landing battery", s.Doc, s.Heading)
	}
	if got := s.Answers[QDocNowStale].Noul; got == nil || *got != 0.9 {
		t.Errorf("doc_now_stale = %v, want 0.9", got)
	}
	if got := s.Answers[QDriftKind].Choice; got != "flag" {
		t.Errorf("drift_kind = %q, want flag", got)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("made %d Jev calls, want one per section", len(rt.requests))
	}
	if sent := string(rt.requests[0]); !strings.Contains(sent, "doc_section") || !strings.Contains(sent, "--recent N") {
		t.Errorf("request does not carry the section text:\n%s", sent)
	}
}

// jevDriftReply answers the drift battery's three questions for one section.
func jevDriftReply() string {
	return `{
  "model": "jev-1.13.0",
  "answers": {
    "doc_now_stale": {"type": "noul", "noul": 0.9},
    "needs_new_section": {"type": "noul", "noul": 0.2},
    "drift_kind": {"type": "choice", "choice": "flag", "confidence": 0.8,
      "probabilities": {"command": 0.05, "flag": 0.8, "config_key": 0.05, "behavior": 0.02, "none": 0.05, "unclear": 0.03}}
  },
  "usage": {"input_tokens": 100, "output_tokens": 20}
}`
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

// TestJevDriftChoiceAnswerIgnoresProbabilityOrder: the drift_kind answer is the
// option with the highest probability, read by name, whichever order Jev
// sends the probabilities in.
func TestJevDriftChoiceAnswerIgnoresProbabilityOrder(t *testing.T) {
	t.Parallel()
	orders := []struct {
		name, probabilities string
	}{
		{"sorted keys", `{"behavior": 0.02, "command": 0.05, "config_key": 0.05, "flag": 0.8, "none": 0.05, "unclear": 0.03}`},
		{"reversed keys", `{"unclear": 0.03, "none": 0.05, "flag": 0.8, "config_key": 0.05, "command": 0.05, "behavior": 0.02}`},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			reply := strings.Replace(jevDriftReply(), `"probabilities": {"command": 0.05, "flag": 0.8, "config_key": 0.05, "behavior": 0.02, "none": 0.05, "unclear": 0.03}`, `"probabilities": `+order.probabilities, 1)
			client, err := jev.NewClient(jev.WithBaseURL("http://jev.invalid/v1/systemone"), jev.WithKey("test-key"),
				jev.WithHTTPClient(&http.Client{Transport: &replyTo{reply: reply}}))
			if err != nil {
				t.Fatal(err)
			}
			rec := landed("gt-order", 2*time.Minute)
			r := newRig(t, rec)
			r.pass.Asker = client
			r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
			if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pass.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			lines := readLines(t, r.logPath)
			if len(lines) != 1 || len(lines[0].Sections) != 1 {
				t.Fatalf("log = %+v, want one line with one section", lines)
			}
			ans := lines[0].Sections[0].Answers[QDriftKind]
			if ans.Choice != "flag" || ans.Probabilities["flag"] != 0.8 {
				t.Errorf("drift_kind = %+v, want flag at 0.8", ans)
			}
		})
	}
}

// TestJevDriftGrepFallbackPicksTheNamingSection: with no ranker the pass
// greps every doc for the diff's identifiers, and the section that names the
// flag wins.
func TestJevDriftGrepFallbackPicksTheNamingSection(t *testing.T) {
	t.Parallel()
	rec := landed("gt-grep", 2*time.Minute)
	r := newRig(t, rec)
	r.git.docs = map[string]string{
		"docs/reference.md":  landingDoc,
		"docs/writing.md":    "# Writing\n\nNothing about landings here.\n",
		"docs/operations.md": "# Operations\n\n## Patrols\n\nThe patrols run on the heartbeat.\n",
	}
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 1 || len(lines[0].Sections) != 1 {
		t.Fatalf("log = %+v, want one section", lines)
	}
	if got := lines[0].Sections[0]; got.Doc != "docs/reference.md" || got.Heading != "Landing battery" {
		t.Errorf("section = %+v, want the Landing battery section", got)
	}
}

// TestJevDriftRankerNamesTheDocsFirst: a ranker that names a doc with a
// matching section searches that doc alone, even when another doc matches too.
// A ranker that names no matching doc falls back to grepping every doc.
func TestJevDriftRankerNamesTheDocsFirst(t *testing.T) {
	t.Parallel()
	docs := map[string]string{
		"docs/reference.md": landingDoc,
		"docs/guide.md":     "# Guide\n\n## Recent\n\nRun gt jev landing --recent N to read it.\n",
	}
	tests := []struct {
		name     string
		ranked   []string
		wantDocs []string
	}{
		{"ranked doc matches", []string{"docs/reference.md"}, []string{"docs/reference.md"}},
		{"ranked doc has no match", []string{"docs/missing.md"}, []string{"docs/guide.md", "docs/reference.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := landed("gt-hm", 2*time.Minute)
			r := newRig(t, rec)
			r.git.docs = docs
			r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
			r.pass.Ranker = fakeRanker{paths: tt.ranked}
			if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pass.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			lines := readLines(t, r.logPath)
			if len(lines) != 1 {
				t.Fatalf("log has %d lines, want 1", len(lines))
			}
			var got []string
			for _, s := range lines[0].Sections {
				got = append(got, s.Doc)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantDocs, ",") {
				t.Errorf("sections in %v, want %v", got, tt.wantDocs)
			}
		})
	}
}

// TestJevDriftRankerFailureFallsBackToGrep: a ranker that fails leaves the
// pass to search the docs itself, and the pass says why on its warnings.
func TestJevDriftRankerFailureFallsBackToGrep(t *testing.T) {
	t.Parallel()
	rec := landed("gt-hmdown", 2*time.Minute)
	r := newRig(t, rec)
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	r.pass.Ranker = fakeRanker{err: errors.New("hm: not indexed")}
	var warnings strings.Builder
	r.pass.Warnings = &warnings
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 1 || len(lines[0].Sections) != 1 {
		t.Fatalf("log = %+v, want one section from the grep fallback", lines)
	}
	if !strings.Contains(warnings.String(), "hm: not indexed") {
		t.Errorf("warnings = %q, want the ranker's failure named", warnings.String())
	}
}

// TestJevDriftLandingWithoutDocsRelevantChangeGetsALineWithNoSections: a diff
// that touches only docs or tests asks nothing and still logs the landing.
func TestJevDriftLandingWithoutDocsRelevantChangeGetsALineWithNoSections(t *testing.T) {
	t.Parallel()
	rec := landed("gt-docsonly", 2*time.Minute)
	r := newRig(t, rec)
	r.serve(rec, "3\t1\tdocs/reference.md\n", "")
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || len(r.asker.states) != 0 {
		t.Fatalf("summary = %+v, asks = %d; want one line and no Ask", sum, len(r.asker.states))
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 1 || lines[0].Sections == nil || len(lines[0].Sections) != 0 {
		t.Errorf("log = %+v, want one line with an empty sections list", lines)
	}
}

// TestJevDriftReadFailureStopsThePass: a git read that fails for a reason other
// than a refused ref stops the pass with the cursor on the failing landing, so
// the next pass reads it again and the landings behind it wait.
func TestJevDriftReadFailureStopsThePass(t *testing.T) {
	t.Parallel()
	bad := landed("gt-bad", time.Minute)
	good := landed("gt-good", 2*time.Minute)
	r := newRig(t, bad, good)
	r.serve(good, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	r.serve(bad, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	badRecord := &perRecordErr{Repo: r.git, errs: map[string]error{bad.Head: errors.New("bad object")}}
	r.pass.Git = badRecord
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err == nil || errors.Is(err, errSkip) {
		t.Fatalf("err = %v, want the read failure to stop the pass", err)
	}
	if lines := readLines(t, r.logPath); len(lines) != 0 {
		t.Errorf("log = %+v, want nothing before the failing landing", lines)
	}
	cur, _, _ := battery.ReadCursor(r.cursor)
	if !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v, want it unmoved", cur)
	}

	badRecord.errs = nil
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 2 || sum.Skipped != 0 {
		t.Errorf("summary = %+v, want the next pass to ask both landings", sum)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 2 || lines[0].Bead != "gt-bad" || lines[1].Bead != "gt-good" {
		t.Errorf("log = %+v, want gt-bad then gt-good", lines)
	}
}

// TestJevDriftRefusedRefSkipsTheLanding: a landing whose refs the pass refuses
// to hand to git gets no line, and the landings behind it are still asked.
func TestJevDriftRefusedRefSkipsTheLanding(t *testing.T) {
	t.Parallel()
	bad := landed("gt-bad", time.Minute)
	good := landed("gt-good", 2*time.Minute)
	r := newRig(t, bad, good)
	r.serve(good, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	refused := fmt.Errorf("%w: %q", ErrBadRef, bad.Head)
	r.pass.Git = &perRecordErr{Repo: r.git, errs: map[string]error{bad.Head: refused}}
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 1 || sum.Asked != 1 {
		t.Errorf("summary = %+v, want one skipped and one asked", sum)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 1 || lines[0].Bead != "gt-good" {
		t.Errorf("log = %+v, want only gt-good", lines)
	}
	cur, _, _ := battery.ReadCursor(r.cursor)
	if cur.ID != "gt-good" {
		t.Errorf("cursor = %+v, want it past gt-good", cur)
	}
}

// TestJevDriftPartialAnswersCountTheUnanswered: when Jev gives an unusable
// response for one of a landing's sections, the line still holds the answered
// section and says how many it left unanswered.
func TestJevDriftPartialAnswersCountTheUnanswered(t *testing.T) {
	t.Parallel()
	rec := landed("gt-partial", 2*time.Minute)
	r := newRig(t, rec)
	r.git.docs["docs/reference.md"] = "# Ref\n\n## Recent\n\nUse `gt jev landing --recent N`.\n\n## Last\n\nAlso --recent here.\n"
	r.asker.unusable = 1
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 {
		t.Fatalf("summary = %+v, want the landing logged", sum)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 1 {
		t.Fatalf("log = %+v, want one line", lines)
	}
	if got := lines[0]; got.Unanswered != 1 || len(got.Sections) != 1 || got.Sections[0].Heading != "Last" {
		t.Errorf("line = %+v, want the second section answered and one unanswered", got)
	}
}

// perRecordErr fails the numstat of the heads it names and passes the rest to
// Repo.
type perRecordErr struct {
	Repo
	errs map[string]error
}

func (p *perRecordErr) Numstat(ctx context.Context, base, head string) (string, error) {
	if err := p.errs[head]; err != nil {
		return "", err
	}
	return p.Repo.Numstat(ctx, base, head)
}

// TestJevDriftSyncFailureStopsThePass: a clone that cannot be fetched asks
// about nothing and leaves the cursor where it was.
func TestJevDriftSyncFailureStopsThePass(t *testing.T) {
	t.Parallel()
	rec := landed("gt-sync", 2*time.Minute)
	r := newRig(t, rec)
	r.git.syncErr = errors.New("fetch refused")
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	_, err := r.pass.Run(context.Background())
	if !errors.Is(err, ErrFetch) {
		t.Fatalf("err = %v, want ErrFetch", err)
	}
	if len(r.asker.states) != 0 {
		t.Errorf("asked %d times after a failed fetch, want 0", len(r.asker.states))
	}
	cur, _, _ := battery.ReadCursor(r.cursor)
	if !cur.Time.Equal(t0) {
		t.Errorf("cursor moved to %v, want %v", cur.Time, t0)
	}
}

// TestJevDriftStopsWhenTheCapIsSpent: with the day's spend at the cap the
// pass writes nothing for the landing it stopped in, and that landing is asked
// again in full by the next pass.
func TestJevDriftStopsWhenTheCapIsSpent(t *testing.T) {
	t.Parallel()
	rec := landed("gt-cap", 2*time.Minute)
	r := newRig(t, rec)
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	r.allowed = false
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Asked != 0 || len(r.asker.states) != 0 {
		t.Errorf("summary = %+v, asks = %d; want the cap reached with no ask", sum, len(r.asker.states))
	}
	if lines := readLines(t, r.logPath); len(lines) != 0 {
		t.Errorf("log = %+v, want nothing", lines)
	}
	cur, _, _ := battery.ReadCursor(r.cursor)
	if cur.ID != "" || !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v, want it unmoved", cur)
	}
}

// TestJevDriftSkipsALandingWhoseEverySectionIsUnusable: when Jev returns an
// unusable response for every section a landing names, the landing is skipped
// with no line.
func TestJevDriftSkipsALandingWhoseEverySectionIsUnusable(t *testing.T) {
	t.Parallel()
	rec := landed("gt-junk", 2*time.Minute)
	r := newRig(t, rec)
	r.asker.err = jev.ErrBadResponse
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 1 || sum.Asked != 0 {
		t.Errorf("summary = %+v, want one skipped", sum)
	}
	if lines := readLines(t, r.logPath); len(lines) != 0 {
		t.Errorf("log = %+v, want nothing", lines)
	}
}

// TestJevDriftAskFailureStopsThePass: an Ask that fails for a reason other
// than an unusable response stops the pass and leaves the cursor on the
// landing, so the next pass asks again.
func TestJevDriftAskFailureStopsThePass(t *testing.T) {
	t.Parallel()
	rec := landed("gt-down", 2*time.Minute)
	r := newRig(t, rec)
	r.asker.err = jev.ErrTimeout
	r.serve(rec, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); !errors.Is(err, jev.ErrTimeout) {
		t.Fatalf("err = %v, want jev.ErrTimeout", err)
	}
	cur, _, _ := battery.ReadCursor(r.cursor)
	if !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v, want it unmoved", cur)
	}
}

// TestJevDriftNothingPendingDoesNotSync: a pass with no landings since the
// cursor does not fetch the clone.
func TestJevDriftNothingPendingDoesNotSync(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.git.synced != 0 {
		t.Errorf("synced %d times with nothing pending, want 0", r.git.synced)
	}
}

// TestJevDriftPendingTakesEveryLandingInOrder: every landing counts, not only
// the risk-path ones, and ties on the landing time break by bead ID.
func TestJevDriftPendingTakesEveryLandingInOrder(t *testing.T) {
	t.Parallel()
	risky := landed("gt-b", 2*time.Minute)
	risky.RiskPaths = []string{"internal/daemon/x.go"}
	plain := landed("gt-a", 2*time.Minute)
	early := landed("gt-c", time.Minute)
	got := pending([]land.LandingRecord{risky, plain, early}, battery.Cursor{Time: t0})
	var ids []string
	for _, r := range got {
		ids = append(ids, r.BeadID)
	}
	if want := "gt-c,gt-a,gt-b"; strings.Join(ids, ",") != want {
		t.Errorf("pending = %s, want %s", strings.Join(ids, ","), want)
	}
}

// TestJevDriftRerunAsksNoLandingTwice: a second pass over the same cursor
// finds nothing new to ask.
func TestJevDriftRerunAsksNoLandingTwice(t *testing.T) {
	t.Parallel()
	first := landed("gt-1", time.Minute)
	second := landed("gt-2", 2*time.Minute)
	r := newRig(t, first, second)
	r.serve(first, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	r.serve(second, "1\t1\tinternal/cmd/jev_landing.go\n", recentFlagPatch)
	if err := battery.WriteCursor(r.cursor, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, r.logPath)
	if len(lines) != 2 || lines[0].Bead != "gt-1" || lines[1].Bead != "gt-2" {
		t.Errorf("log has %d lines, want gt-1 then gt-2 once each", len(lines))
	}
}

// TestJevDriftQuestionSet: the three questions the battery asks, with the
// choice's options named.
func TestJevDriftQuestionSet(t *testing.T) {
	t.Parallel()
	qs := Questions()
	if len(qs) != 3 {
		t.Fatalf("questions = %d, want 3", len(qs))
	}
	if qs[QDocNowStale].Type != "noul" || qs[QNeedsNewSection].Type != "noul" {
		t.Errorf("noul questions have types %q and %q", qs[QDocNowStale].Type, qs[QNeedsNewSection].Type)
	}
	kind := qs[QDriftKind]
	if kind.Type != "choice" {
		t.Fatalf("drift_kind type = %q, want choice", kind.Type)
	}
	for _, opt := range []string{"command", "flag", "config_key", "behavior", "none", "unclear"} {
		if _, ok := kind.Options[opt]; !ok {
			t.Errorf("drift_kind lacks option %q", opt)
		}
	}
}

func TestJevDriftSplitSectionsKeepsFencedHashesAsBody(t *testing.T) {
	t.Parallel()
	doc := "# Top\n\nintro\n\n## Usage\n\n```bash\n# not a heading\ngt jev landing --recent N\n```\n\n## Next\n\nbody\n"
	secs := splitSections("docs/x.md", doc)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if want := "Top,Usage,Next"; strings.Join(headings, ",") != want {
		t.Errorf("headings = %v, want %s", headings, want)
	}
	if !strings.Contains(secs[1].Text, "# not a heading") {
		t.Errorf("usage section lost its fenced line: %q", secs[1].Text)
	}
}

func TestJevDriftIdentifiersReadOnlyChangedLines(t *testing.T) {
	t.Parallel()
	patch := "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-  flag --recent\n+  flag --last\n context_line_only --never-here\n"
	got := identifiers(patch)
	joined := strings.Join(got, ",")
	for _, want := range []string{"--recent", "--last"} {
		if !strings.Contains(joined, want) {
			t.Errorf("identifiers = %v, missing %s", got, want)
		}
	}
	if strings.Contains(joined, "never-here") {
		t.Errorf("identifiers = %v, took a context line", got)
	}
}

func TestJevDriftCloneRepoRefusesOptionRefs(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"-x", "main..x", "main:docs", "main x", ""} {
		if err := checkRefs(bad); !errors.Is(err, ErrBadRef) {
			t.Errorf("checkRefs(%q) = %v, want ErrBadRef", bad, err)
		}
	}
	if err := checkRefs("refs/heads/main", "abc123", "refs/heads/feature+1"); err != nil {
		t.Errorf("checkRefs refused a plain ref: %v", err)
	}
}

// TestJevDriftCloneRepoRefusesBadDocPaths: ReadDoc takes only paths under
// docs/, and refuses before git runs, so a bad path never reaches the clone.
func TestJevDriftCloneRepoRefusesBadDocPaths(t *testing.T) {
	t.Parallel()
	var r cloneRepo
	for _, path := range []string{"README.md", "docs/../mayor/rig/x.md", "-docs/x.md"} {
		if _, err := r.ReadDoc(context.Background(), "refs/heads/main", path); !errors.Is(err, ErrBadRef) {
			t.Errorf("ReadDoc(%q) = %v, want ErrBadRef", path, err)
		}
	}
}

// cannedClone answers every Run with out, the way git would print it.
type cannedClone struct {
	out string
}

func (c cannedClone) Sync(context.Context) error { return nil }

func (c cannedClone) Run(context.Context, ...string) (string, error) { return c.out, nil }

// TestJevDriftListDocsSplitsNULDelimitedPaths: ls-tree -z ends each path with
// NUL, so a name with a space or a non-ASCII byte arrives whole, and a
// non-Markdown entry is left out.
func TestJevDriftListDocsSplitsNULDelimitedPaths(t *testing.T) {
	t.Parallel()
	r := cloneRepo{c: cannedClone{out: "docs/café.md\x00docs/a b.md\x00docs/notes.txt\x00"}}
	got, err := r.ListDocs(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if want := "docs/café.md,docs/a b.md"; strings.Join(got, ",") != want {
		t.Errorf("ListDocs = %v, want %s", got, want)
	}
}

// TestJevDriftRankTiesGoToTheEarlierSection: sections the same score in one
// doc rank by their place in it, whatever order rank is handed them in.
func TestJevDriftRankTiesGoToTheEarlierSection(t *testing.T) {
	t.Parallel()
	secs := splitSections("docs/x.md", "## First\n\nNames --recent here.\n\n## Second\n\nAlso --recent here.\n")
	got := rank([]string{"--recent"}, []Section{secs[1], secs[0]})
	if len(got) != 2 || got[0].Heading != "First" || got[1].Heading != "Second" {
		t.Errorf("rank = %+v, want First then Second", got)
	}
}

func TestJevDriftParseDocPathsReadsHitTextInRankOrder(t *testing.T) {
	t.Parallel()
	out := []byte(`{"hits":[
  {"title":"see docs/reference.md","body":"and docs/operations.md"},
  {"title":"commit","body":"touched docs/reference.md again"}
]}`)
	got, err := parseDocPaths(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "docs/reference.md,docs/operations.md"; strings.Join(got, ",") != want {
		t.Errorf("paths = %v, want %s", got, want)
	}
	if _, err := parseDocPaths([]byte("not json")); err == nil {
		t.Error("parseDocPaths accepted a non-JSON reply")
	}
}
