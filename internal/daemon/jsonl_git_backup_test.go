package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

func TestGitIdentityEnv_NothingToAddWhenPresent(t *testing.T) {
	t.Parallel()
	base := []string{"HOME=/tmp/fake-home", "USER=fakeuser", "LOGNAME=fakeuser", "SSH_AUTH_SOCK=/tmp/fake-agent.sock"}
	noLookup := func() (*user.User, error) {
		t.Error("identity looked up although HOME, USER and LOGNAME were all set")
		return nil, errors.New("unexpected lookup")
	}
	if got := gitIdentityEnvFrom(base, noLookup); len(got) != 0 {
		t.Errorf("added %q; want nothing when the identity is all there", got)
	}
}

func TestGitIdentityEnv_RecoversMissingIdentity(t *testing.T) {
	t.Parallel()
	// A daemon child that lost USER/LOGNAME (the gh#zt1w failure mode). An
	// empty "USER=" counts as present, so the keys are absent, not empty.
	base := []string{"HOME=/tmp/fake-home", "PATH=/usr/bin"}
	current := func() (*user.User, error) {
		return &user.User{Username: "recovered", HomeDir: "/home/recovered"}, nil
	}
	got := gitIdentityEnvFrom(base, current)
	if want := []string{"USER=recovered", "LOGNAME=recovered"}; !slices.Equal(got, want) {
		t.Errorf("added %q, want %q (HOME was already set)", got, want)
	}
}

func TestGitIdentityEnv_LookupFailureAddsNothing(t *testing.T) {
	t.Parallel()
	got := gitIdentityEnvFrom([]string{"PATH=/usr/bin"}, func() (*user.User, error) {
		return nil, errors.New("no user exists for uid 501")
	})
	if len(got) != 0 {
		t.Errorf("added %q when the lookup failed; want nothing", got)
	}
}

// TestBackupGitDefaultsToRealGit is the wiring guard for backupGitAt's nil
// path: a *git.Git on the repository. The identity and deadline it carries
// are exercised on real git by the integration tier's backup tests.
func TestBackupGitDefaultsToRealGit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if g, ok := (&Daemon{}).backupGitAt(dir, time.Minute).(*gtgit.Git); !ok || g.WorkDir() != dir {
		t.Fatalf("backupGitAt = %T; want a *git.Git on %s", (&Daemon{}).backupGitAt(dir, time.Minute), dir)
	}
}

func TestEnsureGitRepoInitialized_CreatesMissingRepo(t *testing.T) {
	t.Parallel()
	d, f := backupTestDaemon(t)
	gitRepo := filepath.Join(t.TempDir(), "nested", "git")

	if _, err := os.Stat(gitRepo); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to exist yet", gitRepo)
	}
	if err := d.ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitRepo, ".git")); err != nil {
		t.Fatalf("expected %s/.git to exist after init: %v", gitRepo, err)
	}
	if branch := f.Open(gitRepo).DefaultBranch(); branch != "main" {
		t.Errorf("new repository's branch = %q, want main", branch)
	}
}

func TestEnsureGitRepoInitialized_NoOpOnExistingRepo(t *testing.T) {
	t.Parallel()
	d, f := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if err := d.ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized on existing repo: %v", err)
	}
	if headAfter, err := f.Open(gitRepo).Rev("HEAD"); err != nil || headAfter != head {
		t.Errorf("existing repo history changed: before %q, after %q (%v)", head, headAfter, err)
	}
}

func TestEnsureGitRepoInitialized_SetsPostBufferOnFreshRepo(t *testing.T) {
	t.Parallel()
	d, f := backupTestDaemon(t)
	gitRepo := t.TempDir()

	if err := d.ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized: %v", err)
	}
	if got := gitConfigGet(t, f, gitRepo, "http.postBuffer"); got != gitPostBufferBytes {
		t.Errorf("http.postBuffer = %q, want %q", got, gitPostBufferBytes)
	}
}

func TestEnsureGitRepoInitialized_SetsPostBufferOnExistingRepo(t *testing.T) {
	t.Parallel()
	// Regression for gt-kxa1: a repo initialized before this fix (or with the
	// config lost some other way) must also get covered, not just fresh inits.
	d, f := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)

	if err := d.ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized on existing repo: %v", err)
	}
	if got := gitConfigGet(t, f, gitRepo, "http.postBuffer"); got != gitPostBufferBytes {
		t.Errorf("http.postBuffer = %q, want %q", got, gitPostBufferBytes)
	}
}

func gitConfigGet(t *testing.T, f *gitfake.Fake, gitRepo, key string) string {
	t.Helper()
	v, err := f.Open(gitRepo).(gitfake.WorkTree).ConfigGet(key)
	if err != nil {
		t.Fatalf("config --get %s: %v", key, err)
	}
	return v
}

func TestIsPostBufferPushError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{
			name:   "github chunked-POST rejection",
			errMsg: "git push: error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400",
			want:   true,
		},
		{
			name:   "bare curl 22 without HTTP 400 substring",
			errMsg: "git push: curl 22 error sending request",
			want:   true,
		},
		{
			name:   "unrelated auth failure",
			errMsg: "git push: fatal: Authentication failed for 'https://github.com/...'",
			want:   false,
		},
		{
			name:   "unrelated network timeout",
			errMsg: "git push: context deadline exceeded",
			want:   false,
		},
		{
			name:   "empty error",
			errMsg: "",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPostBufferPushError(tt.errMsg); got != tt.want {
				t.Errorf("isPostBufferPushError(%q) = %v, want %v", tt.errMsg, got, tt.want)
			}
		})
	}
}

func TestPostBufferHint_NamesCauseAndFix(t *testing.T) {
	t.Parallel()
	hint := postBufferHint("2.9 MiB")
	if !contains(hint, "http.postBuffer") {
		t.Errorf("hint should name http.postBuffer, got: %s", hint)
	}
	if !contains(hint, "2.9 MiB") {
		t.Errorf("hint should include the pack size when known, got: %s", hint)
	}
}

func TestPostBufferHint_OkWithoutPackSize(t *testing.T) {
	t.Parallel()
	hint := postBufferHint("")
	if !contains(hint, "http.postBuffer") {
		t.Errorf("hint should still name http.postBuffer with no pack size, got: %s", hint)
	}
}

func TestCommitAndPushJsonlBackup_NoRemoteIsError(t *testing.T) {
	t.Parallel()
	// Regression for gt-kme: a JSONL backup repo with commits but no configured
	// remote used to be reported as a silent success ("skipping push"), even
	// though the data never left the machine. It must now be a hard error so
	// the daemon's consecutive-failure escalation fires.
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)

	dbDir := filepath.Join(gitRepo, "testdb")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 5)

	err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil)
	if err == nil {
		t.Fatal("expected an error when no remote is configured, got nil")
	}
	if !contains(err.Error(), "remote") {
		t.Errorf("expected error to mention the missing remote, got: %v", err)
	}

	// The commit itself should still have happened locally.
	if subject := backupSubjects(t, d, gitRepo)[0]; !contains(subject, "backup") {
		t.Errorf("expected a local backup commit despite the push failure, got %q", subject)
	}
}

