package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGitChildEnv_ForwardsExisting(t *testing.T) {
	t.Setenv("HOME", "/tmp/fake-home")
	t.Setenv("USER", "fakeuser")
	t.Setenv("LOGNAME", "fakeuser")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/fake-agent.sock")

	got := envMap(gitChildEnv())
	if got["HOME"] != "/tmp/fake-home" {
		t.Errorf("HOME: got %q, want /tmp/fake-home", got["HOME"])
	}
	if got["USER"] != "fakeuser" {
		t.Errorf("USER: got %q, want fakeuser", got["USER"])
	}
	if got["LOGNAME"] != "fakeuser" {
		t.Errorf("LOGNAME: got %q, want fakeuser", got["LOGNAME"])
	}
	if got["SSH_AUTH_SOCK"] != "/tmp/fake-agent.sock" {
		t.Errorf("SSH_AUTH_SOCK: got %q, want /tmp/fake-agent.sock", got["SSH_AUTH_SOCK"])
	}
}

func TestGitChildEnv_RecoversMissingIdentity(t *testing.T) {
	// Simulate a daemon child that lost USER/LOGNAME (the gh#zt1w failure mode).
	// HOME stays set so user.Current() succeeds without needing getpwuid.
	t.Setenv("HOME", "/tmp/fake-home")
	os.Unsetenv("USER")
	os.Unsetenv("LOGNAME")

	got := envMap(gitChildEnv())
	if got["USER"] == "" {
		t.Error("USER should have been recovered, got empty")
	}
	if got["LOGNAME"] == "" {
		t.Error("LOGNAME should have been recovered, got empty")
	}
	if got["USER"] != got["LOGNAME"] {
		t.Errorf("USER (%q) should match LOGNAME (%q)", got["USER"], got["LOGNAME"])
	}
}

func TestEnsureGitRepoInitialized_CreatesMissingRepo(t *testing.T) {
	gitRepo := filepath.Join(t.TempDir(), "nested", "git")

	if _, err := os.Stat(gitRepo); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to exist yet", gitRepo)
	}

	if err := ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized: %v", err)
	}

	if _, err := os.Stat(filepath.Join(gitRepo, ".git")); err != nil {
		t.Fatalf("expected %s/.git to exist after init: %v", gitRepo, err)
	}
}

func TestEnsureGitRepoInitialized_NoOpOnExistingRepo(t *testing.T) {
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)

	head, err := exec.Command("git", "-C", gitRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	if err := ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized on existing repo: %v", err)
	}

	headAfter, err := exec.Command("git", "-C", gitRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if string(head) != string(headAfter) {
		t.Errorf("existing repo history changed: before %q, after %q", head, headAfter)
	}
}

func TestEnsureGitRepoInitialized_SetsPostBufferOnFreshRepo(t *testing.T) {
	gitRepo := t.TempDir()

	if err := ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized: %v", err)
	}

	got := gitConfigGet(t, gitRepo, "http.postBuffer")
	if got != gitPostBufferBytes {
		t.Errorf("http.postBuffer = %q, want %q", got, gitPostBufferBytes)
	}
}

func TestEnsureGitRepoInitialized_SetsPostBufferOnExistingRepo(t *testing.T) {
	// Regression for gt-kxa1: a repo initialized before this fix (or with the
	// config lost some other way) must also get covered, not just fresh inits.
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)

	if err := ensureGitRepoInitialized(gitRepo); err != nil {
		t.Fatalf("ensureGitRepoInitialized on existing repo: %v", err)
	}

	got := gitConfigGet(t, gitRepo, "http.postBuffer")
	if got != gitPostBufferBytes {
		t.Errorf("http.postBuffer = %q, want %q", got, gitPostBufferBytes)
	}
}

func gitConfigGet(t *testing.T, gitRepo, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", gitRepo, "config", "--get", key).Output()
	if err != nil {
		t.Fatalf("git config --get %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

func TestIsPostBufferPushError(t *testing.T) {
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
	hint := postBufferHint("2.9 MiB")
	if !contains(hint, "http.postBuffer") {
		t.Errorf("hint should name http.postBuffer, got: %s", hint)
	}
	if !contains(hint, "2.9 MiB") {
		t.Errorf("hint should include the pack size when known, got: %s", hint)
	}
}

func TestPostBufferHint_OkWithoutPackSize(t *testing.T) {
	hint := postBufferHint("")
	if !contains(hint, "http.postBuffer") {
		t.Errorf("hint should still name http.postBuffer with no pack size, got: %s", hint)
	}
}

func TestCommitAndPushJsonlBackup_NoRemoteIsError(t *testing.T) {
	// Regression for gt-kme: a JSONL backup repo with commits but no configured
	// remote used to be reported as a silent success ("skipping push"), even
	// though the data never left the machine. It must now be a hard error so
	// the daemon's consecutive-failure escalation fires.
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)

	dbDir := filepath.Join(gitRepo, "testdb")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 5)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil)
	if err == nil {
		t.Fatal("expected an error when no remote is configured, got nil")
	}
	if !contains(err.Error(), "remote") {
		t.Errorf("expected error to mention the missing remote, got: %v", err)
	}

	// The commit itself should still have happened locally.
	out, cerr := exec.Command("git", "-C", gitRepo, "log", "-1", "--format=%s").Output()
	if cerr != nil {
		t.Fatalf("git log: %v", cerr)
	}
	if !contains(string(out), "backup") {
		t.Errorf("expected a local backup commit despite the push failure, got log: %q", out)
	}
}

