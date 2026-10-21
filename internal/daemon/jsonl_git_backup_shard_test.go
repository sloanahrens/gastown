package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

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

// shardContents reads a table's export out of dir, keyed by file name, so a
// test can hold an export and compare it with the one a later swap leaves.
func shardContents(t *testing.T, dir, table string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	for _, name := range shardNames(t, dir, table) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		files[name] = string(data)
	}
	return files
}

// assertNoShardLitter fails on a .tmp or .bak left in dir: whatever path the
// swap took out, the directory holds the table's shards and nothing else.
func assertNoShardLitter(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".tmp"), strings.HasSuffix(e.Name(), ".bak"):
			t.Errorf("the swap left %s behind", e.Name())
		}
	}
}

// shardOpsWith is the swap's real filesystem work with the calls a test passes
// replaced (nil keeps the real one), for driving a swap through a failure no
// real directory can be made to produce.
func shardOpsWith(rename func(oldpath, newpath string) error, remove func(path string) error, verify func(dir, table string, wantRows int, maxFileBytes int64) error) jsonlShardOps {
	ops := osShardOps()
	if rename != nil {
		ops.rename = rename
	}
	if remove != nil {
		ops.remove = remove
	}
	if verify != nil {
		ops.verify = verify
	}
	return ops
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
		// The shards the swap moves aside are gone once the new export is in
		// place; a .bak that outlives its swap rides into the commit.
		if strings.HasSuffix(e.Name(), ".bak") {
			t.Errorf("shard backup %s left behind", e.Name())
		}
	}
}

// A swap a crash cut short leaves .bak files behind. The next export rewrites
// the table whole, so they go before anything new is published (gt-fhcwe).
func TestWriteTableShards_ClearsBackupsFromAnInterruptedSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	otherTable := "issues.2.jsonl.bak"
	for _, name := range []string{"events.jsonl.bak", "events.7.jsonl.bak", otherTable} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("second export: %v", err)
	}
	for _, name := range []string{"events.jsonl.bak", "events.7.jsonl.bak"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the next export (stat err %v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, otherTable)); err != nil {
		t.Errorf("%s belongs to another table and was removed: %v", otherTable, err)
	}
	if err := verifyJsonlShards(dir, "events", 20, jsonlShardHardLimit); err != nil {
		t.Errorf("verify after the cleanup: %v", err)
	}
}

// A swap whose publish fails part way through puts the export that was there
// back: the directory never keeps a shard from each generation (gt-fhcwe).
func TestWriteTableShards_PublishFailurePutsTheOldExportBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardContents(t, dir, "events")
	if len(before) < 2 {
		t.Fatalf("the first export wrote %d shard(s), want several for the swap to publish", len(before))
	}

	// The second shard's publish fails, after the first one is in place.
	ops := shardOpsWith(func(oldpath, newpath string) error {
		if filepath.Base(newpath) == "events.2.jsonl" && strings.HasSuffix(oldpath, ".tmp") {
			return errors.New("rename refused")
		}
		return os.Rename(oldpath, newpath)
	}, nil, nil)

	if _, err := writeTableShardsWith(ops, dir, "events", syntheticRows(12, 200), 1000); err == nil {
		t.Fatal("a swap whose publish failed reported success")
	}
	if after := shardContents(t, dir, "events"); !maps.Equal(before, after) {
		t.Errorf("the failed swap left %v, want the export that was there: %v", after, before)
	}
	assertNoShardLitter(t, dir)
}

// A swap that fails while moving the old export aside puts back what it already
// moved, so the failure leaves the export whole (gt-fhcwe).
func TestWriteTableShards_SetAsideFailurePutsBackWhatItMoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardContents(t, dir, "events")
	if len(before) < 2 {
		t.Fatalf("the first export wrote %d shard(s), want several for the swap to move", len(before))
	}

	// The second shard cannot be set aside, after the first one was.
	ops := shardOpsWith(func(oldpath, newpath string) error {
		if filepath.Base(oldpath) == "events.2.jsonl" && strings.HasSuffix(newpath, ".bak") {
			return errors.New("rename refused")
		}
		return os.Rename(oldpath, newpath)
	}, nil, nil)

	if _, err := writeTableShardsWith(ops, dir, "events", syntheticRows(12, 200), 1000); err == nil {
		t.Fatal("a swap that failed while setting the old export aside reported success")
	}
	if after := shardContents(t, dir, "events"); !maps.Equal(before, after) {
		t.Errorf("the failed swap left %v, want the export that was there: %v", after, before)
	}
	assertNoShardLitter(t, dir)
}