func TestDiscoverJsonlBackupDatabases(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	makeDbDir := func(name string) {
		if err := os.MkdirAll(filepath.Join(dataDir, name, ".dolt"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	makeDbDir("gt")
	makeDbDir("hq")
	makeDbDir("testdb_scratch") // excluded: test prefix
	makeDbDir("doctest_foo")    // excluded: test prefix
	if err := os.MkdirAll(filepath.Join(dataDir, ".doltcfg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "not-a-db"), 0755); err != nil {
		t.Fatal(err) // no .dolt subdir — should be excluded
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	got := discoverJsonlBackupDatabases(dataDir)
	want := []string{"gt", "hq"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func TestDiscoverJsonlBackupDatabases_MissingDataDir(t *testing.T) {
	t.Parallel()
	got := discoverJsonlBackupDatabases(filepath.Join(t.TempDir(), "does-not-exist"))
	if got != nil {
		t.Errorf("expected nil for missing data dir, got %v", got)
	}
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			m[kv[:eq]] = kv[eq+1:]
		}
	}
	return m
}

func TestIsTestPollution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		record   map[string]interface{}
		expected bool
	}{
		{
			name:     "normal issue",
			record:   map[string]interface{}{"id": "gt-abc1", "title": "Fix login bug"},
			expected: false,
		},
		{
			name:     "title starts with Test Issue",
			record:   map[string]interface{}{"id": "gt-xyz2", "title": "Test Issue for validation"},
			expected: true,
		},
		{
			name:     "title starts with test issue lowercase",
			record:   map[string]interface{}{"id": "gt-xyz2", "title": "test issue for validation"},
			expected: true,
		},
		{
			name:     "short test id bd-1",
			record:   map[string]interface{}{"id": "bd-1", "title": "Something"},
			expected: true,
		},
		{
			name:     "short test id bd-99",
			record:   map[string]interface{}{"id": "bd-99", "title": "Something"},
			expected: true,
		},
		{
			name:     "test-style id bd-abc12",
			record:   map[string]interface{}{"id": "bd-abc12", "title": "Something"},
			expected: true,
		},
		{
			name:     "testdb prefix id",
			record:   map[string]interface{}{"id": "testdb_foo", "title": "Something"},
			expected: true,
		},
		{
			name:     "beads_t prefix id",
			record:   map[string]interface{}{"id": "beads_t123", "title": "Something"},
			expected: true,
		},
		{
			name:     "beads_pt prefix id",
			record:   map[string]interface{}{"id": "beads_pt456", "title": "Something"},
			expected: true,
		},
		{
			name:     "doctest prefix id",
			record:   map[string]interface{}{"id": "doctest_foo", "title": "Something"},
			expected: true,
		},
		{
			name:     "title starts with test_",
			record:   map[string]interface{}{"id": "gt-ok1", "title": "test_something"},
			expected: true,
		},
		{
			name:     "title starts with test space",
			record:   map[string]interface{}{"id": "gt-ok1", "title": "test something"},
			expected: true,
		},
		{
			name:     "normal id with test in middle",
			record:   map[string]interface{}{"id": "gt-test1", "title": "Normal title"},
			expected: false,
		},
		{
			name:     "longer legitimate id bd-abcde12",
			record:   map[string]interface{}{"id": "bd-abcde12", "title": "Something"},
			expected: true,
		},
		{
			name:     "legitimate bd id bd-abcdef",
			record:   map[string]interface{}{"id": "bd-abcdef", "title": "Something"},
			expected: false,
		},
		{
			name:     "empty record",
			record:   map[string]interface{}{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTestPollution(tt.record)
			if got != tt.expected {
				t.Errorf("isTestPollution(%v) = %v, want %v", tt.record, got, tt.expected)
			}
		})
	}
}

func TestFilterTestPollution(t *testing.T) {
	t.Parallel()
	// Build JSONL with mix of good and bad records.
	good1, _ := json.Marshal(map[string]interface{}{"id": "gt-abc1", "title": "Fix bug"})
	good2, _ := json.Marshal(map[string]interface{}{"id": "gt-def2", "title": "Add feature"})
	bad1, _ := json.Marshal(map[string]interface{}{"id": "bd-1", "title": "test thing"})
	bad2, _ := json.Marshal(map[string]interface{}{"id": "gt-xyz3", "title": "Test Issue 42"})

	input := string(good1) + "\n" + string(bad1) + "\n" + string(good2) + "\n" + string(bad2) + "\n"

	filtered, removed := filterTestPollution([]byte(input))

	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	// Verify only good records remain.
	lines := splitNonEmpty(string(filtered))
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines after filter, got %d: %v", len(lines), lines)
	}

	// Verify the good records are preserved.
	for _, line := range lines {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("failed to parse filtered line: %v", err)
		}
		if isTestPollution(rec) {
			t.Errorf("test pollution record survived filtering: %v", rec)
		}
	}
}

func TestFilterTestPollution_NoRemoval(t *testing.T) {
	t.Parallel()
	good1, _ := json.Marshal(map[string]interface{}{"id": "gt-abc1", "title": "Fix bug"})
	good2, _ := json.Marshal(map[string]interface{}{"id": "gt-def2", "title": "Add feature"})
	input := string(good1) + "\n" + string(good2) + "\n"

	filtered, removed := filterTestPollution([]byte(input))

	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}

	lines := splitNonEmpty(string(filtered))
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

func TestFilterTestPollution_EmptyInput(t *testing.T) {
	t.Parallel()
	filtered, removed := filterTestPollution([]byte(""))
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
	if len(filtered) != 0 {
		t.Errorf("expected empty output, got %q", filtered)
	}
}

func TestSpikeThreshold(t *testing.T) {
	t.Parallel()
	// nil config → default
	if got := spikeThreshold(nil); got != defaultSpikeThreshold {
		t.Errorf("expected %v, got %v", defaultSpikeThreshold, got)
	}

	// nil SpikeThreshold → default
	config := &JsonlGitBackupConfig{}
	if got := spikeThreshold(config); got != defaultSpikeThreshold {
		t.Errorf("expected %v, got %v", defaultSpikeThreshold, got)
	}

	// custom threshold
	threshold := 0.10
	config.SpikeThreshold = &threshold
	if got := spikeThreshold(config); got != 0.10 {
		t.Errorf("expected 0.10, got %v", got)
	}

	// invalid threshold (> 1.0) → default
	invalid := 1.5
	config.SpikeThreshold = &invalid
	if got := spikeThreshold(config); got != defaultSpikeThreshold {
		t.Errorf("expected default for invalid threshold, got %v", got)
	}

	// zero threshold → default
	zero := 0.0
	config.SpikeThreshold = &zero
	if got := spikeThreshold(config); got != defaultSpikeThreshold {
		t.Errorf("expected default for zero threshold, got %v", got)
	}
}

func TestFormatSpikeReport(t *testing.T) {
	t.Parallel()
	spikes := []spikeInfo{
		{DB: "prod_beads", File: "prod_beads/issues.jsonl", Previous: 100, Current: 150, Delta: 0.50,
			Baseline: "prod_beads: 100 (HEAD), 90 (HEAD~1)"},
		{DB: "dev_beads", File: "dev_beads/issues.jsonl", Previous: 200, Current: 50, Delta: 0.75,
			Baseline: "dev_beads: 200 (cached)"},
	}
	report := formatSpikeReport(spikes)
	if report == "" {
		t.Fatal("expected non-empty report")
	}
	// Verify it mentions both databases.
	if got := report; !contains(got, "prod_beads") || !contains(got, "dev_beads") {
		t.Errorf("report should mention both databases: %s", got)
	}
	if !contains(report, "JUMP") {
		t.Errorf("report should mention JUMP for increase: %s", report)
	}
	if !contains(report, "DROP") {
		t.Errorf("report should mention DROP for decrease: %s", report)
	}
	// Both counts' sources must be named, so a reviewer can tell where the
	// previous level came from without re-deriving the baseline (gt-tj-he).
	for _, want := range []string{
		"previous 100 from prod_beads: 100 (HEAD), 90 (HEAD~1)",
		"current  150 from prod_beads/issues.jsonl",
		"previous 200 from dev_beads: 200 (cached)",
		"prod_beads/issues.jsonl",
	} {
		if !contains(report, want) {
			t.Errorf("report missing %q: %s", want, report)
		}
	}
}

func TestVerifyExportCounts_NoBaselineFailsLoud(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// Commits exist but none carry per-database counts, and there is no cache —
	// history this detector cannot read. verifyExportCounts must fail loud
	// instead of silently treating the export as a first export (gt-tj-he).
	// (A repo with NO commits at all is a different case: see
	// TestVerifyExportCounts_FreshRepoBootstraps.)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)

	_, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 100}, 0.20)
	if err == nil {
		t.Fatal("expected error when no spike baseline can be computed, got nil")
	}
	if !errors.Is(err, errNoSpikeBaseline) {
		t.Errorf("expected errNoSpikeBaseline, got %v", err)
	}
}