func TestDiscoverJsonlBackupDatabases(t *testing.T) {
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
	filtered, removed := filterTestPollution([]byte(""))
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
	if len(filtered) != 0 {
		t.Errorf("expected empty output, got %q", filtered)
	}
}

func TestSpikeThreshold(t *testing.T) {
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
	spikes := []spikeInfo{
		{DB: "prod_beads", File: "prod_beads/issues.jsonl", Previous: 100, Current: 150, Delta: 0.50},
		{DB: "dev_beads", File: "dev_beads/issues.jsonl", Previous: 200, Current: 50, Delta: 0.75},
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
}

func TestVerifyExportCounts_NoBaselineFailsLoud(t *testing.T) {
	// A brand-new repo: no backup commits, no cache. recomputeSpikeBaseline
	// finds nothing, so verifyExportCounts must fail loud instead of silently
	// treating the export as a first export (gt-tj-he).
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	_, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 100}, 0.20)
	if err == nil {
		t.Fatal("expected error when no spike baseline can be computed, got nil")
	}
	if !errors.Is(err, errNoSpikeBaseline) {
		t.Errorf("expected errNoSpikeBaseline, got %v", err)
	}
}

func TestVerifyExportCounts_FirstExport(t *testing.T) {
	// History exists for db1, but db2 is being exported for the first time:
	// it is absent from the baseline window and must be skipped, not spiked.
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "db1"), 0755)
	commitBackup(t, gitRepo, "db1", 100)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	spikes, err := d.verifyExportCounts(gitRepo, []string{"db2"}, map[string]int{"db2": 100}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("expected no spikes on first export of a new db, got %v", spikes)
	}
}

func TestVerifyExportCounts_WithinThreshold(t *testing.T) {
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 100)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 100)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 100)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 10)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 100)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	// The gt-tj-he scenario: a spike halt blocked the commit that would have
	// refreshed the baseline, so the repo's newest committed level is stale
	// (1000) while the export already holds 400. The baseline window must be
	// re-derived, and the halted level must survive via the cache so the next
	// run does not re-halt forever against the stale value.
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 1000)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	commitBackup(t, gitRepo, "testdb", 400)

	// Second run: same count (400). The baseline is re-derived from history
	// and now tracks the committed 400 → stable, no spike. Without the
	// re-derivation this would re-halt against the stale 1000 forever.
	// Drop the gitignored cache first: a fresh daemon process has none, so
	// the window is re-derived purely from committed history — the true
	// "baseline refreshed, no re-halt" guarantee. (A warm cache seeding the
	// same level is covered by TestRecomputeSpikeBaseline_CacheSeedsAcrossHalt.)
	if err := os.Remove(filepath.Join(gitRepo, spikeBaselineCacheFile)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove cache: %v", err)
	}
	spikes, err = d.verifyExportCounts(gitRepo, []string{"testdb"}, counts, 0.50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("expected no spikes after the new level is committed, got %v", spikes)
	}
}

func TestVerifyExportCounts_HaltWithoutCommitRebaselinesFromCache(t *testing.T) {
	// Follow-up run while the halt is still in place: the newest committed
	// level is still 1000 and the cache was just refreshed with the same
	// committed window, so the detector still sees 400 vs 1000 and must
	// spike again — a genuine unresolved halt keeps alerting (it is
	// de-duplicated by the alert mechanism, not by a stale baseline).
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, gitRepo, "testdb", 1000)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}

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
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "db1"), 0755)
	for _, n := range []int{100, 110, 120, 130, 140, 150, 160, 170, 180} {
		commitBackup(t, gitRepo, "db1", n)
	}

	sb, err := recomputeSpikeBaseline(gitRepo)
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
	// No committed history for the db, but a cache written by an earlier run
	// seeds the window — this is the one-cycle gap a spike halt leaves
	// (gt-tj-he).
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)

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

	sb, err := recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, ok := spikeBaselineCount(sb, "db1")
	if !ok || n != 1271 {
		t.Errorf("expected cached level 1271, got %d (ok=%v)", n, ok)
	}
}

