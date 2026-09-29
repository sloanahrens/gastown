package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// leftoversTown builds a town with one routed rig ("gastown") and one Dolt
// database directory ("gt") under .dolt-data, both clean.
func leftoversTown(t *testing.T) (townRoot, dbDoltDir, rigConfig string) {
	t.Helper()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	rigConfig = filepath.Join(rigBeads, "config.yaml")
	if err := os.WriteFile(rigConfig, []byte("export.auto: \"false\"\n# sync.remote: \"git+https://example.com/x.git\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbDoltDir = filepath.Join(townRoot, ".dolt-data", "gt", ".dolt")
	if err := os.MkdirAll(dbDoltDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepoState(t, dbDoltDir, `{"head":"refs/heads/main","remotes":{},"backups":{},"branches":{}}`)
	return townRoot, dbDoltDir, rigConfig
}

func writeRepoState(t *testing.T, dbDoltDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dbDoltDir, "repo_state.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runLeftovers(t *testing.T, townRoot string) *CheckResult {
	t.Helper()
	return NewDoltRemoteLeftoversCheck().Run(&CheckContext{TownRoot: townRoot})
}

func TestDoltRemoteLeftovers_CleanTownIsOK(t *testing.T) {
	t.Parallel()
	town, _, _ := leftoversTown(t)
	res := runLeftovers(t, town)
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want OK; message %q details %v", res.Status, res.Message, res.Details)
	}
}

func TestDoltRemoteLeftovers_NoDataDirIsOK(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	res := runLeftovers(t, town)
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want OK for a town with no .dolt-data; message %q", res.Status, res.Message)
	}
}

func TestDoltRemoteLeftovers_RemoteRowWarns(t *testing.T) {
	t.Parallel()
	town, doltDir, _ := leftoversTown(t)
	writeRepoState(t, doltDir, `{"head":"refs/heads/main","remotes":{"origin":{"name":"origin","url":"git+https://github.com/o/gastown.git","fetch_specs":[],"params":{}}},"backups":{},"branches":{}}`)
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning", res.Status)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "gt") || !strings.Contains(joined, "origin") {
		t.Errorf("details %q should name the database and the remote", joined)
	}
	if !strings.Contains(res.FixHint, "Removing Dolt remotes") {
		t.Errorf("FixHint %q should name the operator procedure", res.FixHint)
	}
}

func TestDoltRemoteLeftovers_RemoteCacheWarns(t *testing.T) {
	t.Parallel()
	town, doltDir, _ := leftoversTown(t)
	if err := os.MkdirAll(filepath.Join(doltDir, "git-remote-cache", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning", res.Status)
	}
	if joined := strings.Join(res.Details, "\n"); !strings.Contains(joined, "git-remote-cache") {
		t.Errorf("details %q should name git-remote-cache", joined)
	}
}

func TestDoltRemoteLeftovers_SyncRemoteWarns(t *testing.T) {
	t.Parallel()
	town, _, cfg := leftoversTown(t)
	if err := os.WriteFile(cfg, []byte("sync.remote: \"git+https://github.com/o/gastown.git\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning", res.Status)
	}
	if joined := strings.Join(res.Details, "\n"); !strings.Contains(joined, "sync.remote") || !strings.Contains(joined, "gastown") {
		t.Errorf("details %q should name the rig and sync.remote", joined)
	}
}

// An unreadable or malformed repo_state.json is not evidence of a clean
// database: the check must say it could not tell, not report OK.
func TestDoltRemoteLeftovers_UnparseableRepoStateWarns(t *testing.T) {
	t.Parallel()
	town, doltDir, _ := leftoversTown(t)
	writeRepoState(t, doltDir, `{not json`)
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning for an unparseable repo_state.json", res.Status)
	}
}

// A town with no routes.jsonl has no rigs to scan: that is a clean answer,
// not a failure, so it must not warn.
func TestDoltRemoteLeftovers_MissingRoutesIsOK(t *testing.T) {
	t.Parallel()
	town, _, _ := leftoversTown(t)
	if err := os.Remove(filepath.Join(town, ".beads", "routes.jsonl")); err != nil {
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want OK with no routes.jsonl; details %v", res.Status, res.Details)
	}
}