func TestRecomputeSpikeBaseline_FreshRepoBootstraps(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// A freshly initialized backup repo has no commits at all (what
	// ensureGitRepoInitialized leaves behind: `git init`, no seed commit).
	// There is no level to compare against and no spike is possible without
	// one, so this is a bootstrap — not errNoSpikeBaseline, which would halt
	// the first run and thereby prevent the very commit that seeds history
	// (gt-tj-he).
	gitRepo := t.TempDir()
	initEmptyGitRepo(t, d, gitRepo)

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if !errors.Is(err, errSpikeBaselineBootstrap) {
		t.Fatalf("expected errSpikeBaselineBootstrap, got sb=%+v err=%v", sb, err)
	}
}

func TestVerifyExportCounts_FreshRepoBootstraps(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// First run on a fresh town must not be blocked: with no baseline, halting
	// leaves the patrol permanently inert (no baseline → no commit → never a
	// baseline). The run proceeds and its commit seeds the next run's baseline.
	gitRepo := t.TempDir()
	initEmptyGitRepo(t, d, gitRepo)

	spikes, err := d.verifyExportCounts(gitRepo, []string{"hq"}, map[string]int{"hq": 1271}, 0.50)
	if err != nil {
		t.Fatalf("fresh repo must bootstrap, got error: %v", err)
	}
	if len(spikes) != 0 {
		t.Fatalf("expected no spikes on the first run, got %v", spikes)
	}

	// The run commits (that is what bootstrapping unblocks), and the next run
	// derives its baseline from that commit.
	os.MkdirAll(filepath.Join(gitRepo, "hq"), 0755)
	commitBackup(t, d, gitRepo, "hq", 1271)

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n, ok := spikeBaselineCount(sb, "hq"); !ok || n != 1271 {
		t.Errorf("baseline after the bootstrap commit = %d (ok=%v), want 1271", n, ok)
	}
}

func TestRecomputeSpikeBaseline_CommitHistoryOutranksWarmCache(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// Steady state: recompute rewrites the cache every tick, so it is warm on
	// essentially every run while history carries the newest commit. A cached
	// level must never shadow a committed one — an earlier revision seeded the
	// cache at age 0 and appended the commit entry beside it, so the stale
	// cached level won the sort and the baseline sat one cycle behind forever
	// (gt-tj-he).
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "hq"), 0755)
	commitBackup(t, d, gitRepo, "hq", 1266)
	commitBackup(t, d, gitRepo, "hq", 1271)

	// Cache as written by the previous run, i.e. before the 1271 commit landed.
	stale := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: time.Now().Format(time.RFC3339),
		Counts: map[string][]spikeCommit{
			"hq": {{Db: "hq", N: 1266, Age: 0, Source: spikeSourceCommit}},
		},
	}
	if err := saveSpikeBaselineHistory(gitRepo, stale); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n, ok := spikeBaselineCount(sb, "hq"); !ok || n != 1271 {
		t.Fatalf("baseline = %d (ok=%v), want the committed 1271, not the cached 1266", n, ok)
	}
	entries := sb.Counts["hq"]
	if len(entries) != 2 {
		t.Fatalf("expected both commits in the window, got %d entries: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Source == spikeSourceCache {
			t.Errorf("a readable commit history must contribute no cached entries, got %+v", e)
		}
	}
}

func TestVerifyExportCounts_FirstExport(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// History exists for db1, but db2 is being exported for the first time:
	// it is absent from the baseline window and must be skipped, not spiked.
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "db1"), 0755)
	commitBackup(t, d, gitRepo, "db1", 100)

	spikes, err := d.verifyExportCounts(gitRepo, []string{"db2"}, map[string]int{"db2": 100}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("expected no spikes on first export of a new db, got %v", spikes)
	}
}

func TestVerifyExportCounts_WithinThreshold(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 100)

	// 130 records = 30% increase. With 0.20 threshold and 2x asymmetric
	// multiplier for increases, effective threshold is 0.40, so 30% is fine.
	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 130}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("expected no spikes for 30%% increase (effective threshold 40%%), got %v", spikes)
	}
}

