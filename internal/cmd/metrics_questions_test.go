package cmd

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Unit tier: the command over fakes; no real files, git or clock.

func questionsFakeEnv(files map[string]string) questionsEnv {
	town := "/town"
	return questionsEnv{
		Town: town,
		Now:  func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) },
		ReadFile: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
		Glob: func(pattern string) ([]string, error) {
			return []string{"/town/.runtime/metrics/2026-10-31", "/town/.runtime/metrics/2026-10-30", "/town/.runtime/metrics/bin"}, nil
		},
		GitList: func(_ context.Context, repo, dir string) ([]string, error) {
			if repo != "/town/.dolt-archive/git" || dir != "gt" {
				return nil, errors.New("unexpected list " + repo + " " + dir)
			}
			return []string{"gt/events.2.jsonl", "gt/events.jsonl", "gt/issues.jsonl", "gt/labels.jsonl"}, nil
		},
		GitShow: func(_ context.Context, repo, spec string) ([]byte, error) {
			if repo != "/town/.dolt-archive/git" {
				return nil, errors.New("unexpected repo " + repo)
			}
			switch spec {
			case "HEAD:gt/events.jsonl": // the later shard first in the listing order below does not matter
				return []byte(`{"issue_id":"gt-a","event_type":"label_added","comment":"Added label: gt:ready-to-land","created_at":"2026-11-01 12:00:00"}` + "\n" +
					`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-11-01 12:30:00"}` + "\n"), nil
			case "HEAD:gt/events.2.jsonl": // the created event lives in the older shard
				return []byte(`{"issue_id":"gt-a","event_type":"created","created_at":"2026-11-01 10:00:00"}` + "\n"), nil
			}
			return nil, errors.New("not an events file: " + spec)
		},
	}
}

func TestMetricsQuestionsPrintsAllTenWithTheLatestLedger(t *testing.T) {
	t.Parallel()
	rt := "/town/.runtime"
	env := questionsFakeEnv(map[string]string{
		filepath.Join(rt, "landings", "reviews.jsonl"): `{"bead":"gt-a","attempt":2,"at":"2026-11-01T01:00:00Z"}` + "\n",
		filepath.Join(rt, "metrics", "snapshots.jsonl"): `{"ts":"old","host":{"load1":1}}` + "\n" +
			`{"ts":"2026-11-01T16:13:49Z","host":{"load1":13.5},"queue":{"ready":215},"money":{"deepseek_spend_age_s":60,"deepseek_balance":74.55}}` + "\n",
		filepath.Join(rt, "metrics", "2026-10-31", "landings.csv"): "bead,rig,landed_at,route\ngt-a,gastown,2026-11-01T12:30:00Z,daemon\n",
	})
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		" 3. Review loop", "1 beads reviewed", // review log read
		"load1 13.5",         // the LAST snapshot line, not the first
		"balance $74.55",     // fresh balance printed
		"ready 215",          // backlog
		"11-01 2.0/0.50/2.5", // time to land from the fake archive events
		"Ledger 2026-10-31",  // the newest dated dir, not bin
		"10. Key and credential health",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestMetricsQuestionsNamesAnUnreadableSourceAndKeepsGoing(t *testing.T) {
	t.Parallel()
	env := questionsFakeEnv(map[string]string{}) // every file missing
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"review log unreadable", "snapshot log unreadable", "landing ledger unreadable", "note: review log:", "note: snapshot log:", " 7. Onboarding friction"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// questionsFailAfter is a writer that fails once n bytes have been written.
type questionsFailAfter struct{ left int }

func (w *questionsFailAfter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("write refused")
	}
	w.left -= len(p)
	return len(p), nil
}

func TestMetricsQuestionsReportsAWriteFailureOfTheTable(t *testing.T) {
	t.Parallel()
	err := runMetricsQuestions(context.Background(), &questionsFailAfter{left: 0}, questionsFakeEnv(map[string]string{}), 7)
	if err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Errorf("a refused write of the table must be returned, got %v", err)
	}
}

func TestMetricsQuestionsReportsAWriteFailureOfTheNotes(t *testing.T) {
	t.Parallel()
	// Room for the whole table but not for the first note line: measure the table first.
	var table bytes.Buffer
	env := questionsFakeEnv(map[string]string{})
	if err := runMetricsQuestions(context.Background(), &table, env, 7); err != nil {
		t.Fatal(err)
	}
	idx := strings.Index(table.String(), "note: ")
	if idx < 0 {
		t.Fatalf("fixture must produce a note line:\n%s", table.String())
	}
	err := runMetricsQuestions(context.Background(), &questionsFailAfter{left: idx + 1}, env, 7)
	if err == nil {
		t.Error("a refused write of a note line must be returned")
	}
}

func TestMetricsQuestionsInReturnsTheTownLookupError(t *testing.T) {
	t.Parallel()
	want := errors.New("not in a town")
	err := runMetricsQuestionsIn(context.Background(), &bytes.Buffer{}, func() (string, error) { return "", want }, 7)
	if !errors.Is(err, want) {
		t.Errorf("got %v, want the lookup error", err)
	}
}

func TestMetricsQuestionsJoinsAnIssueWhoseEventsSpanTwoShards(t *testing.T) {
	t.Parallel()
	env := questionsFakeEnv(map[string]string{
		"/town/.runtime/metrics/2026-10-31/landings.csv": "bead,rig,landed_at,route\ngt-a,gastown,2026-11-01T12:30:00Z,daemon\n",
	})
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "11-01 2.0/0.50/2.5") {
		t.Errorf("created in one shard and submitted/closed in another must still join:\n%s", out.String())
	}
}