func TestRecomputeSpikeBaseline_NoneAvailable(t *testing.T) {
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo) // only the non-count "init" commit

	sb, err := recomputeSpikeBaseline(gitRepo)
	if !errors.Is(err, errNoSpikeBaseline) {
		t.Fatalf("expected errNoSpikeBaseline, got sb=%+v err=%v", sb, err)
	}
}

func TestParseCommitCounts(t *testing.T) {
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

func TestSpikeBaselineHistorySaveLoadRemove(t *testing.T) {
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

	// Remove.
	removeSpikeBaselineHistory(dir)
	if sb := loadSpikeBaselineHistory(dir); sb != nil {
		t.Errorf("expected nil after remove, got %+v", sb)
	}
}

func TestSpikeHistoryDetail(t *testing.T) {
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
}

func TestCountFileLines(t *testing.T) {
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

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init failed: %v: %s", err, out)
		}
	}
	// Need at least one commit for HEAD to exist.
	readme := filepath.Join(dir, "README")
	os.WriteFile(readme, []byte("init\n"), 0644)
	commitAll(t, dir, "init")
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	cmds := [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", msg, "--author=Test <test@test.com>"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
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
func commitBackup(t *testing.T, gitRepo, db string, n int) {
	t.Helper()
	writeNLines(t, filepath.Join(gitRepo, db, "issues.jsonl"), n)
	commitAll(t, gitRepo, fmt.Sprintf("backup 2026-01-01 00:00: %s=%d", db, n))
}

func TestEscalationTitle_SingleLineUnchanged(t *testing.T) {
	got := escalationTitle("jsonl_git_backup", "git push failed 3 consecutive times")
	want := "jsonl_git_backup: git push failed 3 consecutive times"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscalationTitle_MultilineCollapsedToFirstLine(t *testing.T) {
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

func TestEscalationTitle_TruncatedToMaxLen(t *testing.T) {
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
	if defaultEscalationTimeout != 60*time.Second {
		t.Errorf("defaultEscalationTimeout = %v, want 60s", defaultEscalationTimeout)
	}
}

// TestMaxEscalationRetries verifies the retry budget.
func TestMaxEscalationRetries(t *testing.T) {
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
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for gt")
	}

	townRoot := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	// Fake gt that succeeds on the 3rd call. On calls 1-2 it exits 1
	// immediately (simulating a transient failure that the retry should
	// overcome).
	gtScript := `#!/usr/bin/env bash
counter=` + counterFile + `
count=$(cat "$counter")
count=$((count + 1))
echo "$count" > "$counter"
if [ $count -lt 3 ]; then
	exit 1
fi
exit 0
`
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logger := log.New(os.Stderr, "TestEscalate_RetriesOnTimeout: ", log.LstdFlags)
	d := &Daemon{
		logger: logger,
		config: &Config{
			TownRoot: townRoot,
		},
	}

	done := make(chan struct{})
	go func() {
		d.escalate("main_branch_test", "test failed")
		close(done)
	}()

	select {
	case <-done:
		// Good — escalate completed after retries.
	case <-time.After(30 * time.Second):
		t.Fatal("escalate did not complete within 30 s")
	}

	// Verify the fake gt was called 3 times (3 attempts).
	count, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if strings.TrimSpace(string(count)) != "3" {
		t.Errorf("gt called %s times, want 3", strings.TrimSpace(string(count)))
	}
}

// TestEscalate_FallsBackToFeedOnPermanentFailure verifies that when gt
// escalate always fails, the full message is logged to the feed (not just
// the title).
func TestEscalate_FallsBackToFeedOnPermanentFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for gt")
	}

	townRoot := t.TempDir()
	eventsFile := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(filepath.Join(townRoot, "daemon"), nil, 0o755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}

	// Fake gt that always fails with stderr output.
	gtScript := `#!/usr/bin/env bash
echo "bd: database not found" >&2
exit 1
`
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logger := log.New(os.Stderr, "TestEscalate_Fallback: ", log.LstdFlags)
	d := &Daemon{
		logger: logger,
		config: &Config{
			TownRoot: townRoot,
		},
	}

	testMessage := "main branch test failures:\ngastown: gate \"test\": exit status 1"
	d.escalate("main_branch_test", testMessage)

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

func TestRunGitCmd_TimeoutIsDistinguishable(t *testing.T) {
	// gt-1aj2: callers clear an orphaned index lock when a command is killed on
	// its deadline, so a timeout must be tellable apart from a normal git error.
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	// An already-expired budget is the state a killed command ends in.
	if err := d.runGitCmd(gitRepo, time.Nanosecond, "add", "-A", "."); !errors.Is(err, errGitCmdTimeout) {
		t.Fatalf("expired budget: got %v, want errGitCmdTimeout", err)
	}

	if err := d.runGitCmd(gitRepo, 30*time.Second, "not-a-git-command"); err == nil || errors.Is(err, errGitCmdTimeout) {
		t.Fatalf("real git failure: got %v, want a plain error", err)
	}
}

func TestIsIndexLockExistsError(t *testing.T) {
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

func TestClearStaleIndexLock_RespectsGrace(t *testing.T) {
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	t.Run("stale is removed", func(t *testing.T) {
		gitRepo := t.TempDir()
		initGitRepo(t, gitRepo)
		lockPath := ageIndexLock(t, gitRepo, 2*gitIndexLockGracePeriod)
		if !d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			t.Fatal("expected a lock older than the grace period to be cleared")
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Errorf("lock still present, stat err = %v", err)
		}
	})

	t.Run("fresh is left for its owner", func(t *testing.T) {
		gitRepo := t.TempDir()
		initGitRepo(t, gitRepo)
		lockPath := ageIndexLock(t, gitRepo, 0)
		if d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			t.Fatal("a lock younger than the grace period must not be cleared")
		}
		if _, err := os.Stat(lockPath); err != nil {
			t.Errorf("fresh lock should still exist: %v", err)
		}
	})

	t.Run("absent is a no-op", func(t *testing.T) {
		gitRepo := t.TempDir()
		initGitRepo(t, gitRepo)
		if d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			t.Error("no lock on disk should report nothing cleared")
		}
	})
}