func TestVerifyExportCounts_ExceedsThreshold(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 100)

	// 200 records = 100% jump. Even with 2x asymmetric multiplier (effective
	// threshold 0.40), 100% exceeds it.
	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 200}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Fatalf("expected 1 spike, got %d", len(spikes))
	}
	if spikes[0].DB != "testdb" {
		t.Errorf("expected spike for testdb, got %s", spikes[0].DB)
	}
	if spikes[0].Previous != 100 || spikes[0].Current != 200 {
		t.Errorf("expected 100→200, got %d→%d", spikes[0].Previous, spikes[0].Current)
	}
}

func TestVerifyExportCounts_Drop(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 100)

	// 60 records = 40% drop. Drops use the base threshold (no 2x multiplier)
	// because losing data is more suspicious than gaining it.
	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 60}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Fatalf("expected 1 spike for drop, got %d", len(spikes))
	}
	if spikes[0].Delta < 0.3 {
		t.Errorf("expected delta > 0.3, got %f", spikes[0].Delta)
	}
}

func TestVerifyExportCounts_SmallAbsoluteChangeIgnored(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 10)

	// 5 records = 50% drop, but only 5 records absolute change.
	// Below minAbsoluteDelta (20), so should NOT spike.
	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("expected no spikes for small absolute change (<20 records), got %v", spikes)
	}
}

func TestVerifyExportCounts_AsymmetricThreshold(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 100)

	// 70 records = 30% drop at 0.20 threshold → should spike (drops use base threshold)
	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 70}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Fatalf("expected 1 spike for 30%% drop at 20%% threshold, got %d", len(spikes))
	}

	// 130 records = 30% increase at 0.20 threshold → should NOT spike (increases use 2x = 0.40)
	spikes, err = d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 130}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("expected no spike for 30%% increase at 40%% effective threshold, got %v", spikes)
	}
}

func TestVerifyExportCounts_StaleBaselineRecovery(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// The gt-tj-he scenario: a spike halt blocked the commit that would have
	// refreshed the baseline, so the repo's newest committed level is stale
	// (1000) while the export already holds 400. The baseline window must be
	// re-derived, and the halted level must survive via the cache so the next
	// run does not re-halt forever against the stale value.
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 1000)

	// First run after the halt: 400 vs committed 1000 = 60% drop → spikes.
	counts := map[string]int{"testdb": 400}
	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, counts, 0.50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Fatalf("expected 1 spike on first detection, got %d", len(spikes))
	}

	// The recompute persisted the committed window to the in-repo cache; the
	// export is NOT committed (halt), so history still says 1000.
	cache := loadSpikeBaselineHistory(gitRepo)
	if cache == nil || len(cache.Counts["testdb"]) == 0 {
		t.Fatal("expected rolling baseline cache to be written")
	}
	if n, ok := spikeBaselineCount(cache, "testdb"); !ok || n != 1000 {
		t.Errorf("expected cached window head 1000, got %d (ok=%v)", n, ok)
	}

	// Commit the new level — the next backup commit records db=400.
	commitBackup(t, d, gitRepo, "testdb", 400)

	// Second run: same count (400), with the cache left exactly as the first
	// run wrote it (that is the steady state — recompute rewrites the cache
	// every tick, so it is warm on the next run). The baseline is re-derived
	// from history and now tracks the committed 400 → stable, no spike.
	// Without the re-derivation this re-halts against the stale 1000 forever.
	spikes, err = d.verifyExportCounts(gitRepo, []string{"testdb"}, counts, 0.50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Fatalf("expected no spikes after the new level is committed, got %v", spikes)
	}
	// Pin the level that made it stable: the committed 400, not the stale
	// cached 1000 the halted run wrote.
	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n, ok := spikeBaselineCount(sb, "testdb"); !ok || n != 400 {
		t.Errorf("baseline = %d (ok=%v), want the committed 400", n, ok)
	}
}

func TestVerifyExportCounts_HaltWithoutCommitRebaselinesFromCache(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// Follow-up run while the halt is still in place: the newest committed
	// level is still 1000 and the cache was just refreshed with the same
	// committed window, so the detector still sees 400 vs 1000 and must
	// spike again — a genuine unresolved halt keeps alerting (it is
	// de-duplicated by the alert mechanism, not by a stale baseline).
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 1000)

	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 400}, 0.50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Fatalf("expected 1 spike, got %d", len(spikes))
	}

	// Still halted: no new commit, cache carries the same window.
	spikes, err = d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 400}, 0.50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Errorf("expected 1 spike while the halt is unresolved, got %d", len(spikes))
	}
}

func TestRecomputeSpikeBaseline_RollingWindow(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "db1"), 0755)
	for _, n := range []int{100, 110, 120, 130, 140, 150, 160, 170, 180} {
		commitBackup(t, d, gitRepo, "db1", n)
	}

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sb.Window; got != defaultSpikeBaselineWindow {
		t.Errorf("window = %d, want %d", got, defaultSpikeBaselineWindow)
	}
	entries := sb.Counts["db1"]
	if len(entries) != defaultSpikeBaselineWindow {
		t.Fatalf("expected %d window entries, got %d", defaultSpikeBaselineWindow, len(entries))
	}
	// Newest first: 180 (most recent commit) through 110 (oldest in window).
	want := []int{180, 170, 160, 150, 140, 130, 120, 110}
	for i, w := range want {
		if entries[i].N != w {
			t.Errorf("entry %d = %d, want %d", i, entries[i].N, w)
		}
	}
}

func TestRecomputeSpikeBaseline_CacheSeedsAcrossHalt(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	// History exists but carries no counts (a repo whose count-bearing commits
	// were reset), while a cache written by an earlier run holds the last known
	// levels. The cache is the only remaining source, so it seeds the window
	// instead of leaving the patrol halted with nothing to compare against
	// (gt-tj-he).
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)

	cache := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: time.Now().Format(time.RFC3339),
		Counts: map[string][]spikeCommit{
			"db1": {{Db: "db1", N: 1271, Age: 0}},
		},
	}
	if err := saveSpikeBaselineHistory(gitRepo, cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, ok := spikeBaselineCount(sb, "db1")
	if !ok || n != 1271 {
		t.Errorf("expected cached level 1271, got %d (ok=%v)", n, ok)
	}
}