func questionsLedgerOnly() map[string]string {
	return map[string]string{
		"/town/.runtime/metrics/2026-10-31/landings.csv": "bead,rig,landed_at,route\ngt-a,gastown,2026-11-01T12:30:00Z,daemon\n",
	}
}

func TestMetricsQuestionsSaysWhenEveryEventFileIsUnreadable(t *testing.T) {
	t.Parallel()
	env := questionsFakeEnv(questionsLedgerOnly())
	env.GitShow = func(context.Context, string, string) ([]byte, error) { return nil, errors.New("object missing") }
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tracker export events unreadable (2 event files listed, none readable)", "note: tracker export gt/events.jsonl: object missing", "note: tracker export gt/events.2.jsonl: object missing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestMetricsQuestionsSaysWhenTheExportCannotBeListed(t *testing.T) {
	t.Parallel()
	env := questionsFakeEnv(questionsLedgerOnly())
	env.GitList = func(context.Context, string, string) ([]string, error) {
		return nil, errors.New("not a git repository")
	}
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tracker export events unreadable", "note: tracker export: not a git repository"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestMetricsQuestionsDistinguishesAListingErrorFromNoLedger(t *testing.T) {
	t.Parallel()
	env := questionsFakeEnv(questionsLedgerOnly())
	env.Glob = func(string) ([]string, error) { return nil, errors.New("permission denied") }
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the ledger directories could not be listed", "note: ledger directories: permission denied"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	env.Glob = func(string) ([]string, error) { return nil, nil }
	out.Reset()
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no dated landing ledger under .runtime/metrics") || strings.Contains(out.String(), "note: ledger directories") {
		t.Errorf("an empty directory must read as no ledger, with no error note:\n%s", out.String())
	}
}

// TestMetricsQuestionsReadsTheProductionLayout runs the real file wiring over a
// temporary town laid out like the real one: runtime files under .runtime and
// the export repository at .dolt-archive/git. Only the two git calls are
// replaced, and they assert the repository path the command asks about.
func TestMetricsQuestionsReadsTheProductionLayout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(town, ".runtime", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("landings/reviews.jsonl", `{"bead":"gt-a","attempt":2,"at":"2026-11-01T01:00:00Z"}`+"\n")
	write("metrics/snapshots.jsonl", `{"ts":"2026-11-01T16:13:49Z","host":{"load1":13.5},"queue":{"ready":215},"money":{"deepseek_spend_age_s":60,"deepseek_balance":74.55}}`+"\n")
	write("metrics/2026-10-31/landings.csv", "bead,rig,landed_at,route\ngt-a,gastown,2026-11-01T12:30:00Z,daemon\n")
	env := questionsOSEnv(town)
	env.Now = func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) }
	wantRepo := filepath.Join(town, ".dolt-archive", "git")
	env.GitList = func(_ context.Context, repo, dir string) ([]string, error) {
		if repo != wantRepo || dir != "gt" {
			t.Errorf("GitList(%q, %q), want (%q, gt)", repo, dir, wantRepo)
		}
		return []string{"gt/events.jsonl"}, nil
	}
	env.GitShow = func(_ context.Context, repo, spec string) ([]byte, error) {
		if repo != wantRepo {
			t.Errorf("GitShow repo %q, want %q", repo, wantRepo)
		}
		return []byte(`{"issue_id":"gt-a","event_type":"created","created_at":"2026-11-01 10:00:00"}` + "\n" +
			`{"issue_id":"gt-a","event_type":"label_added","comment":"Added label: gt:ready-to-land","created_at":"2026-11-01 12:00:00"}` + "\n" +
			`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-11-01 12:30:00"}` + "\n"), nil
	}
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 beads reviewed", "load1 13.5", "balance $74.55", "ready 215", "11-01 2.0/0.50/2.5", "Ledger 2026-10-31"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "note: ") {
		t.Errorf("the production layout must read without notes:\n%s", out.String())
	}
}

// TestQuestionsGitReadersAgainstARealRepository runs the two read-only git
// helpers against a throwaway repository under t.TempDir(): one commit with an
// events file, and a directory that is not a repository for the failure path.
func TestQuestionsGitReadersAgainstARealRepository(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, "gt"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"events.jsonl", "events.2.jsonl", "issues.jsonl"} {
		if err := os.WriteFile(filepath.Join(repo, "gt", f), []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "gt")
	git("commit", "-q", "-m", "export")

	names, err := questionsGitList(context.Background(), repo, "gt")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "gt/events.2.jsonl,gt/events.jsonl,gt/issues.jsonl" {
		t.Errorf("listed %q", got)
	}
	b, err := questionsGitShow(context.Background(), repo, "HEAD:gt/events.jsonl")
	if err != nil || string(b) != "events.jsonl\n" {
		t.Errorf("show = %q, %v", b, err)
	}
	if _, err := questionsGitShow(context.Background(), repo, "HEAD:gt/nope.jsonl"); err == nil {
		t.Error("showing a file that is not in the commit must fail")
	}
	if _, err := questionsGitList(context.Background(), t.TempDir(), "gt"); err == nil {
		t.Error("listing a directory that is not a repository must fail")
	}
}
