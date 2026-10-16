package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// syntheticRows builds n rows of about rowBytes each, numbered so a test can
// check the order survives the cut into shards.
func syntheticRows(n, rowBytes int) []json.RawMessage {
	rows := make([]json.RawMessage, n)
	for i := range rows {
		rows[i], _ = json.Marshal(map[string]any{
			"id":      fmt.Sprintf("row-%d", i),
			"payload": strings.Repeat("x", rowBytes),
		})
	}
	return rows
}

func shardNames(t *testing.T, dir, table string) []string {
	t.Helper()
	paths, err := jsonlShardPaths(dir, table)
	if err != nil {
		t.Fatalf("jsonlShardPaths: %v", err)
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}

func TestWriteTableShards_CutsLargeTableIntoFilesUnderTheLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const maxBytes = 1000
	rows := syntheticRows(25, 200) // about 230 bytes a line: four lines to a shard

	n, err := writeTableShards(dir, "events", rows, maxBytes)
	if err != nil {
		t.Fatalf("writeTableShards: %v", err)
	}
	if n != len(rows) {
		t.Errorf("rows written = %d, want %d", n, len(rows))
	}

	names := shardNames(t, dir, "events")
	if len(names) < 2 {
		t.Fatalf("shards = %v, want the table cut into several files", names)
	}
	for i, name := range names {
		if want := jsonlShardName("events", i+1); name != want {
			t.Errorf("shard %d = %s, want %s", i+1, name, want)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxBytes {
			t.Errorf("%s is %d bytes, over the %d byte cut", name, info.Size(), maxBytes)
		}
	}

	// Concatenated in shard order the files are the rows, in order, once each.
	var ids []string
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			var row struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("%s holds a line that is not a row: %v", name, err)
			}
			ids = append(ids, row.ID)
		}
	}
	if len(ids) != len(rows) {
		t.Fatalf("shards hold %d rows, want %d", len(ids), len(rows))
	}
	for i, id := range ids {
		if want := fmt.Sprintf("row-%d", i); id != want {
			t.Fatalf("row %d = %s, want %s", i, id, want)
		}
	}
}

func TestWriteTableShards_SmallTableKeepsTheUnshardedName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := writeTableShards(dir, "labels", syntheticRows(3, 10), jsonlShardMaxBytes); err != nil {
		t.Fatalf("writeTableShards: %v", err)
	}
	if got := shardNames(t, dir, "labels"); len(got) != 1 || got[0] != "labels.jsonl" {
		t.Errorf("shards = %v, want [labels.jsonl]", got)
	}
}

func TestWriteTableShards_EmptyTableWritesAnEmptyFirstShard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	n, err := writeTableShards(dir, "labels", nil, 1000)
	if err != nil || n != 0 {
		t.Fatalf("writeTableShards = (%d, %v), want (0, nil)", n, err)
	}
	if got := shardNames(t, dir, "labels"); len(got) != 1 || got[0] != "labels.jsonl" {
		t.Errorf("shards = %v, want [labels.jsonl]", got)
	}
}

func TestWriteTableShards_RowLargerThanTheCutGetsItsOwnShard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rows := append(syntheticRows(1, 10), syntheticRows(1, 5000)...)
	rows = append(rows, syntheticRows(1, 10)...)

	if _, err := writeTableShards(dir, "issues", rows, 1000); err != nil {
		t.Fatalf("writeTableShards: %v", err)
	}
	if got := shardNames(t, dir, "issues"); len(got) != 3 {
		t.Errorf("shards = %v, want the big row alone between two small ones", got)
	}
}

func TestWriteTableShards_ReexportRemovesStaleShards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := writeTableShards(dir, "events", syntheticRows(40, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardNames(t, dir, "events")
	if len(before) < 4 {
		t.Fatalf("first export shards = %v, want at least four", before)
	}

	// Files that merely look like a shard of another name stay.
	keep := []string{"issues.2.jsonl", "events.x.jsonl", "events.02.jsonl", "events_3.jsonl", "eventsx.2.jsonl"}
	for _, name := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writeTableShards(dir, "events", syntheticRows(6, 200), 1000); err != nil {
		t.Fatalf("second export: %v", err)
	}
	after := shardNames(t, dir, "events")
	if len(after) >= len(before) || len(after) < 1 {
		t.Errorf("shards after a shorter export = %v, want fewer than %v", after, before)
	}
	for _, name := range before[len(after):] {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("stale shard %s is still in the export (stat err %v)", name, err)
		}
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is not a shard of events and was removed: %v", name, err)
		}
	}
	if err := verifyJsonlShards(dir, "events", 6, jsonlShardHardLimit); err != nil {
		t.Errorf("verify after the shorter export: %v", err)
	}
}