// A set-aside file that will not go leaves the directory holding two
// generations of the export, so the new one is withdrawn rather than published
// beside it (gt-fhcwe).
func TestWriteTableShards_UnremovableBackupWithdrawsTheNewExport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardContents(t, dir, "events")

	ops := shardOpsWith(nil, func(path string) error {
		if strings.HasSuffix(path, ".bak") {
			return errors.New("remove refused")
		}
		return os.Remove(path)
	}, nil)

	_, err := writeTableShardsWith(ops, dir, "events", syntheticRows(12, 200), 1000)
	if err == nil || !strings.Contains(err.Error(), "removing the stale events shards") {
		t.Fatalf("err = %v, want a failed swap naming the stale shards", err)
	}
	if after := shardContents(t, dir, "events"); !maps.Equal(before, after) {
		t.Errorf("the failed swap left %v, want the export that was there: %v", after, before)
	}
	assertNoShardLitter(t, dir)
}

// A temp an earlier cycle left that will not go withdraws the new export, like
// the set-aside shard it shares a stale tail with: the swap reports a failure
// it took back, not an export that landed (gt-27gnb).
func TestWriteTableShards_UnremovableTempWithdrawsTheNewExport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardContents(t, dir, "events")

	// A temp past the last shard this export writes, so the swap steps over it.
	if err := os.WriteFile(filepath.Join(dir, "events.7.jsonl.tmp"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ops := osShardOps()
	ops.removeAll = func(path string) error {
		if strings.HasSuffix(path, ".jsonl.tmp") {
			return errors.New("remove refused")
		}
		return os.RemoveAll(path)
	}

	_, err := writeTableShardsWith(ops, dir, "events", syntheticRows(12, 200), 1000)
	if err == nil || !strings.Contains(err.Error(), "clearing events temp files") {
		t.Fatalf("err = %v, want a failed swap naming the temp files", err)
	}
	if after := shardContents(t, dir, "events"); !maps.Equal(before, after) {
		t.Errorf("the failed swap left %v, want the export that was there: %v", after, before)
	}
}

// An export that fails verification does not replace the last one that
// verified; with none to put back it leaves nothing at all (gt-fhcwe).
func TestWriteTableShards_VerifyFailureLeavesTheLastExportInPlace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardContents(t, dir, "events")

	ops := shardOpsWith(nil, nil, func(string, string, int, int64) error {
		return errors.New("verify refused")
	})

	if _, err := writeTableShardsWith(ops, dir, "events", syntheticRows(12, 200), 1000); err == nil {
		t.Fatal("an export that failed verification reported success")
	}
	if after := shardContents(t, dir, "events"); !maps.Equal(before, after) {
		t.Errorf("the failed export left %v, want the export that was there: %v", after, before)
	}
	assertNoShardLitter(t, dir)

	fresh := t.TempDir()
	if _, err := writeTableShardsWith(ops, fresh, "events", syntheticRows(12, 200), 1000); err == nil {
		t.Fatal("an export that failed verification reported success")
	}
	if names := shardNames(t, fresh, "events"); len(names) != 0 {
		t.Errorf("shards = %v, want an unverified export to leave none", names)
	}
	assertNoShardLitter(t, fresh)
}

// A rollback that fails as well leaves the export with a gap in it, and the
// error carries both failures rather than the first one alone (gt-fhcwe).
func TestWriteTableShards_AFailedRollbackReportsBothFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}

	// Publishing a shard fails, and so does the rename that would put the old
	// one back.
	ops := shardOpsWith(func(oldpath, newpath string) error {
		if strings.HasSuffix(oldpath, ".tmp") || strings.HasSuffix(oldpath, ".bak") {
			return errors.New("rename refused")
		}
		return os.Rename(oldpath, newpath)
	}, nil, nil)

	_, err := writeTableShardsWith(ops, dir, "events", syntheticRows(12, 200), 1000)
	if err == nil {
		t.Fatal("a swap whose rollback failed reported success")
	}
	for _, want := range []string{"renaming", "restoring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to carry the %q failure too", err, want)
		}
	}
}

// A backup the cleanup cannot remove stops the swap before anything new reaches
// the directory, so the export is left as it was (gt-fhcwe).
func TestWriteTableShards_StuckBackupStopsTheSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := writeTableShards(dir, "events", syntheticRows(20, 200), 1000); err != nil {
		t.Fatalf("first export: %v", err)
	}
	before := shardContents(t, dir, "events")

	// A non-empty directory where the backup belongs: the cleanup cannot remove
	// it and neither can the next swap.
	stuck := filepath.Join(dir, "events.jsonl.bak")
	if err := os.MkdirAll(stuck, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "keep"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := writeTableShards(dir, "events", syntheticRows(12, 200), 1000)
	if err == nil || !strings.Contains(err.Error(), "clearing events shard backups") {
		t.Fatalf("err = %v, want the cleanup failure named", err)
	}
	if after := shardContents(t, dir, "events"); !maps.Equal(before, after) {
		t.Errorf("the refused swap left %v, want the export that was there: %v", after, before)
	}
}

