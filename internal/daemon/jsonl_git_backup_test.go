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
	"maps"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beadsql"
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
			name:     "real bead titled test the pager",
			record:   map[string]interface{}{"id": "gt-ok1", "title": "test the pager"},
			expected: false,
		},
		{
			name:     "real bead titled Test coverage for X",
			record:   map[string]interface{}{"id": "gt-ok2", "title": "Test coverage for the hook dispatcher"},
			expected: false,
		},
		{
			name:     "generated test issue title",
			record:   map[string]interface{}{"id": "gt-ok3", "title": "Test issue 3"},
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

	filtered, removed, err := filterTestPollutionStream(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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

	filtered, removed, err := filterTestPollutionStream(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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
	filtered, removed, err := filterTestPollutionStream(strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
	if len(filtered) != 0 {
		t.Errorf("expected empty output, got %q", filtered)
	}
}

// longLineIssuesJSONL is an export with a 2 MiB line in the middle, one
// pollution record before it, and real records on either side. A 1 MiB scanner
// cap stops at the long line, so the records after it are dropped and — because
// a pollution record was removed — the truncated buffer is written back over
// the export (gt-q6ljl).
func longLineIssuesJSONL() (content string, records []string) {
	records = []string{
		`{"id":"gt-aaa1","title":"First real bead"}`,
		`{"id":"gt-bbb2","title":"Second real bead"}`,
		`{"id":"gt-ddd4","title":"Long notes bead","description":"` + strings.Repeat("x", 2*1024*1024) + `"}`,
		`{"id":"gt-eee5","title":"Last real bead"}`,
	}
	polluted := `{"id":"gt-ccc3","title":"test_something"}`

	lines := []string{records[0], records[1], polluted, records[2], records[3]}
	return strings.Join(lines, "\n") + "\n", records
}

func TestFilterTestPollution_LongLineKeepsEveryOtherRecord(t *testing.T) {
	t.Parallel()
	input, records := longLineIssuesJSONL()

	filtered, removed, err := filterTestPollutionStream(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	lines := splitNonEmpty(string(filtered))
	if len(lines) != len(records) {
		t.Fatalf("kept %d records, want %d — a record after the long line was dropped", len(lines), len(records))
	}
	for _, want := range records {
		if !slices.Contains(lines, want) {
			t.Errorf("record missing after filtering: %.60s", want)
		}
	}
}

// errAfterReader yields data, then a hard read error — a read that fails
// mid-stream is not end of input.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestFilterTestPollutionStream_ReadErrorIsReported(t *testing.T) {
	t.Parallel()
	readErr := errors.New("input/output error")
	r := &errAfterReader{
		data: []byte(`{"id":"gt-aaa1","title":"First real bead"}` + "\n"),
		err:  readErr,
	}

	filtered, _, err := filterTestPollutionStream(r)
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want %v — a read error must not read as end of input", err, readErr)
	}
	if filtered != nil {
		t.Errorf("filtered = %q, want nil: a partial read is not a whole-file result", filtered)
	}
}

func TestApplyPollutionFilter_LongLineKeepsEveryOtherRecord(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "backup")
	issuesPath := filepath.Join(repo, "gt", "issues.jsonl")
	if err := os.MkdirAll(filepath.Dir(issuesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	input, records := longLineIssuesJSONL()
	if err := os.WriteFile(issuesPath, []byte(input), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if got := d.applyPollutionFilter(repo, []string{"gt"}); got["gt"] != 1 || len(got) != 1 {
		t.Errorf("applyPollutionFilter = %v, want only gt=1", got)
	}

	data, err := os.ReadFile(issuesPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != len(records) {
		t.Fatalf("file has %d records, want %d — the export was truncated", len(lines), len(records))
	}
	for _, want := range records {
		if !slices.Contains(lines, want) {
			t.Errorf("record missing from the rewritten export: %.60s", want)
		}
	}
}

func TestApplyPollutionFilter_ReadErrorLeavesFileUntouched(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "backup")
	issuesPath := filepath.Join(repo, "gt", "issues.jsonl")
	// A directory where the file belongs: open succeeds and the first read
	// fails, which is the mid-stream read error the filter must report rather
	// than mistake for end of input.
	if err := os.MkdirAll(issuesPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sentinel := filepath.Join(issuesPath, "keep")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var logs strings.Builder
	d := &Daemon{logger: log.New(&logs, "", 0)}
	if got := d.applyPollutionFilter(repo, []string{"gt"}); len(got) != 0 {
		t.Errorf("applyPollutionFilter = %v, want nothing removed on a read error", got)
	}
	if !strings.Contains(logs.String(), "issues.jsonl left untouched") {
		t.Errorf("read error was not reported; log = %q", logs.String())
	}
	fi, err := os.Stat(issuesPath)
	if err != nil || !fi.IsDir() {
		t.Fatalf("issues.jsonl was replaced on a read error (stat err=%v)", err)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "untouched" {
		t.Errorf("content under issues.jsonl changed: %q (err=%v)", b, err)
	}
}

func TestApplyPollutionFilter_MissingExportSkipsQuietly(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "backup")

	var logs strings.Builder
	d := &Daemon{logger: log.New(&logs, "", 0)}
	if got := d.applyPollutionFilter(repo, []string{"gt"}); len(got) != 0 {
		t.Errorf("applyPollutionFilter = %v, want nothing removed", got)
	}
	if logs.Len() != 0 {
		t.Errorf("a database with no export logged: %q", logs.String())
	}
}

// The per-shard loop keeps filtering the shards it can read: a shard that fails
// is named and the others still lose their pollution, so one unreadable file
// does not quietly disable the filter for the whole export (gt-fhcwe).
func TestApplyPollutionFilter_OneUnreadableShardStillFiltersTheRest(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "backup")
	dbDir := filepath.Join(repo, "gt")
	// Shard 1 cannot be read — a directory where the file belongs — so only
	// shard 2 is filtered.
	if err := os.MkdirAll(filepath.Join(dbDir, "issues.jsonl"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	good := `{"id":"gt-aaa1","title":"First real bead"}`
	polluted := `{"id":"gt-bbb2","title":"Test issue 7"}`
	second := filepath.Join(dbDir, "issues.2.jsonl")
	if err := os.WriteFile(second, []byte(good+"\n"+polluted+"\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var logs strings.Builder
	d := &Daemon{logger: log.New(&logs, "", 0)}
	removed := d.applyPollutionFilter(repo, []string{"gt"})
	if removed["gt"] != 1 || len(removed) != 1 {
		t.Errorf("applyPollutionFilter = %v, want only gt=1, from the shard it could read", removed)
	}
	if !strings.Contains(logs.String(), "issues.jsonl left untouched") {
		t.Errorf("the unreadable shard was not named; log = %q", logs.String())
	}
	data, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != good+"\n" {
		t.Errorf("issues.2.jsonl = %q, want only the real record", data)
	}
}

// A shard the post-scrub check cannot read, and a database whose export it
// cannot find, leave the export unverified instead of reading as clean: the
// caller passes the databases this cycle exported, so nothing to read is the
// export missing rather than a database that never had one (gt-fhcwe).
func TestVerifyNoPollution_UnreadableShardIsAnError(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(filepath.Join(repo, "gt", "issues.2.jsonl"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	if got, err := d.verifyNoPollution(repo, []string{"gt"}); err == nil || got != 0 || !strings.Contains(err.Error(), "issues.2.jsonl") {
		t.Errorf("verifyNoPollution = (%d, %v), want an error naming the shard it could not read", got, err)
	}
	if got, err := d.verifyNoPollution(repo, []string{"missing"}); err == nil || got != 0 {
		t.Errorf("verifyNoPollution = (%d, %v), want an error for a database with no export", got, err)
	}
	// A directory with no issues shard in it is no export either.
	if err := os.MkdirAll(filepath.Join(repo, "empty"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got, err := d.verifyNoPollution(repo, []string{"empty"}); err == nil || got != 0 {
		t.Errorf("verifyNoPollution = (%d, %v), want an error for a database with no issues shard", got, err)
	}
}

// A removed count above the export's total cannot come from one export; the
// recount reports it and leaves the count alone rather than clamp it (gt-q6ljl).
func TestRecountAfterFilter_RejectsRemovedAboveTotal(t *testing.T) {
	t.Parallel()
	counts := map[string]int{"gt": 7}
	if err := recountAfterFilter(counts, map[string]int{"gt": 8}); err == nil {
		t.Fatal("recountAfterFilter returned nil error for more records removed than exported")
	}
	if counts["gt"] != 7 {
		t.Errorf("counts[gt] = %d, want the untouched 7", counts["gt"])
	}
}

// A rejected removal leaves every count as it arrived, not just its own. The
// recount used to adjust each database as it reached it, so which counts a
// rejected tick had already changed came down to map order — the counts it
// returned were neither the old ones nor the new ones (gt-83c3w).
func TestRecountAfterFilter_RejectsWithoutAdjustingAnyCount(t *testing.T) {
	t.Parallel()
	counts := map[string]int{"hq": 100, "gt": 50, "longeye": 20, "beads": 8, "other": 9}
	// hq's removal is fine on its own; "beads" could not have come from one
	// export. Map order still decides which is reached first, so a wider map
	// makes the partly adjusted result near-certain under the old loop rather
	// than a coin flip — the assertion is on the whole map because that is the
	// state the caller is left holding.
	removed := map[string]int{"hq": 1, "gt": 2, "longeye": 3, "other": 4, "beads": 9}
	if err := recountAfterFilter(counts, removed); err == nil {
		t.Fatal("recountAfterFilter returned nil error for more records removed than exported")
	}
	want := map[string]int{"hq": 100, "gt": 50, "longeye": 20, "beads": 8, "other": 9}
	if !maps.Equal(counts, want) {
		t.Errorf("counts = %v, want %v — a rejected recount must leave every count alone", counts, want)
	}
}

// TestRecountAfterFilterKeepsCountMetric is the gt-tx0ia regression: counts[db]
// is the exportDatabaseToJsonl total (issues plus the supplemental tables), and
// the recount must stay on that metric. It used to overwrite the count with the
// issues.jsonl line count, so a filter tick committed a different number than a
// clean tick over the same data.
func TestRecountAfterFilterKeepsCountMetric(t *testing.T) {
	t.Parallel()
	// hq: 30 issue lines (one filtered) + 70 supplemental rows = 100 exported
	// before the filter. gt had nothing filtered and keeps its count, as does a
	// database the filter never saw.
	counts := map[string]int{"hq": 100, "gt": 50, "other": 9}
	if err := recountAfterFilter(counts, map[string]int{"hq": 1}); err != nil {
		t.Fatalf("recountAfterFilter: %v", err)
	}
	want := map[string]int{"hq": 99, "gt": 50, "other": 9}
	if !maps.Equal(counts, want) {
		t.Errorf("counts = %v, want %v — the unfiltered total minus the removed issue lines", counts, want)
	}
}

// TestJSONLSupplementalExportBudget pins the supplemental bound to one shared
// budget per database rather than a timeout per table: six tables at five
// minutes each held a database for half an hour, well past a 15-minute tick
// (gt-4nqej).
func TestJSONLSupplementalExportBudget(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if got := d.jsonlSupplementalExportBudget(); got != jsonlSupplementalExportTimeout {
		t.Errorf("default budget = %s, want %s", got, jsonlSupplementalExportTimeout)
	}
	if jsonlSupplementalExportTimeout <= jsonlExportTimeout {
		t.Errorf("supplemental budget = %s, want more than the issues bound %s for the largest table (events)",
			jsonlSupplementalExportTimeout, jsonlExportTimeout)
	}
	if jsonlSupplementalExportTimeout >= defaultJsonlGitBackupInterval {
		t.Errorf("supplemental budget = %s, want a bounded value well inside the %s tick",
			jsonlSupplementalExportTimeout, defaultJsonlGitBackupInterval)
	}
	d.jsonlSupplementalTimeout = time.Minute
	if got := d.jsonlSupplementalExportBudget(); got != time.Minute {
		t.Errorf("budget with an override = %s, want the test's 1m", got)
	}
}

func TestRunJSONLExport_DeadlineKillNamesTableAndTimeout(t *testing.T) {
	t.Parallel()
	// A run that fails under an expired deadline is the mid-run kill: the run
	// returns an error and ctx.Err() reports DeadlineExceeded. The run is a
	// stand-in, so the test needs no dolt binary.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	timeout := jsonlSupplementalExportTimeout
	cmd := jsonlExportCommand(ctx, false, "", 0, "", "", "select 1", t.TempDir())
	killed := func(c *exec.Cmd) error {
		_, _ = c.Stderr.Write([]byte("table locked\n"))
		// exec.CommandContext reports the context's error for a run its
		// deadline killed.
		return ctx.Err()
	}

	_, err := runJSONLExportWith(ctx, killed, cmd, "events", timeout)
	if err == nil {
		t.Fatal("runJSONLExport returned nil for a killed run")
	}
	for _, want := range []string{"events", "5m0s", "timeout", "table locked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %q does not wrap the deadline cause", err)
	}
}

// A run that succeeds hands back its stdout and no error.
func TestRunJSONLExportReturnsStdoutOfASuccessfulRun(t *testing.T) {
	t.Parallel()
	cmd := jsonlExportCommand(context.Background(), false, "", 0, "", "", "select 1", t.TempDir())
	ok := func(c *exec.Cmd) error {
		_, _ = c.Stdout.Write([]byte("{\"rows\":[]}"))
		return nil
	}
	out, err := runJSONLExportWith(context.Background(), ok, cmd, "events", jsonlSupplementalExportTimeout)
	if err != nil || string(out) != "{\"rows\":[]}" {
		t.Fatalf("runJSONLExportWith = %q, %v; want the run's stdout and no error", out, err)
	}
}

func TestJSONLExportFailure(t *testing.T) {
	t.Parallel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cause := errors.New("signal: killed")

	tests := []struct {
		name       string
		ctx        context.Context
		stderr     string
		wantAll    []string
		wantAbsent string
	}{
		{"deadline kill names table and timeout", expired, "", []string{"events", "5m0s", "signal: killed"}, ""},
		{"deadline kill keeps stderr", expired, " table locked\n", []string{"5m0s", "signal: killed", "table locked"}, ""},
		{"plain failure keeps stderr, no timeout blame", context.Background(), "table locked\n", []string{"signal: killed", "table locked"}, "timeout"},
		{"plain failure without stderr is the bare error", context.Background(), "", []string{"signal: killed"}, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := jsonlExportFailure(tt.ctx, cause, tt.stderr, "events", jsonlSupplementalExportTimeout)
			for _, want := range tt.wantAll {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if tt.wantAbsent != "" && strings.Contains(err.Error(), tt.wantAbsent) {
				t.Errorf("error %q contains %q", err, tt.wantAbsent)
			}
			if !errors.Is(err, cause) {
				t.Errorf("error %q does not wrap the cause", err)
			}
		})
	}
}

func TestJSONLExportCommand_PasswordIsNotInArgv(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-dolt-password"

	cmd := jsonlExportCommand(context.Background(), true, "127.0.0.1", 3307, "root", secret, "SELECT 1", t.TempDir())

	for _, arg := range cmd.Args {
		if arg == secret {
			t.Fatalf("password appears in argv: %v", cmd.Args)
		}
	}
	if slices.Contains(cmd.Args, "-p") {
		t.Errorf("argv still passes -p: %v", cmd.Args)
	}
	if got := envValue(cmd.Env, "DOLT_CLI_PASSWORD"); got != secret {
		t.Errorf("DOLT_CLI_PASSWORD = %q, want the configured password", got)
	}
	entries := 0
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "DOLT_CLI_PASSWORD=") {
			entries++
		}
	}
	if entries != 1 {
		t.Errorf("DOLT_CLI_PASSWORD appears %d times in the child env, want exactly 1", entries)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
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
			"hq": {{Db: "hq", N: 1266, Age: 0, Source: spikeSourceCommit, Version: countMetricVersion}},
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

// TestVerifyExportCounts_ZeroBaselineGrowth: a database whose committed level
// is 0 has no ratio to divide by — |current-0|/0 is +Inf, which beat every
// threshold — and since a halted tick commits nothing, the level stayed 0, so
// every later run halted on the same non-measurement and the database could
// never back up at all (gt-ttqq8). Growth is measured against the small-change
// floor instead: finite, and a bulk load into an empty database still halts for
// an operator to accept.
func TestVerifyExportCounts_ZeroBaselineGrowth(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755)
	commitBackup(t, d, gitRepo, "testdb", 0)

	spikes, err := d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 25}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 {
		t.Fatalf("0 → 25 = %+v, want one spike: growth past the floor from empty is the bulk load an operator confirms", spikes)
	}
	if math.IsInf(spikes[0].Delta, 0) || math.IsNaN(spikes[0].Delta) {
		t.Errorf("delta = %v; a zero baseline must not divide to a non-finite ratio", spikes[0].Delta)
	}
	if spikes[0].Previous != 0 || spikes[0].Current != 25 {
		t.Errorf("spike = %+v, want 0 → 25", spikes[0])
	}

	// Below the floor the same growth is an ordinary small change, so a database
	// that fills a few records at a time backs up unattended.
	spikes, err = d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 15}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 0 {
		t.Errorf("0 → 15 = %+v, want no spike below the small-change floor", spikes)
	}

	// Once a level is committed the guard measures as usual again.
	commitBackup(t, d, gitRepo, "testdb", 25)
	spikes, err = d.verifyExportCounts(gitRepo, []string{"testdb"}, map[string]int{"testdb": 120}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 || spikes[0].Previous != 25 || spikes[0].Current != 120 {
		t.Errorf("25 → 120 = %+v, want one spike measured against 25", spikes)
	}
}

// acceptHintSubject returns the commit subject a halt escalation tells the
// operator to land.
func acceptHintSubject(t *testing.T, reason string) string {
	t.Helper()
	_, rest, ok := strings.Cut(reason, "-m ")
	if !ok {
		t.Fatalf("the halt escalation names no commit to land:\n%s", reason)
	}
	subject, err := strconv.Unquote(strings.TrimSpace(rest))
	if err != nil {
		t.Fatalf("the named commit subject is not a quoted string (%v): %q", err, rest)
	}
	return subject
}

// TestSyncJsonlGitBackupZeroBaselineGrowthHaltsWithTheAcceptPath: what a halt
// names must be a subject the baseline parser reads, or accepting the export is
// a command that leaves the patrol halted — the failure gt-ttqq8 is about. The
// escalation carries the counts this run measured, under the metric the writer
// itself records, so landing it is the level the next tick compares against.
func TestSyncJsonlGitBackupZeroBaselineGrowthHaltsWithTheAcceptPath(t *testing.T) {
	t.Parallel()
	lines := 25
	var logs strings.Builder
	d, rec, gitRepo := metricChangeDaemon(t, seedSubjectPrefix+"hq=0", &lines, &logs)

	d.syncJsonlGitBackup()

	open := rec.OpenEscalations(alertKeyJSONLSpike)
	if len(open) != 1 {
		t.Fatalf("open spike escalations = %+v, want one", open)
	}
	subject := acceptHintSubject(t, open[0].Reason)
	if got := parseCommitCounts(subject)["hq"]; got != 25 {
		t.Errorf("the accept subject %q names hq=%d, want the level this run measured (25)", subject, got)
	}
	if v := parseCommitVersion(subject); v != countMetricVersion {
		t.Errorf("the accept subject %q reads as metric version %d, want %d", subject, v, countMetricVersion)
	}
	if subjects := backupSubjects(t, d, gitRepo); len(subjects) != 1 {
		t.Errorf("a halted tick committed: %q", subjects)
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
			"db1": {{Db: "db1", N: 1271, Age: 0, Version: countMetricVersion}},
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

func TestCountFileLines_LongLineIsCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "issues.jsonl")

	long := `{"id":"gt-ddd4","title":"Long notes bead","description":"` + strings.Repeat("x", 2*1024*1024) + `"}`
	content := `{"id":"gt-aaa1","title":"First"}` + "\n" + long + "\n" + `{"id":"gt-eee5","title":"Last"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := countFileLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Errorf("countFileLines = %d, want 3 — a long line hid the lines after it", got)
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
	// The notifier records the alerts and clears a verify raises, so a test
	// that does not look at them never reaches `gt escalate`.
	d := &Daemon{logger: log.New(io.Discard, "", 0), notifier: notifyfake.New()}
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

// nLines is the export text writeNLines writes, for a test that seeds a commit
// with it as well as a worktree.
func nLines(n int) string {
	var buf []byte
	for i := 0; i < n; i++ {
		line, _ := json.Marshal(map[string]interface{}{"id": "rec-" + itoa(i), "title": "Record " + itoa(i)})
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	return string(buf)
}

func writeNLines(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(nLines(n)), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// seedSubjectPrefix is the head of a backup commit subject written under the
// current count metric ("backup <ts> m<V>: "), for tests that seed history ahead
// of a tick.
var seedSubjectPrefix = fmt.Sprintf("backup 2026-01-01 00:00 m%d: ", countMetricVersion)

// commitBackup writes dbDir/issues.jsonl with n lines and commits it with a
// backup-style subject line ("backup <ts> m<V>: <db>=<n>"), matching
// commitAndPushJsonlBackup — the rolling spike baseline derives its window from
// these subject lines (gt-tj-he). The subject carries the current
// countMetricVersion (seedSubjectPrefix), so the commit enters the window; a
// test of older history writes its subject with commitAll instead (gt-26qqh).
func commitBackup(t *testing.T, d *Daemon, gitRepo, db string, n int) {
	t.Helper()
	writeNLines(t, filepath.Join(gitRepo, db, "issues.jsonl"), n)
	commitAll(t, d, gitRepo, fmt.Sprintf("%s%s=%d", seedSubjectPrefix, db, n))
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
	if err := os.WriteFile(filepath.Join(townRoot, "daemon"), nil, 0o644); err != nil {
		t.Fatalf("write daemon: %v", err)
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

// TestCommitAndPushJsonlBackup_ExhaustedLocalBudgetFailsFast is the gt-ttqq8
// regression for the phase deadline: the local commands share one, so
// time.Until(localDeadline) reaches zero once an earlier one has spent it, and
// the git runner sets a context timeout only above zero (internal/git/exec.go)
// — the commit after that ran with no deadline at all. A commit that hangs
// holds jsonlGitBackupRunning, and every later backup trigger skips while it
// does. The phase refuses the command instead, from the first it cannot bound.
func TestCommitAndPushJsonlBackup_ExhaustedLocalBudgetFailsFast(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	if err := os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(gitRepo, "testdb", "issues.jsonl"), 5)

	d.jsonlLocalPhaseTimeout = -time.Second

	err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil)
	if err == nil {
		t.Fatal("a spent local phase budget must fail the tick, got nil")
	}
	if !contains(err.Error(), "git add") || !contains(err.Error(), "budget") {
		t.Errorf("error = %v; want the first unbindable command and the budget named", err)
	}
	if subjects := backupSubjects(t, d, gitRepo); len(subjects) != 1 {
		t.Errorf("commits = %q; want the init commit alone — a spent phase runs no git command", subjects)
	}
}

// TestStageGit_ReReadsTheBudgetForEachAttempt: runGitIndexOp retries an op that
// lost a race for the index lock, so a budget read once for the phase would let
// the retry outlive the deadline — and the command it then runs carries no
// deadline at all (gt-ttqq8).
func TestStageGit_ReReadsTheBudgetForEachAttempt(t *testing.T) {
	t.Parallel()
	gitRepo := lockTestRepo(t)
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	reads, calls := 0, 0
	remaining := func() time.Duration {
		reads++
		if reads == 1 {
			return time.Minute
		}
		return -time.Second
	}
	err := d.stageGit(gitRepo, "git add", remaining, func(daemonGit) error {
		calls++
		return indexLockErr // the lock appeared between the check and git's attempt
	})
	if err == nil || !contains(err.Error(), "budget") {
		t.Fatalf("stageGit = %v; want the retried command refused on a spent budget", err)
	}
	if reads != 2 {
		t.Errorf("the budget was read %d time(s), want once per attempt", reads)
	}
	if calls != 1 {
		t.Errorf("the op ran %d time(s); the refused retry must not run it", calls)
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

	// Nothing new to back up: no second commit, and origin already has HEAD, so
	// nothing to push either.
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again, _ := f.Open(gitRepo).Rev("HEAD"); again != head {
		t.Errorf("an unchanged export committed again (%s)", again)
	}
}

// TestCommitAndPushJsonlBackup_RetriesUnpushedCommitWhenNothingStaged is the
// gt-fxm2i regression for the push half of the bug: a tick that committed and
// failed to push used to be reported as success by the next tick, whose
// identical exports staged nothing. The push must be retried, and its failure
// still surfaced, so syncJsonlGitBackup keeps the failure counter and its alert.
func TestCommitAndPushJsonlBackup_RetriesUnpushedCommitWhenNothingStaged(t *testing.T) {
	t.Parallel()
	d, f := backupTestDaemon(t)
	root := t.TempDir()

	// origin carries an unrelated history, so every push is rejected
	// non-fast-forward — the shape of "commit made, push fails".
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", "remote-only", map[string]string{"remote.txt": "x\n"})

	gitRepo := filepath.Join(root, "repo")
	if err := os.MkdirAll(gitRepo, 0755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, d, gitRepo)
	f.AddRemote(t, gitRepo, "origin", remote)
	if err := os.MkdirAll(filepath.Join(gitRepo, "testdb"), 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(gitRepo, "testdb", "issues.jsonl"), 5)

	// Tick 1: commits locally, fails to push.
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err == nil {
		t.Fatal("tick 1: expected the push to fail, got nil")
	}
	subjects := backupSubjects(t, d, gitRepo)
	if len(subjects) != 2 || !strings.HasPrefix(subjects[0], "backup ") {
		t.Fatalf("tick 1 subjects = %q, want an init commit and one backup commit", subjects)
	}

	// Tick 2: identical exports, nothing staged. The unpushed commit must be
	// retried — reported as a failure, not a success.
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err == nil {
		t.Fatal("tick 2: an unpushed commit must not be reported as success")
	}
	if again := backupSubjects(t, d, gitRepo); len(again) != len(subjects) {
		t.Errorf("tick 2 added a commit: %q", again)
	}
}

// TestCommitAndPushJsonlBackup_PushesUnpushedCommitWhenNothingStaged is the
// other half of gt-fxm2i: once origin accepts the previously unpushed commit,
// the push-less tick reports success, which is what lets syncJsonlGitBackup
// reset its failure counter and clear the push alert.
func TestCommitAndPushJsonlBackup_PushesUnpushedCommitWhenNothingStaged(t *testing.T) {
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

	// A backup commit that never reached origin — the state a failed push
	// leaves behind.
	commitBackup(t, d, gitRepo, "testdb", 5)
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip == head {
		t.Fatal("fixture: the backup commit should not be on origin yet")
	}

	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err != nil {
		t.Fatalf("tick with an unpushed commit: %v", err)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip != head {
		t.Errorf("origin main = %s, want the previously unpushed commit %s", tip, head)
	}
}

// TestIsOversizeFilePushError pins the classifier the recovery hangs off:
// GitHub's per-file refusal is recognized, and the other push failures the
// backup sees are not — a non-fast-forward rejection is not a reason to
// replace the unpushed range. Each phrase is the refusal itself, so a lone
// fragment of one is not a match either: arming this recovery drops commits
// (gt-bodcw).
func TestIsOversizeFilePushError(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		errMsg string
		want   bool
	}{
		{"GH001", "git push: remote: error: GH001: Large files detected. You may want to try Git Large File Storage - https://git-lfs.github.com.", true},
		{"per-file limit", "git push: remote: error: File hq/events.jsonl is 100.08 MB; this exceeds GitHub's file size limit of 100.00 MB", true},
		{"bare GH001", "git push: remote: error: GH001: something else entirely", false},
		{"bare fragment", "git push: remote: error: Large files detected.", false},
		{"non-fast-forward", "git push: To /tmp/origin.git\n ! [rejected]        main -> main (non-fast-forward)", false},
		{"auth failure", "git push: fatal: Authentication failed for 'https://github.com/x/y'", false},
		{"postBuffer 400", "git push: error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400", false},
		{"timeout", "git push: context deadline exceeded", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isOversizeFilePushError(tt.errMsg); got != tt.want {
				t.Errorf("isOversizeFilePushError(%q) = %v, want %v", tt.errMsg, got, tt.want)
			}
		})
	}
}

// oversizeBackupFixture is a backup repository cloned from a remote whose main
// is at the returned seed commit, with a file limit small enough for a test to
// exceed — mirrored on both sides, so the remote refuses exactly what the
// daemon considers too large.
func oversizeBackupFixture(t *testing.T) (d *Daemon, f *gitfake.Fake, gitRepo, remote, seed string, limit int64) {
	t.Helper()
	d, f = backupTestDaemon(t)
	limit = 1024
	d.jsonlPushFileLimit = limit
	f.SetPushFileLimit(limit)
	root := t.TempDir()
	remote = filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", "seed", map[string]string{"README": "init\n"})
	gitRepo = filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)
	seed = f.Ref(remote, "refs/heads/main")
	return d, f, gitRepo, remote, seed, limit
}

// TestCommitAndPushJsonlBackup_ReplacesUnpushedHistoryOverAnOversizeFile is
// the gt-q30zx regression, and the acceptance criterion of that bead.
//
// A tick whose export was just fixed still cannot push: the remote weighs the
// objects the push sends, and the commit made before the export sharded its
// tables (gt-4btp4) keeps its over-limit blob reachable from HEAD. The tick
// must replace that unpushed range with one commit on origin's tip, whose tree
// holds no such file, and push it.
func TestCommitAndPushJsonlBackup_ReplacesUnpushedHistoryOverAnOversizeFile(t *testing.T) {
	t.Parallel()
	d, f, gitRepo, remote, seed, limit := oversizeBackupFixture(t)
	dbDir := filepath.Join(gitRepo, "hq")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}

	// The history this bead is about, as two unpushed commits: the export
	// before sharding wrote one over-limit file, the sharded export after it
	// writes only files the limit accepts.
	big := bytes.Repeat([]byte("x"), int(limit)*2)
	if err := os.WriteFile(filepath.Join(dbDir, "events.jsonl"), big, 0644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, d, gitRepo, "backup 2026-10-16 10:00 m2: hq=1")
	if err := os.WriteFile(filepath.Join(dbDir, "events.jsonl"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 5)
	commitAll(t, d, gitRepo, "backup 2026-10-16 10:15 m2: hq=5")

	// The tick re-exports the same sharded files, so it has nothing to commit:
	// only the push can fail, and it fails until the unpushed range is gone.
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"hq"}, map[string]int{"hq": 5}, nil); err != nil {
		t.Fatalf("a tick whose unpushed history holds an over-limit file should recover, got: %v", err)
	}

	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip != head {
		t.Fatalf("origin main = %s, want the replacing commit %s", tip, head)
	}
	// Exactly one commit carries the export now, parented on origin's tip: the
	// range holding the over-limit blob went rather than being pushed.
	if parents := f.Parents(head); len(parents) != 1 || parents[0] != seed {
		t.Fatalf("the replacing commit's parents = %v, want origin's tip %s alone", parents, seed)
	}
	sizes, err := d.backupGitAt(gitRepo, 0).TreeFileSizes("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) == 0 {
		t.Fatal("the replacing commit's tree is empty")
	}
	for path, size := range sizes {
		if size > limit {
			t.Errorf("%s is %d bytes in the pushed tree, over the %d-byte limit the remote refuses", path, size, limit)
		}
	}
	// The replacing commit keeps this tick's subject, counts and metric
	// version included: the spike baseline is read back out of these lines
	// (gt-26qqh).
	if subject := backupSubjects(t, d, gitRepo)[0]; !strings.HasPrefix(subject, "backup ") || !strings.Contains(subject, ": hq=5") {
		t.Errorf("commit subject = %q; want this tick's counts", subject)
	}
}

// TestCommitAndPushJsonlBackup_KeepsHistoryWhenTheExportIsStillOversize is the
// other half of gt-q30zx: replacing the unpushed range only helps when the
// tree being committed fits. A file still over the limit means the export is
// what needs fixing, so the recovery must decline, name the file, and leave
// the branch where it is rather than flatten the history into another refusal.
func TestCommitAndPushJsonlBackup_KeepsHistoryWhenTheExportIsStillOversize(t *testing.T) {
	t.Parallel()
	d, f, gitRepo, remote, seed, limit := oversizeBackupFixture(t)
	dbDir := filepath.Join(gitRepo, "hq")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), int(limit)*2)
	if err := os.WriteFile(filepath.Join(dbDir, "events.jsonl"), big, 0644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, d, gitRepo, "backup 2026-10-16 10:00 m2: hq=1")
	commit, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}

	err = d.commitAndPushJsonlBackup(gitRepo, []string{"hq"}, map[string]int{"hq": 1}, nil)
	if err == nil {
		t.Fatal("a push refused for an over-limit file must stay a failure while the export holds it")
	}
	if !strings.Contains(err.Error(), "hq/events.jsonl") || !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", len(big))) {
		t.Errorf("error = %v; want it to name the over-limit file and its size", err)
	}
	if head, _ := f.Open(gitRepo).Rev("HEAD"); head != commit {
		t.Errorf("HEAD = %s, want the unpushed commit %s left alone", head, commit)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip != seed {
		t.Errorf("origin main = %s, want it still at the seed %s", tip, seed)
	}
}

// TestOverLimitFilesBoundary pins the daemon's size check to the boundary the
// remote uses: a file of exactly limit bytes is one a push carries, so only a
// bigger one is over the limit — read the other way round, the recovery
// declines the drop that would let the push through (gt-bodcw).
func TestOverLimitFilesBoundary(t *testing.T) {
	t.Parallel()
	const limit = 1024
	for _, tt := range []struct {
		name  string
		sizes map[string]int64
		want  []string
	}{
		{"under the limit", map[string]int64{"hq/issues.jsonl": limit - 1}, nil},
		{"at the limit", map[string]int64{"hq/issues.jsonl": limit}, nil},
		{"over the limit", map[string]int64{"hq/issues.jsonl": limit + 1},
			[]string{fmt.Sprintf("hq/issues.jsonl (%d bytes)", limit+1)}},
		{"both, in path order", map[string]int64{"hq/issues.jsonl": limit + 1, "hq/events.jsonl": limit + 2},
			[]string{fmt.Sprintf("hq/events.jsonl (%d bytes)", limit+2), fmt.Sprintf("hq/issues.jsonl (%d bytes)", limit+1)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := overLimitFiles(tt.sizes, limit); !slices.Equal(got, tt.want) {
				t.Errorf("overLimitFiles(%v, %d) = %q, want %q", tt.sizes, limit, got, tt.want)
			}
		})
	}
}

// TestCommitAndPushJsonlBackup_RecoversWhenTheExportSitsAtTheLimit is the
// boundary half of the recovery's size guard (gt-bodcw): a tree whose largest
// file is exactly the limit is one the remote accepts, so the tick must
// replace the unpushed range and push rather than decline as though the export
// were too big.
func TestCommitAndPushJsonlBackup_RecoversWhenTheExportSitsAtTheLimit(t *testing.T) {
	t.Parallel()
	d, f, gitRepo, remote, seed, limit := oversizeBackupFixture(t)
	dbDir := filepath.Join(gitRepo, "hq")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	// The over-limit blob sits in the commit below; the tree on top of it
	// holds a file of exactly the limit.
	if err := os.WriteFile(filepath.Join(dbDir, "events.jsonl"), bytes.Repeat([]byte("x"), int(limit)*2), 0644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, d, gitRepo, "backup 2026-10-16 10:00 m2: hq=1")
	if err := os.WriteFile(filepath.Join(dbDir, "events.jsonl"), bytes.Repeat([]byte("y"), int(limit)), 0644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, d, gitRepo, "backup 2026-10-16 10:15 m2: hq=1")

	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"hq"}, map[string]int{"hq": 1}, nil); err != nil {
		t.Fatalf("a tick whose tree sits exactly at the limit should recover, got: %v", err)
	}
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip != head {
		t.Fatalf("origin main = %s, want the replacing commit %s", tip, head)
	}
	if parents := f.Parents(head); len(parents) != 1 || parents[0] != seed {
		t.Fatalf("the replacing commit's parents = %v, want origin's tip %s alone", parents, seed)
	}
}

// TestRecoverOversizeBackupPush_DeclinesWhenOriginTipIsUnknown pins the first
// guard: without origin/<branch>'s tip there is no telling which commits are
// unpushed, so the range under HEAD stays (gt-bodcw).
func TestRecoverOversizeBackupPush_DeclinesWhenOriginTipIsUnknown(t *testing.T) {
	t.Parallel()
	d, f, gitRepo, _, _, _ := oversizeBackupFixture(t)
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	err = d.recoverOversizeBackupPush(gitRepo, "release", "backup 2026-10-16 10:00 m2: hq=1")
	if err == nil || !strings.Contains(err.Error(), "origin/release's tip is not known locally") {
		t.Fatalf("error = %v, want the unknown origin tip named", err)
	}
	if now, _ := f.Open(gitRepo).Rev("HEAD"); now != head {
		t.Errorf("HEAD = %s, want the untouched %s", now, head)
	}
}

// TestRecoverOversizeBackupPush_DeclinesWhenOriginAlreadyCarriesHead pins the
// guard against a refusal that is not about the unpushed range at all: with
// origin's tip at HEAD there is nothing for a replacing commit to carry
// (gt-bodcw).
func TestRecoverOversizeBackupPush_DeclinesWhenOriginAlreadyCarriesHead(t *testing.T) {
	t.Parallel()
	d, f, gitRepo, _, _, _ := oversizeBackupFixture(t)
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	f.SetRef(t, gitRepo, "refs/remotes/origin/main", head)
	err = d.recoverOversizeBackupPush(gitRepo, "main", "backup 2026-10-16 10:00 m2: hq=1")
	if err == nil || !strings.Contains(err.Error(), "already carries HEAD") {
		t.Fatalf("error = %v, want the refusal declined", err)
	}
	if now, _ := f.Open(gitRepo).Rev("HEAD"); now != head {
		t.Errorf("HEAD = %s, want the untouched %s", now, head)
	}
}

// TestRecoverOversizeBackupPush_DeclinesWhenOriginHoldsUnaccountedCommits is
// the gt-63sz hazard: origin's branch moved on without this clone, so the
// commits HEAD cannot account for would go with a replacement the clone has no
// standing to make. HEAD and origin both stay where they are (gt-bodcw).
func TestRecoverOversizeBackupPush_DeclinesWhenOriginHoldsUnaccountedCommits(t *testing.T) {
	t.Parallel()
	d, f, gitRepo, remote, _, _ := oversizeBackupFixture(t)
	ahead := f.Commit(t, remote, "main", "operator commit", map[string]string{"README": "operator\n"})
	f.SetRef(t, gitRepo, "refs/remotes/origin/main", ahead)
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	err = d.recoverOversizeBackupPush(gitRepo, "main", "backup 2026-10-16 10:00 m2: hq=1")
	if err == nil || !strings.Contains(err.Error(), "cannot account for") {
		t.Fatalf("error = %v, want the unaccounted range declined", err)
	}
	if now, _ := f.Open(gitRepo).Rev("HEAD"); now != head {
		t.Errorf("HEAD = %s, want the untouched %s", now, head)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip != ahead {
		t.Errorf("origin main = %s, want it still at the operator commit %s", tip, ahead)
	}
}

// TestSyncJsonlGitBackup_AllExportsFailedEscalates is the gt-fxm2i regression
// for the export phase: when every database fails to export, the cycle must
// escalate, not merely log — matching the init and no-databases paths.
func TestSyncJsonlGitBackup_AllExportsFailedEscalates(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	rec := notifyfake.New()
	d := &Daemon{
		logger:   log.New(&logs, "", 0),
		config:   &Config{TownRoot: townRoot},
		notifier: rec,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, Databases: []string{"hq", "gt"}},
		}},
	}
	useGitfake(t, d)
	d.jsonlExportFn = func(db, gitRepo, dataDir string, scrub bool) (int, error) {
		return 0, errors.New("dolt: database not found")
	}

	d.syncJsonlGitBackup()

	got := rec.Escalations()
	if len(got) != 1 || got[0].Escalation.Fingerprint != alertKeyJSONLExport {
		t.Fatalf("escalations = %+v, want one under %s", got, alertKeyJSONLExport)
	}
	reason := got[0].Escalation.Reason
	if !strings.Contains(reason, "hq") || !strings.Contains(reason, "gt") {
		t.Errorf("escalation reason = %q, want both failed databases named", reason)
	}
}

// TestSyncJsonlGitBackupFilterTickDoesNotHalt is the gt-tx0ia regression: a
// tick that filters one bead must commit the same count metric as the clean tick
// before it. The filter tick used to commit the issues.jsonl line count against
// a baseline holding the all-tables total, which the spike guard read as a 70%
// drop, so the backup halted and the baseline could never advance.
//
// The tick is asserted to commit, and the assertion is on that commit: a filter
// tick whose export is byte-identical to the previous one commits nothing, so
// checking whatever commits happen to exist passes without ever reading a
// filter-tick count (gt-83c3w).
func TestSyncJsonlGitBackupFilterTickDoesNotHalt(t *testing.T) {
	t.Parallel()
	const (
		cleanIssues   = 30 // real issue rows in the clean tick
		supplemental  = 70 // rows across the supplemental tables per tick
		cleanCount    = cleanIssues + supplemental
		pollutedTitle = "Test Issue 42"
	)
	// The filter tick exports one more real issue row as well as the stray test
	// bead, so its filtered export still differs from the previous commit and
	// the tick commits a count to check. Without the extra row the two exports
	// are byte-identical, the tick has nothing to commit, and the count metric
	// this test exists to guard is never exercised (gt-83c3w).
	realIssues := cleanIssues
	filteredCount := cleanIssues + 1 + supplemental

	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	// The seed commit carries a count so the spike guard has a baseline.
	f.Commit(t, remote, "main", fmt.Sprintf("%shq=%d", seedSubjectPrefix, cleanCount), map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq"}},
	}}

	// polluted adds one test-titled bead to the export, as a tick with a stray
	// test bead does. exportDatabaseToJsonl counts it, like every other row.
	polluted := false
	d.jsonlExportFn = func(db, repo, dataDir string, scrub bool) (int, error) {
		dbDir := filepath.Join(repo, db)
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return 0, err
		}
		writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), realIssues)
		total := realIssues
		if polluted {
			bad, _ := json.Marshal(map[string]interface{}{"id": "hq-bad1", "title": pollutedTitle})
			fh, err := os.OpenFile(filepath.Join(dbDir, "issues.jsonl"), os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				return 0, err
			}
			_, err = fh.Write(append(bad, '\n'))
			fh.Close()
			if err != nil {
				return 0, err
			}
			total++
		}
		writeNLines(t, filepath.Join(dbDir, "comments.jsonl"), supplemental)
		return total + supplemental, nil
	}

	d.syncJsonlGitBackup()
	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("clean tick escalated: %+v", got)
	}
	beforeTick := backupSubjects(t, d, gitRepo)
	if first := beforeTick[0]; !strings.Contains(first, fmt.Sprintf(": hq=%d", cleanCount)) {
		t.Fatalf("clean tick subject = %q, want hq=%d", first, cleanCount)
	}

	// The same data plus one real issue row and one pollution bead the filter
	// removes. The filter leaves the committed export holding the extra real
	// row and no test bead, so the tick commits.
	realIssues = cleanIssues + 1
	polluted = true
	d.syncJsonlGitBackup()

	for _, e := range rec.Escalations() {
		if e.Escalation.Fingerprint == alertKeyJSONLSpike {
			t.Fatalf("filter tick halted on a spike: %s", e.Escalation.Reason)
		}
	}
	subjects := backupSubjects(t, d, gitRepo)
	if len(subjects) != len(beforeTick)+1 {
		t.Fatalf("filter tick made %d commit(s), want exactly 1 — with no commit the count metric goes unchecked", len(subjects)-len(beforeTick))
	}
	// The count is the export total minus the filtered bead: the pre-filter
	// total (31 issues + 70 supplemental) less the 1 removed. The issues.jsonl
	// line count left on disk is 31, and that is what the tick used to commit —
	// a 69% drop from the 100 baseline, which halted the backup.
	if got, want := subjects[0], fmt.Sprintf(": hq=%d", filteredCount); !strings.Contains(got, want) {
		t.Errorf("filter tick subject = %q, want %s — the exported total less the filtered bead, not the issues.jsonl line count", got, want)
	}
	data, err := os.ReadFile(filepath.Join(gitRepo, "hq", "issues.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), pollutedTitle) {
		t.Error("the pollution filter did not remove the test bead; the tick did not exercise the filter path")
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

// partialExportFaults decides what a tick's exports do: every database named
// here returns the deadline error, and the others export ten rows each. A
// caller flips the map between ticks to drive a failure, a recovery and a
// relapse through one daemon.
type partialExportFaults struct{ failing map[string]bool }

// partialExportDaemon wires a daemon over a fake remote with a seeded baseline
// and two databases, hq and gt.
func partialExportDaemon(t *testing.T, faults *partialExportFaults) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", seedSubjectPrefix+"hq=10 gt=10", map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq", "gt"}},
	}}
	d.jsonlExportFn = func(db, repo, dataDir string, scrub bool) (int, error) {
		if faults.failing[db] {
			return 0, errors.New("context deadline exceeded")
		}
		dbDir := filepath.Join(repo, db)
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return 0, err
		}
		writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 10)
		return 10, nil
	}
	return d, rec
}

// TestSyncJsonlGitBackupPartialFailureKeepsAlert: a database that fails every
// tick while another exports must keep the export alert raised, not clear it
// because exported > 0 — and the second failing tick must not raise it again.
// `gt escalate` records a repeat on the open escalation bead, a Dolt write and
// a subprocess per call, so a failure that stays broken would spend one every
// 15-minute tick (gt-ga9hx, gt-vwry).
func TestSyncJsonlGitBackupPartialFailureKeepsAlert(t *testing.T) {
	t.Parallel()
	faults := &partialExportFaults{failing: map[string]bool{"hq": true}}
	d, rec := partialExportDaemon(t, faults)

	d.syncJsonlGitBackup()
	d.syncJsonlGitBackup()

	open := rec.OpenEscalations(alertKeyJSONLExport)
	if len(open) != 1 {
		t.Fatalf("open %s escalations = %+v, want one", alertKeyJSONLExport, open)
	}
	if open[0].Occurrences != 1 {
		t.Errorf("escalation occurrences = %d after two ticks with the same failure, want 1", open[0].Occurrences)
	}
	listed, failed := exportAlertFailures(t, open[0].Reason)
	if !slices.Equal(listed, []string{"hq"}) {
		t.Errorf("escalation reason = %q lists %v as failing, want exactly [hq]", open[0].Reason, listed)
	}
	if want := map[string]string{"hq": "context deadline exceeded"}; !maps.Equal(failed, want) {
		t.Errorf("escalation failures = %v, want %v", failed, want)
	}
}

// TestSyncJsonlGitBackupAllExportedClearsAlert: a database that recovers clears
// the export alert — the failing tick has to raise it for this to be about the
// recovery path at all (gt-ga9hx) — and the same failure returning after that
// healthy tick is raised again, not matched against the alert the healthy tick
// took down.
func TestSyncJsonlGitBackupAllExportedClearsAlert(t *testing.T) {
	t.Parallel()
	faults := &partialExportFaults{failing: map[string]bool{"hq": true}}
	d, rec := partialExportDaemon(t, faults)

	d.syncJsonlGitBackup()
	if open := rec.OpenEscalations(alertKeyJSONLExport); len(open) != 1 {
		t.Fatalf("the failing tick left open escalations = %+v, want one for the healthy tick to clear", open)
	}

	delete(faults.failing, "hq")
	d.syncJsonlGitBackup()
	if open := rec.OpenEscalations(alertKeyJSONLExport); len(open) != 0 {
		t.Errorf("after an all-exported tick, open escalations = %+v, want none", open)
	}

	faults.failing["hq"] = true
	d.syncJsonlGitBackup()
	if open := rec.OpenEscalations(alertKeyJSONLExport); len(open) != 1 {
		t.Errorf("the failure returned and left open escalations = %+v, want one", open)
	}
}

// TestSyncJsonlGitBackupPartialFailureAfterAllFailedRaisesAgain: an all-failed
// cycle replaces the alert's text, so a partial failure that returns unchanged
// is raised again rather than matched against a detail that no longer describes
// what is failing (gt-ga9hx).
func TestSyncJsonlGitBackupPartialFailureAfterAllFailedRaisesAgain(t *testing.T) {
	t.Parallel()
	faults := &partialExportFaults{failing: map[string]bool{"hq": true}}
	d, rec := partialExportDaemon(t, faults)

	d.syncJsonlGitBackup() // partial: hq fails, gt exports
	faults.failing["gt"] = true
	d.syncJsonlGitBackup() // all fail: the alert's text is replaced
	delete(faults.failing, "gt")
	d.syncJsonlGitBackup() // the same partial failure comes back

	open := rec.OpenEscalations(alertKeyJSONLExport)
	if len(open) != 1 {
		t.Fatalf("open %s escalations = %+v, want one", alertKeyJSONLExport, open)
	}
	listed, _ := exportAlertFailures(t, open[0].Reason)
	if !slices.Equal(listed, []string{"hq"}) {
		t.Errorf("escalation reason = %q lists %v as failing, want the partial failure that is standing now", open[0].Reason, listed)
	}
}

// exportAlertFailures reads the databases a partial-failure export alert says
// are failing, and the error it names for each: the comma-separated list
// between "the offsite copy of " and " is stale" on its summary line, and one
// "<database>: <error>" line under it. Asserting on the names the alert
// carries, in both places it carries them, is what lets "the database that
// exported is not named as failing" fail when a reason names it — a phrase
// search for "gt:" passes on any rewording that drops the colon (gt-ga9hx).
func exportAlertFailures(t *testing.T, reason string) (listed []string, failed map[string]string) {
	t.Helper()
	summary, detail, ok := strings.Cut(reason, "\n")
	if !ok {
		t.Fatalf("escalation reason = %q, want a summary line and a line per failed database", reason)
	}
	_, rest, ok := strings.Cut(summary, "the offsite copy of ")
	if !ok {
		t.Fatalf("escalation reason = %q, want its summary to name the databases whose copy is stale", reason)
	}
	list, _, ok := strings.Cut(rest, " is stale")
	if !ok {
		t.Fatalf("escalation reason = %q, want its summary to close the list with \" is stale\"", reason)
	}
	listed = strings.Split(list, ", ")
	failed = map[string]string{}
	for _, line := range strings.Split(detail, "\n") {
		db, err, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("failure line %q, want \"<database>: <error>\"", line)
		}
		failed[db] = err
	}
	return listed, failed
}

// tableRowsFake returns a jsonlTableExportFn that writes rows[table] lines to
// {dir}/{table}.jsonl, and fails the tables in failing without touching their
// file, as exportTableToJsonl does when dolt errors or times out.
func tableRowsFake(t *testing.T, rows map[string]int, failing map[string]bool) func(string, beadsql.Query, string, string, time.Duration) (int, error) {
	t.Helper()
	return func(table string, _ beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
		if failing[table] {
			return 0, errors.New("signal: killed: context deadline exceeded")
		}
		writeNLines(t, filepath.Join(dir, table+".jsonl"), rows[table])
		return rows[table], nil
	}
}

// TestExportDatabaseToJsonlSupplementalFailureIsReported: a supplemental table
// whose export fails used to be logged and skipped, so the database exported
// "successfully" with the table's rows missing from the count. The database is
// now reported failed, and the other tables are still exported.
func TestExportDatabaseToJsonlSupplementalFailureIsReported(t *testing.T) {
	t.Parallel()
	rows := map[string]int{"issues": 30, "comments": 10, "events": 60}
	d, _ := backupTestDaemon(t)
	d.jsonlTableExportFn = tableRowsFake(t, rows, map[string]bool{"events": true})
	gitRepo := t.TempDir()

	n, err := d.exportDatabaseToJsonl("hq", gitRepo, t.TempDir(), false)
	if err == nil {
		t.Fatalf("exportDatabaseToJsonl = (%d, nil), want an error for the failed events table", n)
	}
	if !strings.Contains(err.Error(), "events") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %q, want the failed table and its cause", err)
	}
	// A failing optional table does not abort the others.
	for _, table := range []string{"issues", "comments"} {
		if _, statErr := os.Stat(filepath.Join(gitRepo, "hq", table+".jsonl")); statErr != nil {
			t.Errorf("%s was not exported after the events failure: %v", table, statErr)
		}
	}
}

// TestExportDatabaseToJsonlSupplementalSuccess pins the happy path next to the
// failure test: every table exporting returns the summed count and no error.
func TestExportDatabaseToJsonlSupplementalSuccess(t *testing.T) {
	t.Parallel()
	rows := map[string]int{"issues": 30, "comments": 10, "events": 60}
	d, _ := backupTestDaemon(t)
	d.jsonlTableExportFn = tableRowsFake(t, rows, nil)

	n, err := d.exportDatabaseToJsonl("hq", t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatalf("exportDatabaseToJsonl: %v", err)
	}
	if want := 30 + 10 + 60; n != want {
		t.Errorf("total = %d, want %d", n, want)
	}
}

// TestExportDatabaseToJsonlSupplementalBudgetIsPerDatabase: the six
// supplemental tables share one bound per database, so a dolt that stops
// answering costs the cycle that bound and no more. Before the fix each table
// had its own timeout, and such a dolt held one database — and the cycle's
// doltMaintMu read lock — for half an hour (gt-4nqej). Every table still gets
// its floor, so each is attempted, but together they cannot outrun the bound.
func TestExportDatabaseToJsonlSupplementalBudgetIsPerDatabase(t *testing.T) {
	t.Parallel()
	const budget = time.Minute
	d, _ := backupTestDaemon(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	d.clock = clk
	d.jsonlSupplementalTimeout = budget
	var attempted []string
	var stalled time.Duration
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _ string, _ string, got time.Duration) (int, error) {
		if got <= 0 {
			t.Errorf("%s exported with a spent budget (%s)", table, got)
		}
		if table == "issues" {
			return 0, nil
		}
		attempted = append(attempted, table)
		// Every table stalls for its whole budget, the way a dolt that stops
		// answering does.
		clk.Advance(got)
		stalled += got
		return 0, errors.New("signal: killed: context deadline exceeded")
	}

	n, err := d.exportDatabaseToJsonl("hq", t.TempDir(), t.TempDir(), false)
	if err == nil {
		t.Fatalf("exportDatabaseToJsonl = (%d, nil), want an error for the tables that stalled", n)
	}
	if stalled > budget {
		t.Errorf("the tables held the export for %s, past the %s budget shared by the database", stalled, budget)
	}
	if !slices.Equal(attempted, supplementalTables) {
		t.Errorf("export attempted %v, want every table %v", attempted, supplementalTables)
	}
}

// TestExportDatabaseToJsonlSlowTableLeavesTheRestTheirFloor: a table that is
// slow but succeeds must not spend the whole budget and leave the later tables
// unattempted — cycle after cycle, since the order never changes and the slow
// table is always the same one. Each table still to come keeps a floor.
func TestExportDatabaseToJsonlSlowTableLeavesTheRestTheirFloor(t *testing.T) {
	t.Parallel()
	const budget = time.Minute
	d, _ := backupTestDaemon(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	d.clock = clk
	d.jsonlSupplementalTimeout = budget
	var attempted []string
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _ string, _ string, got time.Duration) (int, error) {
		if table == "issues" {
			return 1, nil
		}
		attempted = append(attempted, table)
		if got <= 0 {
			t.Errorf("%s exported with a spent budget (%s)", table, got)
		}
		if table == supplementalTables[0] {
			// Succeeds, but only after using everything it was given.
			clk.Advance(got)
		}
		return 1, nil
	}

	n, err := d.exportDatabaseToJsonl("hq", t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatalf("exportDatabaseToJsonl: %v — the tables after a slow success were starved", err)
	}
	if want := 1 + len(supplementalTables); n != want {
		t.Errorf("total = %d, want %d", n, want)
	}
	if !slices.Equal(attempted, supplementalTables) {
		t.Errorf("export attempted %v, want every table %v", attempted, supplementalTables)
	}
}

// TestExportDatabaseToJsonlIssuesFailureIsReported: the issues export is the
// function's other export call, and its failure aborts the database before any
// supplemental table is written from a snapshot the issues export did not
// complete.
func TestExportDatabaseToJsonlIssuesFailureIsReported(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.jsonlTableExportFn = tableRowsFake(t, map[string]int{"comments": 10}, map[string]bool{"issues": true})
	gitRepo := t.TempDir()

	n, err := d.exportDatabaseToJsonl("hq", gitRepo, t.TempDir(), false)
	if err == nil {
		t.Fatalf("exportDatabaseToJsonl = (%d, nil), want an error for the failed issues table", n)
	}
	if !strings.Contains(err.Error(), "issues") {
		t.Errorf("error = %q, want the failed table named", err)
	}
	if _, statErr := os.Stat(filepath.Join(gitRepo, "hq", "comments.jsonl")); statErr == nil {
		t.Error("comments.jsonl was written after the issues export failed")
	}
}

// TestSyncJsonlGitBackupSupplementalFailureNoFalseSpike: a tick whose events
// export fails raises the export alert, and the rows it lost do not reach the
// spike guard as "possible data loss".
func TestSyncJsonlGitBackupSupplementalFailureNoFalseSpike(t *testing.T) {
	t.Parallel()
	const baseline = 100 // 30 issues + 10 comments + 60 events
	rows := map[string]int{"issues": 30, "comments": 10, "events": 60}
	failing := map[string]bool{"events": true}

	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", fmt.Sprintf("%shq=%d", seedSubjectPrefix, baseline), map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq", "gt"}},
	}}
	// gt always exports, so the cycle is a partial failure and reaches the
	// spike guard; hq loses its events table.
	d.jsonlTableExportFn = tableRowsFake(t, rows, failing)

	d.syncJsonlGitBackup()

	if open := rec.OpenEscalations(alertKeyJSONLSpike); len(open) != 0 {
		t.Errorf("a failed events export escalated as a spike: %+v", open)
	}
	open := rec.OpenEscalations(alertKeyJSONLExport)
	if len(open) != 1 {
		t.Fatalf("open %s escalations = %+v, want one", alertKeyJSONLExport, open)
	}
	if !strings.Contains(open[0].Reason, "hq") || !strings.Contains(open[0].Reason, "events") {
		t.Errorf("escalation reason = %q, want the database and the failed table", open[0].Reason)
	}
	// The undercount must not become a committed baseline either.
	for _, s := range backupSubjects(t, d, gitRepo) {
		if strings.Contains(s, ": hq=") && !strings.Contains(s, fmt.Sprintf(": hq=%d", baseline)) {
			t.Errorf("commit subject %q records a shrunken hq count", s)
		}
	}
}

// TestSyncJsonlGitBackupSupplementalFailureDoesNotCommitFreshIssues: a database
// whose events export fails has no count, so the spike guard skips it. Its
// issues.jsonl is already rewritten, so a mass drop there (3 rows against a
// baseline of 1000) must not be committed under the [FAILED: hq] subject — and
// the export the failed database does have committed stays in the tree, so the
// last verified copy survives the tick.
func TestSyncJsonlGitBackupSupplementalFailureDoesNotCommitFreshIssues(t *testing.T) {
	t.Parallel()
	const baseline = 1000

	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	// The seed commit is the last verified backup: hq's export at the count its
	// subject records, which the tick below must leave in the tree.
	verified := nLines(baseline)
	f.Commit(t, remote, "main", fmt.Sprintf("%shq=%d", seedSubjectPrefix, baseline),
		map[string]string{"README": "init\n", "hq/issues.jsonl": verified})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	d.config = &Config{TownRoot: townRoot}
	d.notifier = notifyfake.New()
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq", "gt"}},
	}}
	// hq's issues export is a 3-row mass drop and its events export fails; gt
	// exports fully, so the cycle is a partial failure that reaches the commit.
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
		if filepath.Base(dir) == "hq" && table == "events" {
			return 0, errors.New("signal: killed: context deadline exceeded")
		}
		n := 30
		if filepath.Base(dir) == "hq" && table == "issues" {
			n = 3
		}
		writeNLines(t, filepath.Join(dir, table+".jsonl"), n)
		return n, nil
	}

	before := f.Ref(gitRepo, "refs/heads/main")
	d.syncJsonlGitBackup()
	after := f.Ref(gitRepo, "refs/heads/main")

	if after == before {
		t.Fatal("no commit was made: gt exported fully and its files should have been committed")
	}
	// hq's committed issues.jsonl is the seed's, unchanged: the exact text is
	// what fails when the fresh 3-row export is committed, and what fails when
	// the seeded copy is dropped — "no hq/ path in the tree" reads as a pass on
	// both.
	tree := f.Tree(after)
	if got := tree["hq/issues.jsonl"]; got != verified {
		t.Errorf("hq/issues.jsonl in the new commit = %d bytes, want the %d byte last verified export", len(got), len(verified))
	}
	var gt int
	for path := range tree {
		switch {
		case strings.HasPrefix(path, "gt/"):
			gt++
		case strings.HasPrefix(path, "hq/") && path != "hq/issues.jsonl":
			t.Errorf("the new HEAD commit carries %s from the failed database", path)
		}
	}
	if gt == 0 {
		t.Error("the new HEAD commit carries no gt/ file, want the fully exported database committed")
	}
	if subjects := backupSubjects(t, d, gitRepo); len(subjects) == 0 || !strings.Contains(subjects[0], "[FAILED: hq]") {
		t.Errorf("subjects = %v, want the newest to keep the [FAILED: hq] marker", subjects)
	}
}

// TestSyncJsonlGitBackupIssuesFailureIsReported: the cycle is the export's
// other caller, and a database whose issues export fails reaches it as a failed
// database — nothing of that database is committed, and the export alert names
// it.
func TestSyncJsonlGitBackupIssuesFailureIsReported(t *testing.T) {
	t.Parallel()

	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", seedSubjectPrefix+"hq=100", map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq", "gt"}},
	}}
	// hq's issues export fails; gt exports fully, so the cycle still commits.
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
		if filepath.Base(dir) == "hq" && table == "issues" {
			return 0, errors.New("signal: killed: context deadline exceeded")
		}
		writeNLines(t, filepath.Join(dir, table+".jsonl"), 30)
		return 30, nil
	}

	before := f.Ref(gitRepo, "refs/heads/main")
	d.syncJsonlGitBackup()
	after := f.Ref(gitRepo, "refs/heads/main")

	if after == before {
		t.Fatal("no commit was made: gt exported fully and its files should have been committed")
	}
	for path := range f.Tree(after) {
		if strings.HasPrefix(path, "hq/") {
			t.Errorf("the new HEAD commit carries %s from the failed database", path)
		}
	}
	open := rec.OpenEscalations(alertKeyJSONLExport)
	if len(open) != 1 {
		t.Fatalf("open %s escalations = %+v, want one", alertKeyJSONLExport, open)
	}
	if !strings.Contains(open[0].Reason, "hq") || !strings.Contains(open[0].Reason, "issues") {
		t.Errorf("escalation reason = %q, want the database and the failed table", open[0].Reason)
	}
}

func TestParseCommitVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject string
		want    int
	}{
		{"backup 2026-01-02 09:00 m2: hq=1271 beads=42", 2},
		{"backup 2026-01-02 09:00 m12: hq=1271", 12},
		{"backup 2026-01-02 09:00 m3: hq=1271 [FAILED: m2]", 3},
		// No token: history from before versioning reads as version 1.
		{"backup 2026-01-02 09:00: hq=1271 beads=42", 1},
		{"backup 2026-01-02 09:00: hq=1271 [FAILED: m2:]", 1},
		// A database called m2 is a count pair, not a version token.
		{"backup 2026-01-02 09:00: m2=5", 1},
		{"backup 2026-01-02 09:00 m0: hq=1", 1},
		{"init", 1},
		{"", 1},
	}
	for _, tt := range tests {
		if got := parseCommitVersion(tt.subject); got != tt.want {
			t.Errorf("parseCommitVersion(%q) = %d, want %d", tt.subject, got, tt.want)
		}
		// The token never disturbs the count pairs.
		if tt.subject == "backup 2026-01-02 09:00 m2: hq=1271 beads=42" {
			got := parseCommitCounts(tt.subject)
			if len(got) != 2 || got["hq"] != 1271 || got["beads"] != 42 {
				t.Errorf("parseCommitCounts(%q) = %v", tt.subject, got)
			}
		}
	}
}

// metricChangeDaemon wires a daemon over a fake remote whose only history is
// one backup commit with the given subject. hq exports as many lines as
// *exportLines holds. logs collects the daemon's log.
func metricChangeDaemon(t *testing.T, seedSubject string, exportLines *int, logs *strings.Builder) (*Daemon, *notifyfake.Recorder, string) {
	t.Helper()
	d, f := backupTestDaemon(t)
	d.logger = log.New(logs, "", 0)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", seedSubject, map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq"}},
	}}
	d.jsonlExportFn = func(db, repo, dataDir string, scrub bool) (int, error) {
		dbDir := filepath.Join(repo, db)
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return 0, err
		}
		writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), *exportLines)
		return *exportLines, nil
	}
	return d, rec, gitRepo
}

// TestSyncJsonlGitBackupMetricChangeRebaselinesOnce: history counted under
// version 1 (no token) holds nothing comparable to a run counting under the
// current version. The run commits its counts under the new token with one log
// line and no escalation, and the tick after finds same-version history, so it
// neither logs again nor skips the spike check (gt-26qqh).
func TestSyncJsonlGitBackupMetricChangeRebaselinesOnce(t *testing.T) {
	t.Parallel()
	lines := 5000 // 4900% over the 100 the old metric recorded
	var logs strings.Builder
	d, rec, gitRepo := metricChangeDaemon(t, "backup 2026-01-01 00:00: hq=100", &lines, &logs)

	d.syncJsonlGitBackup()

	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("a metric change escalated: %+v", got)
	}
	subjects := backupSubjects(t, d, gitRepo)
	if len(subjects) != 2 {
		t.Fatalf("subjects = %q, want the seed plus one rebaseline commit", subjects)
	}
	if want := fmt.Sprintf(" m%d: hq=5000", countMetricVersion); !strings.Contains(subjects[0], want) {
		t.Errorf("rebaseline commit subject = %q, want it to contain %q", subjects[0], want)
	}
	logLine := fmt.Sprintf("count metric changed from m1 to m%d", countMetricVersion)
	if n := strings.Count(logs.String(), logLine); n != 1 {
		t.Errorf("rebaseline logged %d time(s), want once; log:\n%s", n, logs.String())
	}

	// The next tick compares the current version against itself: an ordinary
	// change commits and does not halt.
	lines = 5010
	d.syncJsonlGitBackup()
	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("the tick after the rebaseline escalated: %+v", got)
	}
	if subjects := backupSubjects(t, d, gitRepo); len(subjects) != 3 {
		t.Fatalf("subjects = %q, want the tick after the rebaseline to commit", subjects)
	}

	// A genuine spike within the version halts again: the rebaseline is a
	// one-off, not a disabled check.
	lines = 50
	d.syncJsonlGitBackup()
	if n := strings.Count(logs.String(), logLine); n != 1 {
		t.Errorf("second tick logged the rebaseline again (%d lines)", n)
	}
	if open := rec.OpenEscalations(alertKeyJSONLSpike); len(open) != 1 {
		t.Errorf("second tick open spike escalations = %+v, want one for the 5000 to 50 drop", open)
	}
}

// TestSyncJsonlGitBackupSpikeWithinVersionStillHalts: history under the
// current version is compared as before, so a jump past the threshold halts the
// backup with no commit.
func TestSyncJsonlGitBackupSpikeWithinVersionStillHalts(t *testing.T) {
	t.Parallel()
	lines := 5000
	var logs strings.Builder
	d, rec, gitRepo := metricChangeDaemon(t, seedSubjectPrefix+"hq=100", &lines, &logs)

	d.syncJsonlGitBackup()

	open := rec.OpenEscalations(alertKeyJSONLSpike)
	if len(open) != 1 {
		t.Fatalf("open spike escalations = %+v, want one", open)
	}
	// A halted tick commits nothing, so the next tick derives the level the
	// report names and halts again — every tick, until a newer count-bearing
	// commit exists. The escalation is where the operator reads how to land one
	// (gt-ttqq8).
	for _, want := range []string{gitRepo, "commit --allow-empty", fmt.Sprintf(" m%d: ", countMetricVersion)} {
		if !strings.Contains(open[0].Reason, want) {
			t.Errorf("the halt escalation must name the way to accept the export, missing %q:\n%s", want, open[0].Reason)
		}
	}
	if subjects := backupSubjects(t, d, gitRepo); len(subjects) != 1 {
		t.Errorf("a halted tick committed: %q", subjects)
	}
	if strings.Contains(logs.String(), "count metric changed") {
		t.Errorf("a same-version spike was read as a metric change; log:\n%s", logs.String())
	}
}

// TestRecomputeSpikeBaseline_LegacySubjectsAreVersionOne: a subject without the
// token reads as version 1, so history from before versioning parses unchanged
// and the baseline reports the version it came from.
func TestRecomputeSpikeBaseline_LegacySubjectsAreVersionOne(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "hq"), 0755)
	writeNLines(t, filepath.Join(gitRepo, "hq", "issues.jsonl"), 100)
	commitAll(t, d, gitRepo, "backup 2026-01-01 00:00: hq=100")

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sb.Counts) != 0 || sb.Rebaselined["hq"] != 1 {
		t.Fatalf("baseline = counts %v, rebaselined %v; want no window and hq rebaselined from m1", sb.Counts, sb.Rebaselined)
	}
	if got := parseCommitCounts("backup 2026-01-01 00:00: hq=100"); got["hq"] != 100 {
		t.Errorf("legacy subject counts = %v, want hq=100", got)
	}

	// A same-version commit above the legacy one becomes the whole window.
	writeNLines(t, filepath.Join(gitRepo, "hq", "issues.jsonl"), 7000)
	commitAll(t, d, gitRepo, seedSubjectPrefix+"hq=7000")
	sb, err = d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries := sb.Counts["hq"]; len(entries) != 1 || entries[0].N != 7000 {
		t.Errorf("window = %+v, want only the same-version 7000", entries)
	}
	if len(sb.Rebaselined) != 0 {
		t.Errorf("rebaselined = %v, want none once hq has same-version history", sb.Rebaselined)
	}
}

// TestVerifyExportCounts_RebaselinesOnlyTheChangedDatabase: gt was last counted
// under version 1 while hq has same-version history. gt's count has nothing
// comparable and is rebaselined, with a log line naming it; hq keeps its spike
// check, so a jump in hq still halts (gt-7p9aj).
func TestVerifyExportCounts_RebaselinesOnlyTheChangedDatabase(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	os.MkdirAll(filepath.Join(gitRepo, "hq"), 0755)
	writeNLines(t, filepath.Join(gitRepo, "hq", "issues.jsonl"), 100)
	commitAll(t, d, gitRepo, "backup 2026-01-01 00:00: hq=100 gt=50")
	writeNLines(t, filepath.Join(gitRepo, "hq", "issues.jsonl"), 110)
	commitAll(t, d, gitRepo, seedSubjectPrefix+"hq=110 [FAILED: gt]")

	spikes, err := d.verifyExportCounts(gitRepo, []string{"hq", "gt"}, map[string]int{"hq": 5000, "gt": 9000}, 0.20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spikes) != 1 || spikes[0].DB != "hq" {
		t.Errorf("spikes = %+v, want only hq's jump to be checked and flagged", spikes)
	}
	want := fmt.Sprintf("gt: count metric changed from m1 to m%d", countMetricVersion)
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
	if strings.Contains(logs.String(), "hq: count metric changed") {
		t.Errorf("hq has same-version history and was rebaselined:\n%s", logs.String())
	}
}

// TestRecomputeSpikeBaseline_VersionedCacheIsTrusted: a cache written under the
// current version seeds the window as before, with nothing rebaselined, so the
// cache path rebaselines only a version mismatch (gt-7p9aj).
func TestRecomputeSpikeBaseline_VersionedCacheIsTrusted(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	cache := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: time.Now().Format(time.RFC3339),
		Counts: map[string][]spikeCommit{
			"hq": {{Db: "hq", N: 100, Age: 0, Source: spikeSourceCommit, Version: countMetricVersion}},
		},
	}
	if err := saveSpikeBaselineHistory(gitRepo, cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n, ok := spikeBaselineCount(sb, "hq"); !ok || n != 100 {
		t.Errorf("baseline hq = %d, %v; want the cached 100", n, ok)
	}
	if len(sb.Rebaselined) != 0 {
		t.Errorf("rebaselined = %v, want none for a current-version cache", sb.Rebaselined)
	}
}

// TestRecomputeSpikeBaseline_MixedVersionCacheRebaselinesStaleDatabaseOnly: hq's
// cached level is current and gt's predates versioning. The cache keeps hq and
// rebaselines gt alone, instead of trusting gt's level or discarding hq's
// (gt-7p9aj).
func TestRecomputeSpikeBaseline_MixedVersionCacheRebaselinesStaleDatabaseOnly(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	cache := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: time.Now().Format(time.RFC3339),
		Counts: map[string][]spikeCommit{
			"hq": {{Db: "hq", N: 100, Age: 0, Source: spikeSourceCommit, Version: countMetricVersion}},
			"gt": {{Db: "gt", N: 50, Age: 0, Source: spikeSourceCommit}},
		},
	}
	if err := saveSpikeBaselineHistory(gitRepo, cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n, ok := spikeBaselineCount(sb, "hq"); !ok || n != 100 {
		t.Errorf("baseline hq = %d, %v; want the cached 100", n, ok)
	}
	if _, ok := spikeBaselineCount(sb, "gt"); ok {
		t.Errorf("baseline kept gt's version-1 level: %+v", sb.Counts["gt"])
	}
	if sb.Rebaselined["gt"] != legacyCountMetricVersion || len(sb.Rebaselined) != 1 {
		t.Errorf("rebaselined = %v, want only gt from m%d", sb.Rebaselined, legacyCountMetricVersion)
	}
}

// TestRecomputeSpikeBaseline_UnversionedCacheIsVersionOne: a cache written
// before versioning omits the field and reads as version 1, so it rebaselines
// instead of seeding a window the new count cannot be compared against.
func TestRecomputeSpikeBaseline_UnversionedCacheIsVersionOne(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	initGitRepo(t, d, gitRepo)
	cache := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: time.Now().Format(time.RFC3339),
		Counts:   map[string][]spikeCommit{"hq": {{Db: "hq", N: 100, Age: 0}}},
	}
	if err := saveSpikeBaselineHistory(gitRepo, cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	sb, err := d.recomputeSpikeBaseline(gitRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := spikeBaselineCount(sb, "hq"); ok || sb.Rebaselined["hq"] != legacyCountMetricVersion {
		t.Fatalf("baseline = counts %v, rebaselined %v; want hq rebaselined from m%d", sb.Counts, sb.Rebaselined, legacyCountMetricVersion)
	}
}