// A routes.jsonl that cannot be read is not evidence that no rig sets
// sync.remote: the check must say it could not tell.
func TestDoltRemoteLeftovers_UnreadableRoutesWarns(t *testing.T) {
	t.Parallel()
	town, _, _ := leftoversTown(t)
	routes := filepath.Join(town, ".beads", "routes.jsonl")
	if err := os.Remove(routes); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(routes, 0o755); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning for an unreadable routes.jsonl", res.Status)
	}
	if joined := strings.Join(res.Details, "\n"); !strings.Contains(joined, "routes.jsonl") {
		t.Errorf("details %q should name routes.jsonl", joined)
	}
}

// A Dolt data dir that exists but cannot be listed is not an empty one.
func TestDoltRemoteLeftovers_UnlistableDataDirWarns(t *testing.T) {
	t.Parallel()
	town, _, _ := leftoversTown(t)
	dataDir := filepath.Join(town, ".dolt-data")
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning for an unlistable data dir", res.Status)
	}
	if joined := strings.Join(res.Details, "\n"); !strings.Contains(joined, "cannot list Dolt data dir") {
		t.Errorf("details %q should say the data dir could not be listed", joined)
	}
}

// A rig config.yaml that exists but cannot be read is not evidence that it
// has no sync.remote.
func TestDoltRemoteLeftovers_UnreadableRigConfigWarns(t *testing.T) {
	t.Parallel()
	town, _, cfg := leftoversTown(t)
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg, 0o755); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning for an unreadable rig config.yaml", res.Status)
	}
	if joined := strings.Join(res.Details, "\n"); !strings.Contains(joined, "cannot read") {
		t.Errorf("details %q should say the config could not be read", joined)
	}
}

// Route paths come from a file; one that climbs out of the town is not read,
// and the check says it skipped it rather than reporting the town clean.
func TestDoltRemoteLeftovers_RouteOutsideTownIsReportedNotRead(t *testing.T) {
	t.Parallel()
	town, _, _ := leftoversTown(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".beads", "config.yaml"), []byte("sync.remote: \"git+https://x/y.git\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(town, outside)
	if err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"ev-","path":"` + rel + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(town, ".beads", "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning naming the skipped route", res.Status)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "route ev-") || !strings.Contains(joined, "not scanned") {
		t.Errorf("details %q should name the skipped route", joined)
	}
	if strings.Contains(joined, "sync.remote git+https://x/y.git") {
		t.Errorf("check read a config outside the town: %q", joined)
	}
}

// An absolute route path is skipped the same way, and reported.
func TestDoltRemoteLeftovers_AbsoluteRouteIsReported(t *testing.T) {
	t.Parallel()
	town, _, _ := leftoversTown(t)
	route := `{"prefix":"ab-","path":"/somewhere/else"}` + "\n"
	if err := os.WriteFile(filepath.Join(town, ".beads", "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runLeftovers(t, town)
	if res.Status != StatusWarning || !strings.Contains(strings.Join(res.Details, "\n"), "route ab-") {
		t.Fatalf("status = %v details %v, want a Warning naming route ab-", res.Status, res.Details)
	}
}

// A database directory whose .dolt or git-remote-cache cannot be examined is
// reported, not read as clean.
func TestDoltRemoteLeftovers_UnstattableDoltDirWarns(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	town, doltDir, _ := leftoversTown(t)
	dbDir := filepath.Dir(doltDir)
	if err := os.Chmod(dbDir, 0o000); err != nil { // stat of dbDir/.dolt now fails with EACCES
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbDir, 0o755) })
	res := runLeftovers(t, town)
	if res.Status != StatusWarning || !strings.Contains(strings.Join(res.Details, "\n"), "cannot examine") {
		t.Fatalf("status = %v details %v, want a Warning saying the database could not be examined", res.Status, res.Details)
	}
}