// A temp file a write failed on does not stay behind: the swap takes the temps
// it already wrote with it (gt-fhcwe).
func TestWriteTableShards_WriteFailureClearsTheTempsItWrote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory where the second shard's temp belongs: the write cannot
	// replace it.
	blocked := filepath.Join(dir, "events.2.jsonl.tmp")
	if err := os.MkdirAll(blocked, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "keep"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeTableShards(dir, "events", syntheticRows(12, 200), 1000); err == nil {
		t.Fatal("a write that failed reported success")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && !e.IsDir() {
			t.Errorf("temp file %s left behind by a failed write", e.Name())
		}
	}
}

// A shard write that fails part way leaves no .jsonl.tmp in the export
// directory, and a re-export that needs fewer shards takes the temps a longer
// one left behind with it. The swap only knows the files jsonlShardPaths reads
// as shards, so a temp it steps over is what the cycle's `git add -A` commits
// (gt-1911b).
func TestWriteTableShards_FailedWriteLeavesNoTempFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory where the second shard's temp belongs: the write cannot
	// replace it, and a cleanup that only removes files leaves it behind.
	blocked := filepath.Join(dir, "events.2.jsonl.tmp")
	if err := os.MkdirAll(blocked, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "half-written"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeTableShards(dir, "events", syntheticRows(12, 200), 1000); err == nil {
		t.Fatal("a write that failed reported success")
	}
	if left := shardTemps(t, dir); len(left) != 0 {
		t.Errorf("temps after the failed write = %v, want none", left)
	}

	// A longer export a crash cut short leaves temps past the last shard the
	// new export writes.
	for _, name := range []string{"events.7.jsonl.tmp", "issues.7.jsonl.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writeTableShards(dir, "events", syntheticRows(3, 200), 1000); err != nil {
		t.Fatalf("the shorter re-export: %v", err)
	}
	if left := shardTemps(t, dir); !slices.Equal(left, []string{"issues.7.jsonl.tmp"}) {
		t.Errorf("temps after the shorter re-export = %v, want the other table's temp alone", left)
	}
}

// shardTemps lists the .tmp files in dir, in name order.
func shardTemps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// The size screen runs while the shards are still only in memory: refused after
// the write, an oversized shard is already in the tree the cycle commits.
func TestCheckShardSizes_RefusesAShardOverTheLimit(t *testing.T) {
	t.Parallel()
	fits, over := &bytes.Buffer{}, &bytes.Buffer{}
	fits.WriteString("1234567890")
	over.WriteString(strings.Repeat("x", 11))

	if err := checkShardSizes("events", []*bytes.Buffer{fits}, 10); err != nil {
		t.Errorf("a shard at the limit was refused: %v", err)
	}
	err := checkShardSizes("events", []*bytes.Buffer{fits, over}, 10)
	if err == nil {
		t.Fatal("a shard over the limit was written anyway")
	}
	if !strings.Contains(err.Error(), "shard 2 is 11 bytes") {
		t.Errorf("err = %v, want it to name the shard over the limit", err)
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

	if got, err := d.verifyNoPollution(gitRepo, []string{"hq"}); err != nil || got != 3 {
		t.Errorf("verifyNoPollution = (%d, %v), want (3, nil) across both shards", got, err)
	}
	removed := d.applyPollutionFilter(gitRepo, []string{"hq"})
	if removed["hq"] != 3 {
		t.Errorf("removed = %v, want 3 for hq across both shards", removed)
	}
	if got, err := d.verifyNoPollution(gitRepo, []string{"hq"}); err != nil || got != 0 {
		t.Errorf("verifyNoPollution after the filter = (%d, %v), want (0, nil)", got, err)
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
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
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

// TestSyncJsonlGitBackupHaltsWhenAShardCannotBeRead drives a cycle whose export
// cannot be read back by the post-scrub check: the cycle halts before the
// commit and raises the scrub alert, because the export it would push was never
// checked for test pollution (gt-fhcwe).
func TestSyncJsonlGitBackupHaltsWhenAShardCannotBeRead(t *testing.T) {
	t.Parallel()
	d, f := backupTestDaemon(t)
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", fmt.Sprintf("%shq=3", seedSubjectPrefix), map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)

	rec := notifyfake.New()
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"hq"}},
	}}
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
		writeNLines(t, filepath.Join(dir, table+".jsonl"), 1)
		return 1, nil
	}
	// An issues shard that is a directory: the check opens it and the read
	// fails, which is the read error it must report rather than read past.
	if err := os.MkdirAll(filepath.Join(gitRepo, "hq", "issues.2.jsonl"), 0755); err != nil {
		t.Fatal(err)
	}

	before := f.Ref(gitRepo, "refs/heads/main")
	d.syncJsonlGitBackup()

	if after := f.Ref(gitRepo, "refs/heads/main"); after != before {
		t.Error("the cycle committed an export the post-scrub check could not read")
	}
	open := rec.OpenEscalations(alertKeyJSONLScrub)
	if len(open) != 1 {
		t.Fatalf("open scrub escalations = %+v, want one", open)
	}
	if !strings.Contains(open[0].Title, "issues.2.jsonl") {
		t.Errorf("escalation title = %q, want it to name the shard it could not read", open[0].Title)
	}
}