func TestClearIndexLockModifiedSince_OnlyRecent(t *testing.T) {
	// A lock written after a command started is that command's own orphan.
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	started := time.Now().Add(-time.Second)
	lockPath := ageIndexLock(t, gitRepo, 0)
	if !d.clearIndexLockModifiedSince(gitRepo, started) {
		t.Fatal("expected a lock written after the command started to be cleared")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("lock still present, stat err = %v", err)
	}

	// A lock predating the command belongs to somebody else.
	gitRepo2 := t.TempDir()
	initGitRepo(t, gitRepo2)
	lockPath2 := ageIndexLock(t, gitRepo2, time.Hour)
	if d.clearIndexLockModifiedSince(gitRepo2, time.Now()) {
		t.Fatal("a lock older than the command must not be cleared")
	}
	if _, err := os.Stat(lockPath2); err != nil {
		t.Errorf("pre-existing lock should still exist: %v", err)
	}
}

func TestRunGitIndexCmd_RecoversFromStaleLock(t *testing.T) {
	// A lock orphaned by a dead tick must not fail the next one (gt-1aj2).
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	if err := os.WriteFile(filepath.Join(gitRepo, "new.jsonl"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	lockPath := ageIndexLock(t, gitRepo, 2*gitIndexLockGracePeriod)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if err := d.runGitIndexCmd(gitRepo, 30*time.Second, "add", "-A", "."); err != nil {
		t.Fatalf("stale index lock should be cleared and the command retried: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("stale lock should be gone, stat err = %v", err)
	}

	out, err := exec.Command("git", "-C", gitRepo, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if !contains(string(out), "new.jsonl") {
		t.Errorf("add should have staged new.jsonl, got: %q", out)
	}
}

func TestRunGitIndexCmd_LeavesFreshLock(t *testing.T) {
	// A lock with a live owner is not an orphan: failing is correct, and the
	// lock must survive so the real owner can finish.
	gitRepo := t.TempDir()
	initGitRepo(t, gitRepo)
	lockPath := ageIndexLock(t, gitRepo, 0)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	err := d.runGitIndexCmd(gitRepo, 30*time.Second, "add", "-A", ".")
	if err == nil {
		t.Fatal("expected the live lock to fail the command")
	}
	if !isIndexLockExistsError(err) {
		t.Errorf("expected an index.lock error, got: %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("a live owner's lock must not be removed: %v", statErr)
	}
}

func TestCommitAndPushJsonlBackup_RecoversFromStaleIndexLock(t *testing.T) {
	// End-to-end for gt-1aj2: the tick after a killed `git add` used to fail
	// with "index.lock: File exists" forever, and three such ticks escalated.
	root := t.TempDir()
	gitRepo := filepath.Join(root, "repo")
	if err := os.MkdirAll(gitRepo, 0755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, gitRepo)

	remote := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", gitRepo, "remote", "add", "origin", remote).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	dbDir := filepath.Join(gitRepo, "testdb")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 5)
	lockPath := ageIndexLock(t, gitRepo, 2*gitIndexLockGracePeriod)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err != nil {
		t.Fatalf("tick after a stale index lock should succeed, got: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("stale lock should have been cleared, stat err = %v", err)
	}

	out, err := exec.Command("git", "-C", remote, "log", "--all", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log on remote: %v", err)
	}
	if !contains(string(out), "backup") {
		t.Errorf("expected the backup commit to reach the remote, got: %q", out)
	}
}