func TestCachedSpikeBaseline_RelabelsDedupesAndTrims(t *testing.T) {
	t.Parallel()
	// The cache is untrusted input: a truncated or hand-edited file must not be
	// able to make "the newest level" depend on sort internals. Entries come
	// back re-labeled as cache-sourced, re-based so the newest is age 0,
	// de-duplicated by age, and trimmed to the window depth (gt-tj-he).
	dir := t.TempDir()
	cache := &spikeBaseline{
		Window:   3,
		Computed: "2026-01-01T00:00:00Z",
		Counts: map[string][]spikeCommit{
			"db1": {
				{Db: "db1", N: 1000, Age: 5},
				{Db: "db1", N: 900, Age: 6},
				{Db: "db1", N: 1, Age: 6}, // duplicate age: dropped, first wins
				{Db: "db1", N: 800, Age: 7},
				{Db: "db1", N: 700, Age: 8}, // past the window depth: trimmed
			},
		},
	}
	if err := saveSpikeBaselineHistory(dir, cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	sb := cachedSpikeBaseline(dir)
	if sb == nil {
		t.Fatal("expected a window built from the cache")
	}
	entries := sb.Counts["db1"]
	want := []spikeCommit{
		{Db: "db1", N: 1000, Age: 0, Source: spikeSourceCache},
		{Db: "db1", N: 900, Age: 1, Source: spikeSourceCache},
		{Db: "db1", N: 800, Age: 2, Source: spikeSourceCache},
	}
	if len(entries) != len(want) {
		t.Fatalf("expected %d entries, got %d: %+v", len(want), len(entries), entries)
	}
	for i, w := range want {
		if entries[i].N != w.N || entries[i].Age != w.Age || entries[i].Source != w.Source {
			t.Errorf("entry %d = %+v, want N=%d Age=%d Source=%s", i, entries[i], w.N, w.Age, w.Source)
		}
	}
	if n, ok := spikeBaselineCount(sb, "db1"); !ok || n != 1000 {
		t.Errorf("baseline head = %d (ok=%v), want 1000", n, ok)
	}

	if sb := cachedSpikeBaseline(t.TempDir()); sb != nil {
		t.Errorf("expected nil for a repo with no cache, got %+v", sb)
	}
}

func TestRecomputeSpikeBaseline_NoneAvailable(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo) // only the non-count "init" commit

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if !errors.Is(err, errNoSpikeBaseline) {
		t.Fatalf("expected errNoSpikeBaseline, got sb=%+v err=%v", sb, err)
	}
}

func TestParseCommitCounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject string
		want    map[string]int
	}{
		{"backup 2026-01-02 09:00: hq=1271 beads=42", map[string]int{"hq": 1271, "beads": 42}},
		{"backup 2026-01-02 09:00: hq=1271 [FAILED: beads]", map[string]int{"hq": 1271}},
		{"init", nil},
		{"", nil},
		// A name failing validDBName (a dot), a non-numeric value, a bare "="
		// field, and "FAILED:" (no '=') are all rejected.
		{"backup 2026-01-02 09:00: bad.name=5 x=notanum =3 [FAILED: y]", nil},
	}
	for _, tt := range tests {
		got := parseCommitCounts(tt.subject)
		if len(got) != len(tt.want) {
			t.Errorf("parseCommitCounts(%q) = %v, want %v", tt.subject, got, tt.want)
			continue
		}
		for k, v := range tt.want {
			if got[k] != v {
				t.Errorf("parseCommitCounts(%q)[%q] = %d, want %d", tt.subject, k, got[k], v)
			}
		}
	}
}

func TestSpikeBaselineHistorySaveLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// No cache file → nil.
	if sb := loadSpikeBaselineHistory(dir); sb != nil {
		t.Errorf("expected nil, got %+v", sb)
	}

	// Save and load.
	sb := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: "2026-01-01T00:00:00Z",
		Counts: map[string][]spikeCommit{
			"db1": {{Db: "db1", N: 100, Age: 0}},
			"db2": {{Db: "db2", N: 200, Age: 0}},
		},
	}
	if err := saveSpikeBaselineHistory(dir, sb); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	got := loadSpikeBaselineHistory(dir)
	if got == nil {
		t.Fatal("expected non-nil after save")
	}
	if n, ok := spikeBaselineCount(got, "db1"); !ok || n != 100 {
		t.Errorf("db1: got %d (ok=%v), want 100", n, ok)
	}
	if n, ok := spikeBaselineCount(got, "db2"); !ok || n != 200 {
		t.Errorf("db2: got %d (ok=%v), want 200", n, ok)
	}

	// The cache file must be git-ignored so it never lands in the backup repo.
	if git, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); !strings.Contains(string(git), spikeBaselineCacheFile) {
		t.Errorf("expected .gitignore to mention %s, got %q", spikeBaselineCacheFile, git)
	}
}

func TestSpikeHistoryDetail(t *testing.T) {
	t.Parallel()
	sb := &spikeBaseline{
		Window: 3,
		Counts: map[string][]spikeCommit{
			"db1": {
				{Db: "db1", N: 1271, Age: 0},
				{Db: "db1", N: 1266, Age: 1},
				{Db: "db1", N: 1250, Age: 2},
			},
		},
	}
	got := spikeHistoryDetail(sb, "db1")
	want := "db1: 1271 (HEAD), 1266 (HEAD~1), 1250 (HEAD~2)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if s := spikeHistoryDetail(nil, "db1"); s != "" {
		t.Errorf("nil baseline should render empty, got %q", s)
	}

	// A cache-sourced entry carries no position in the current commit graph, so
	// it must not be labeled with a HEAD offset (gt-tj-he).
	cached := &spikeBaseline{
		Window: 3,
		Counts: map[string][]spikeCommit{
			"db1": {
				{Db: "db1", N: 1271, Age: 0, Source: spikeSourceCache},
				{Db: "db1", N: 1266, Age: 1, Source: spikeSourceCache},
			},
		},
	}
	if got, want := spikeHistoryDetail(cached, "db1"), "db1: 1271 (cached), 1266 (cached)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCountFileLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	writeNLines(t, path, 42)

	got, err := countFileLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Errorf("expected 42 lines, got %d", got)
	}
}

func TestCountFileLines_Empty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(path, []byte(""), 0644)

	got, err := countFileLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 lines, got %d", got)
	}
}

func TestParseLineCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected int
		wantErr  bool
	}{
		{"42", 42, false},
		{"  42 filename.jsonl", 42, false},
		{"  0", 0, false},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		got, err := parseLineCount(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseLineCount(%q): error = %v, wantErr %v", tt.input, err, tt.wantErr)
		}
		if got != tt.expected {
			t.Errorf("parseLineCount(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

// --- helpers ---

func splitNonEmpty(s string) []string {
	var result []string
	for _, line := range splitLines(s) {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// backupTestDaemon returns a daemon whose git is a fresh gitfake world, and
// the world.
func backupTestDaemon(t *testing.T) (*Daemon, *gitfake.Fake) {
	t.Helper()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	return d, useGitfake(t, d)
}

func initGitRepo(t *testing.T, d *Daemon, dir string) {
	t.Helper()
	initEmptyGitRepo(t, d, dir)
	// Need at least one commit for HEAD to exist.
	readme := filepath.Join(dir, "README")
	if err := os.WriteFile(readme, []byte("init\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, d, dir, "init")
}

// initEmptyGitRepo initializes a git repo with no commits — what
// ensureGitRepoInitialized leaves behind on a fresh town. recomputeSpikeBaseline
// treats that state as a bootstrap, so tests must be able to produce it
// (gt-tj-he).
func initEmptyGitRepo(t *testing.T, d *Daemon, dir string) {
	t.Helper()
	if err := d.backupGitAt(dir, 0).InitRepo("main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
}

func commitAll(t *testing.T, d *Daemon, dir, msg string) {
	t.Helper()
	g := d.backupGitAt(dir, 0)
	if err := g.Add("-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := g.CommitWithAuthor(msg, "Test <test@test.com>"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
}

// backupSubjects lists the backup repository's commit subjects, newest first.
func backupSubjects(t *testing.T, d *Daemon, dir string) []string {
	t.Helper()
	entries, err := d.backupGitAt(dir, 0).LogAll(64)
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	subjects := make([]string, len(entries))
	for i, e := range entries {
		subjects[i] = e.Subject
	}
	return subjects
}

func writeNLines(t *testing.T, path string, n int) {
	t.Helper()
	var buf []byte
	for i := 0; i < n; i++ {
		line, _ := json.Marshal(map[string]interface{}{"id": "rec-" + itoa(i), "title": "Record " + itoa(i)})
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// commitBackup writes dbDir/issues.jsonl with n lines and commits it with a
// backup-style subject line ("backup <ts>: <db>=<n>"), matching
// commitAndPushJsonlBackup — the rolling spike baseline derives its window from
// these subject lines (gt-tj-he).
func commitBackup(t *testing.T, d *Daemon, gitRepo, db string, n int) {
	t.Helper()
	writeNLines(t, filepath.Join(gitRepo, db, "issues.jsonl"), n)
	commitAll(t, d, gitRepo, fmt.Sprintf("backup 2026-01-01 00:00: %s=%d", db, n))
}

func TestEscalationTitle_SingleLineUnchanged(t *testing.T) {
	t.Parallel()
	got := escalationTitle("jsonl_git_backup", "git push failed 3 consecutive times")
	want := "jsonl_git_backup: git push failed 3 consecutive times"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscalationTitle_MultilineCollapsedToFirstLine(t *testing.T) {
	t.Parallel()
	// This is the gt-qna failure mode: the daemon hands escalate() the full
	// go-test failure output (many lines). The title must never contain a
	// newline — bd 1.0.3+ rejects newline-containing flag values, which was
	// silently dropping every one of these escalations.
	message := "main branch test failures:\ngastown: gate \"test\": exit status 1\n--- FAIL: TestFoo (0.01s)\n    foo_test.go:42: assertion failed"
	got := escalationTitle("main_branch_test", message)
	if strings.Contains(got, "\n") {
		t.Fatalf("title must not contain a newline, got %q", got)
	}
	want := "main_branch_test: main branch test failures:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEscalationTitle_CarriageReturnEndsTheLine: notify.Escalation.Validate
// refuses a description containing "\r" as well as "\n", so a title cut only
// at "\n" let a message with a bare carriage return (a progress line from git
// or go test) through to a refusal, and the escalation was never filed.
func TestEscalationTitle_CarriageReturnEndsTheLine(t *testing.T) {
	t.Parallel()
	for name, message := range map[string]string{
		"bare CR":      "push failed\rretrying 2/3",
		"CRLF":         "push failed\r\nretrying 2/3",
		"CR before LF": "push failed\rretrying\nmore",
		"LF before CR": "push failed\nretrying\rmore",
	} {
		got := escalationTitle("jsonl_git_backup", message)
		if want := "jsonl_git_backup: push failed"; got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
		if err := (notify.Escalation{Description: got}).Validate(); err != nil {
			t.Errorf("%s: title %q is not a valid escalation description: %v", name, got, err)
		}
	}
}

func TestEscalationTitle_TruncatedToMaxLen(t *testing.T) {
	t.Parallel()
	longLine := strings.Repeat("x", maxEscalationTitleLen*2)
	got := escalationTitle("source", longLine)
	if n := len([]rune(got)); n > maxEscalationTitleLen {
		t.Fatalf("title length %d exceeds max %d", n, maxEscalationTitleLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated title should end with ellipsis, got %q", got)
	}
}

// TestDefaultEscalationTimeout verifies the per-attempt timeout is long enough
// to survive slot-starvation (the condition that produced gt-tlwv drops).
func TestDefaultEscalationTimeout(t *testing.T) {
	t.Parallel()
	if defaultEscalationTimeout != 60*time.Second {
		t.Errorf("defaultEscalationTimeout = %v, want 60s", defaultEscalationTimeout)
	}
}

// TestMaxEscalationRetries verifies the retry budget.
func TestMaxEscalationRetries(t *testing.T) {
	t.Parallel()
	if maxEscalationRetries != 3 {
		t.Errorf("maxEscalationRetries = %d, want 3", maxEscalationRetries)
	}
}

// TestEscalate_RetriesOnTimeout verifies that escalate retries when gt
// escalate fails, and eventually succeeds on a later attempt. We verify the
// retry logic by using a fake gt that succeeds on the 3rd call — calls 1-2
// exit 1 immediately, confirming the function retries transient failures
// instead of giving up after one attempt.
func TestEscalate_RetriesOnTimeout(t *testing.T) {
	t.Parallel()
	flaky := &flakyEscalations{Recorder: notifyfake.New(), failures: 2}
	clk := newFixedClock()
	d := &Daemon{
		logger:   log.New(io.Discard, "", 0),
		config:   &Config{TownRoot: t.TempDir()},
		notifier: flaky,
		clock:    clk,
	}

	var err error
	runOnClock(t, clk, time.Second, func() { err = d.escalateAlertErr("k", "main_branch_test", "test failed") })
	if err != nil {
		t.Fatalf("escalateAlertErr = %v, want success on the third attempt", err)
	}
	// The retries back off 1 s, then 2 s, on the daemon's clock.
	if waited := clk.Since(testEpoch); waited != 3*time.Second {
		t.Errorf("backoff before the third attempt = %s, want 3s (1s then 2s)", waited)
	}
	if flaky.attempts != 3 {
		t.Errorf("escalation attempts = %d, want 3", flaky.attempts)
	}
	if got := flaky.Escalations(); len(got) != 1 {
		t.Errorf("delivered escalations = %d, want 1", len(got))
	}
}

// flakyEscalations fails the first failures escalations, then records the
// rest.
type flakyEscalations struct {
	*notifyfake.Recorder
	failures int
	attempts int
}

func (f *flakyEscalations) Escalate(ctx context.Context, e notify.Escalation) error {
	f.attempts++
	if f.failures > 0 {
		f.failures--
		return errors.New("gt escalate: exit status 1 (<no output>)")
	}
	return f.Recorder.Escalate(ctx, e)
}

// TestEscalate_FallsBackToFeedOnPermanentFailure verifies that when gt
// escalate always fails, the full message is logged to the feed (not just
// the title).
func TestEscalate_FallsBackToFeedOnPermanentFailure(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	eventsFile := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(filepath.Join(townRoot, "daemon"), nil, 0o755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}

	// Every escalation fails, as gt does when bd cannot reach its database.
	rec := notifyfake.New()
	rec.Fail(notifyfake.KindEscalate, errors.New("gt escalate: exit status 1 (bd: database not found)"))

	logger := log.New(io.Discard, "", 0)
	clk := newFixedClock()
	d := &Daemon{
		logger: logger,
		config: &Config{
			TownRoot: townRoot,
		},
		notifier: rec,
		clock:    clk,
	}

	testMessage := "main branch test failures:\ngastown: gate \"test\": exit status 1"
	runOnClock(t, clk, time.Second, func() { d.escalate("main_branch_test", testMessage) })

	// Verify the events file received the escalation_dropped event with the
	// full message (not just the title).
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected events output, got empty file")
	}

	// The file has one JSON object per line; find the escalation_dropped event.
	// Event struct: {"ts":"...","source":"daemon","type":"escalation_dropped","actor":"daemon","payload":{...}}
	var found bool
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		var record map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			continue
		}
		if record["type"] != "escalation_dropped" {
			continue
		}
		found = true

		// The message is inside the payload map.
		payload, ok := record["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected 'payload' field in event record, got keys: %v", keys(record))
		}
		msg, ok := payload["message"].(string)
		if !ok {
			t.Fatalf("expected 'message' field in payload, got keys: %v", keys(payload))
		}
		if msg != testMessage {
			t.Errorf("event message = %q, want %q", msg, testMessage)
		}

		// The title should be truncated to the first line.
		title, ok := payload["title"].(string)
		if !ok {
			t.Fatalf("expected 'title' field in payload")
		}
		if strings.Contains(title, "\n") {
			t.Errorf("title should not contain newline: %q", title)
		}
		if title != "main_branch_test: main branch test failures:" {
			t.Errorf("title = %q, want 'main_branch_test: main branch test failures:'", title)
		}
		break
	}
	if !found {
		t.Fatal("did not find escalation_dropped event in events file")
	}
}

// TestEscalate_TimedOutAttemptsAreLoggedAsTimeouts: notify.CLI reports a
// killed gt escalate as an error wrapping context.DeadlineExceeded, with gt's
// output after it. escalateAlertErr must recognise the wrapped deadline, log
// each attempt as a timeout (gt-tlwv), and record the final drop in the feed
// as "context deadline exceeded" rather than as an ordinary failure.
func TestEscalate_TimedOutAttemptsAreLoggedAsTimeouts(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rec := notifyfake.New()
	rec.Fail(notifyfake.KindEscalate, fmt.Errorf("gt escalate: %w (signal: killed)", context.DeadlineExceeded))
	var logs bytes.Buffer
	clk := newFixedClock()
	d := &Daemon{
		logger:   log.New(&logs, "", 0),
		config:   &Config{TownRoot: townRoot},
		notifier: rec,
		clock:    clk,
	}

	var err error
	runOnClock(t, clk, time.Second, func() { err = d.escalateAlertErr("k", "main_branch_test", "test failed") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("escalateAlertErr = %v, want the wrapped deadline", err)
	}
	for attempt := 1; attempt <= maxEscalationRetries; attempt++ {
		want := fmt.Sprintf("escalate(main_branch_test): attempt %d/%d timed out after %s", attempt, maxEscalationRetries, defaultEscalationTimeout)
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "failed:") {
		t.Errorf("a timed-out attempt was logged as an ordinary failure:\n%s", logs.String())
	}

	data, err := os.ReadFile(filepath.Join(townRoot, ".events.jsonl"))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var dropped []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("event line %q: %v", line, err)
		}
		if record["type"] == events.TypeEscalationDropped {
			payload, _ := record["payload"].(map[string]any)
			dropped = append(dropped, payload)
		}
	}
	if len(dropped) != 1 {
		t.Fatalf("escalation_dropped events = %d, want 1 (the final drop):\n%s", len(dropped), data)
	}
	if got := dropped[0]["error"]; got != "context deadline exceeded" {
		t.Errorf("dropped error = %q, want %q", got, "context deadline exceeded")
	}
	if got := dropped[0]["message"]; got != "test failed" {
		t.Errorf("dropped message = %q, want %q", got, "test failed")
	}
}

// keys returns the map keys for debugging.
func keys(m map[string]interface{}) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// ageIndexLock writes an index.lock in gitRepo aged by age, standing in for a
// lock a killed git process orphaned, and returns its path.
func ageIndexLock(t *testing.T, gitRepo string, age time.Duration) string {
	t.Helper()
	lockPath := gitIndexLockPath(gitRepo)
	if err := os.WriteFile(lockPath, nil, 0644); err != nil {
		t.Fatalf("writing %s: %v", lockPath, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(lockPath, when, when); err != nil {
		t.Fatalf("aging %s: %v", lockPath, err)
	}
	return lockPath
}

func TestIsIndexLockExistsError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{"git fatal", "fatal: Unable to create '/repo/.git/index.lock': File exists.", true},
		{"git alternative phrasing", "fatal: Unable to create '/repo/.git/index.lock': File exists.\n\nAnother git process seems to be running in this repository", true},
		{"missing file", "fatal: pathspec 'x' did not match any files", false},
		{"index lock named but unrelated", "error: index.lock is not a valid object name", false},
		{"push auth failure", "fatal: Authentication failed for 'https://github.com/x/y'", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIndexLockExistsError(errors.New(tt.errMsg)); got != tt.want {
				t.Errorf("isIndexLockExistsError(%q) = %v, want %v", tt.errMsg, got, tt.want)
			}
		})
	}
	if isIndexLockExistsError(nil) {
		t.Error("isIndexLockExistsError(nil) = true, want false")
	}
}

// lockTestRepo returns a directory laid out like the backup repository
// (a .git directory to hold index.lock); nothing runs git in it.
func lockTestRepo(t *testing.T) string {
	t.Helper()
	gitRepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gitRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return gitRepo
}

func TestClearStaleIndexLock_RespectsGrace(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	t.Run("stale is removed", func(t *testing.T) {
		gitRepo := lockTestRepo(t)
		lockPath := ageIndexLock(t, gitRepo, 2*gitIndexLockGracePeriod)
		if !d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			t.Fatal("expected a lock older than the grace period to be cleared")
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Errorf("lock still present, stat err = %v", err)
		}
	})

	t.Run("fresh is left for its owner", func(t *testing.T) {
		gitRepo := lockTestRepo(t)
		lockPath := ageIndexLock(t, gitRepo, 0)
		if d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			t.Fatal("a lock younger than the grace period must not be cleared")
		}
		if _, err := os.Stat(lockPath); err != nil {
			t.Errorf("fresh lock should still exist: %v", err)
		}
	})

	t.Run("absent is a no-op", func(t *testing.T) {
		gitRepo := lockTestRepo(t)
		if d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			t.Error("no lock on disk should report nothing cleared")
		}
	})
}

func TestClearIndexLockModifiedSince_OnlyRecent(t *testing.T) {
	t.Parallel()
	// A lock written after a command started is that command's own orphan.
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	gitRepo := lockTestRepo(t)
	started := time.Now().Add(-time.Second)
	lockPath := ageIndexLock(t, gitRepo, 0)
	if !d.clearIndexLockModifiedSince(gitRepo, started) {
		t.Fatal("expected a lock written after the command started to be cleared")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("lock still present, stat err = %v", err)
	}

	// A lock predating the command belongs to somebody else.
	gitRepo2 := lockTestRepo(t)
	lockPath2 := ageIndexLock(t, gitRepo2, time.Hour)
	if d.clearIndexLockModifiedSince(gitRepo2, time.Now()) {
		t.Fatal("a lock older than the command must not be cleared")
	}
	if _, err := os.Stat(lockPath2); err != nil {
		t.Errorf("pre-existing lock should still exist: %v", err)
	}
}

// indexLockErr is what git says when index.lock is held.
var indexLockErr = errors.New("git add: fatal: Unable to create '/repo/.git/index.lock': File exists.")

func TestRunGitIndexOp_RecoversFromStaleLock(t *testing.T) {
	t.Parallel()
	// A lock orphaned by a dead tick must not fail the next one (gt-1aj2).
	gitRepo := lockTestRepo(t)
	lockPath := ageIndexLock(t, gitRepo, 2*gitIndexLockGracePeriod)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	calls := 0
	err := d.runGitIndexOp(gitRepo, "git add", func() error {
		calls++
		if _, err := os.Stat(lockPath); err == nil {
			return indexLockErr
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("runGitIndexOp = %v after %d call(s); want the stale lock cleared before the one call", err, calls)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("stale lock should be gone, stat err = %v", err)
	}
}

func TestRunGitIndexOp_LeavesFreshLock(t *testing.T) {
	t.Parallel()
	// A lock with a live owner is not an orphan: failing is correct, and the
	// lock must survive so the real owner can finish. The lock may have
	// appeared just before the command, so it is retried once.
	gitRepo := lockTestRepo(t)
	lockPath := ageIndexLock(t, gitRepo, 0)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	calls := 0
	err := d.runGitIndexOp(gitRepo, "git add", func() error {
		calls++
		return indexLockErr
	})
	if !errors.Is(err, indexLockErr) || calls != 2 {
		t.Fatalf("runGitIndexOp = %v after %d call(s); want the lock error after one retry", err, calls)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("a live owner's lock must not be removed: %v", statErr)
	}
}

func TestRunGitIndexOp_ClearsTheLockATimedOutCommandLeft(t *testing.T) {
	t.Parallel()
	// gt-1aj2: a command killed on its deadline cannot release its own lock,
	// so the lock it wrote is cleared at once and the timeout returned.
	gitRepo := lockTestRepo(t)
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	calls := 0
	err := d.runGitIndexOp(gitRepo, "git add", func() error {
		calls++
		ageIndexLock(t, gitRepo, 0)
		return fmt.Errorf("git add: %w", gtgit.ErrTimedOut)
	})
	if !errors.Is(err, gtgit.ErrTimedOut) || calls != 1 {
		t.Fatalf("runGitIndexOp = %v after %d call(s); want the timeout, not retried", err, calls)
	}
	if _, statErr := os.Stat(gitIndexLockPath(gitRepo)); !os.IsNotExist(statErr) {
		t.Errorf("the timed-out command's lock should be cleared, stat err = %v", statErr)
	}
}

func TestRunGitIndexOp_OtherFailuresAreNotRetried(t *testing.T) {
	t.Parallel()
	gitRepo := lockTestRepo(t)
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	calls := 0
	failure := errors.New("git add: fatal: pathspec 'x' did not match any files")
	if err := d.runGitIndexOp(gitRepo, "git add", func() error { calls++; return failure }); !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("runGitIndexOp = %v after %d call(s); want the failure, not retried", err, calls)
	}
}

// commitAndPushJsonlBackup commits the exported files with the counts in the
// subject and pushes the branch to origin.
func TestCommitAndPushJsonlBackup_CommitsAndPushes(t *testing.T) {
	t.Parallel()
	d, f := backupTestDaemon(t)
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", "seed", map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)
	if err := os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(gitRepo, "testdb", "issues.jsonl"), 5)

	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb", "gone"}, map[string]int{"testdb": 5}, []string{"gone"}); err != nil {
		t.Fatalf("commitAndPushJsonlBackup: %v", err)
	}
	subject := backupSubjects(t, d, gitRepo)[0]
	if !strings.HasPrefix(subject, "backup ") || !strings.Contains(subject, ": testdb=5 [FAILED: gone]") {
		t.Errorf("commit subject = %q; want the counts and the failed database", subject)
	}
	head, _ := f.Open(gitRepo).Rev("HEAD")
	if tip := f.Ref(remote, "refs/heads/main"); tip != head {
		t.Errorf("origin main = %s, want the backup commit %s", tip, head)
	}

	// Nothing new to back up: no second commit.
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again, _ := f.Open(gitRepo).Rev("HEAD"); again != head {
		t.Errorf("an unchanged export committed again (%s)", again)
	}
}

// TestTriggerJsonlGitBackup_SkipsWhenNotDue is the regression test for
// gt-ima2/gt-gxpwc applied to jsonl_git_backup: with a recent last-run record
// on disk, the trigger must decline to start a cycle rather than firing on
// every tick (or every restart) regardless of the persisted schedule.
func TestTriggerJsonlGitBackup_SkipsWhenNotDue(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "jsonl_git_backup", time.Now()); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf strings.Builder
	d := &Daemon{
		logger: log.New(&buf, "", 0),
		config: &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true},
			},
		},
	}

	d.triggerJsonlGitBackup()
	if d.jsonlGitBackupRunning.Load() {
		t.Error("a declined trigger must not set the running guard")
	}
	if !contains(buf.String(), "not due") {
		t.Errorf("expected a not-due log line, got: %q", buf.String())
	}
}