func TestWriteTableShards_LeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("writeTableShards: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func TestVerifyJsonlShards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("writeTableShards: %v", err)
	}

	if err := verifyJsonlShards(dir, "events", 20, 1100); err != nil {
		t.Errorf("a whole export failed verification: %v", err)
	}
	// The count is the sum across shards, not the first file's.
	if err := verifyJsonlShards(dir, "events", 21, 1100); err == nil || !strings.Contains(err.Error(), "hold 20 rows, exported 21") {
		t.Errorf("a row-count mismatch passed verification: %v", err)
	}
	if err := verifyJsonlShards(dir, "events", 20, 500); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("a shard over the file limit passed verification: %v", err)
	}
	// A lost shard is a lost slice of the table.
	if err := os.Remove(filepath.Join(dir, "events.2.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := verifyJsonlShards(dir, "events", 20, 1100); err == nil {
		t.Error("an export missing a shard passed verification")
	}
}

func TestPollutionFilterAndVerifyCoverEveryIssuesShard(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dbDir, name), []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"id":"hq-abc","title":"real work"}`
	polluted := `{"id":"hq-xyz","title":"Test issue 7"}`
	write("issues.jsonl", good, polluted)
	write("issues.2.jsonl", polluted, good, polluted)

	if got := d.verifyNoPollution(gitRepo, []string{"hq"}); got != 3 {
		t.Errorf("verifyNoPollution = %d, want 3 across both shards", got)
	}
	removed := d.applyPollutionFilter(gitRepo, []string{"hq"})
	if removed["hq"] != 3 {
		t.Errorf("removed = %v, want 3 for hq across both shards", removed)
	}
	if got := d.verifyNoPollution(gitRepo, []string{"hq"}); got != 0 {
		t.Errorf("verifyNoPollution after the filter = %d, want 0", got)
	}
	for _, name := range []string{"issues.jsonl", "issues.2.jsonl"} {
		data, err := os.ReadFile(filepath.Join(dbDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != good+"\n" {
			t.Errorf("%s = %q, want only the real record", name, data)
		}
	}
}

// TestSyncJsonlGitBackupCountsAndCommitsEveryShard drives a cycle whose events
// table exports as several shards: the commit subject records the rows summed
// across them, so the spike guard compares like with like, and every shard
// reaches the commit.
func TestSyncJsonlGitBackupCountsAndCommitsEveryShard(t *testing.T) {
	t.Parallel()
	const issues, comments, events = 30, 10, 60
	// 60 rows of about 230 bytes cut at 3000 bytes: several shards.
	eventRows := syntheticRows(events, 200)

	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", fmt.Sprintf("%shq=%d", seedSubjectPrefix, issues+comments+events), map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq"}},
	}}
	rows := map[string]int{"issues": issues, "comments": comments}
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, dir, _ string) (int, error) {
		if table == "events" {
			return writeTableShards(dir, table, eventRows, 3000)
		}
		writeNLines(t, filepath.Join(dir, table+".jsonl"), rows[table])
		return rows[table], nil
	}

	before := f.Ref(gitRepo, "refs/heads/main")
	d.syncJsonlGitBackup()
	after := f.Ref(gitRepo, "refs/heads/main")

	if after == before {
		t.Fatal("no commit was made")
	}
	if open := rec.OpenEscalations(alertKeyJSONLSpike); len(open) != 0 {
		t.Errorf("sharded events escalated as a spike: %+v", open)
	}
	if subjects := backupSubjects(t, d, gitRepo); len(subjects) == 0 || !strings.Contains(subjects[0], fmt.Sprintf(": hq=%d", issues+comments+events)) {
		t.Errorf("commit subject %v, want hq=%d summed across every shard", subjects, issues+comments+events)
	}
	var shards int
	for path := range f.Tree(after) {
		if strings.HasPrefix(path, "hq/events") {
			shards++
		}
	}
	if shards < 2 {
		t.Errorf("commit holds %d events file(s), want the shards", shards)
	}
}
