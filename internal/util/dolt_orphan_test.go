//go:build !windows

package util

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestIsDoltSQLServerArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args string
		want bool
	}{
		{"dolt sql-server --config /tmp/x/dolt-server-config.yaml", true},
		{"/usr/local/bin/dolt sql-server --port 3307", true},
		{"dolt --data-dir /x sql-server", true}, // shared matcher: global flags allowed
		{"dolt sql", false},
		{"dolt commit -m foo", false},
		{"claude --dangerously-skip-permissions", false},
		{"", false},
		{"dolt", false},
	}
	for _, tt := range tests {
		if got := isDoltSQLServerArgs(tt.args); got != tt.want {
			t.Errorf("isDoltSQLServerArgs(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestDoltSQLServerConfigPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args string
		want string
	}{
		{"dolt sql-server --config /tmp/x/dolt-server-config.yaml", "/tmp/x/dolt-server-config.yaml"},
		{"dolt sql-server --config=/tmp/y/dolt-server-config.yaml", "/tmp/y/dolt-server-config.yaml"},
		{"dolt sql-server --port 3307", ""},
		{"dolt sql-server", ""},
	}
	for _, tt := range tests {
		if got := doltSQLServerConfigPath(tt.args); got != tt.want {
			t.Errorf("doltSQLServerConfigPath(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestDoltSQLServerEndpointFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args     string
		wantPort int
		wantDir  string
	}{
		{"dolt sql-server --port 3399 --data-dir /tmp/doltprobe/data", 3399, "/tmp/doltprobe/data"},
		{"dolt sql-server --port=3308 --data-dir=/tmp/x", 3308, "/tmp/x"},
		{"dolt sql-server --config /tmp/x/dolt-server-config.yaml", 0, ""},
		{"dolt sql-server", 0, ""},
	}
	for _, tt := range tests {
		if got := doltSQLServerPort(tt.args); got != tt.wantPort {
			t.Errorf("doltSQLServerPort(%q) = %d, want %d", tt.args, got, tt.wantPort)
		}
		if got := doltSQLServerDataDir(tt.args); got != tt.wantDir {
			t.Errorf("doltSQLServerDataDir(%q) = %q, want %q", tt.args, got, tt.wantDir)
		}
	}
}

// TestDoltProcessDataDir prefers an explicit --data-dir and falls back to the
// --config file's directory, which is how a managed server names its data-dir.
func TestDoltProcessDataDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args string
		cfg  string
		want string
	}{
		{"dolt sql-server --port 3399 --data-dir /tmp/doltprobe/data", "", "/tmp/doltprobe/data"},
		{"dolt sql-server --config /tmp/x/config.yaml", "/tmp/x/config.yaml", "/tmp/x"},
		{"dolt sql-server --port 3399 --data-dir /d --config /tmp/x/config.yaml", "/tmp/x/config.yaml", "/d"},
		{"dolt sql-server", "", ""},
	}
	for _, tt := range tests {
		if got := doltProcessDataDir(tt.args, tt.cfg); got != tt.want {
			t.Errorf("doltProcessDataDir(%q, %q) = %q, want %q", tt.args, tt.cfg, got, tt.want)
		}
	}
}

// TestClassifyDoltOrphanEndpoint reproduces the gt-gyw5w leak: a test server
// on a foreign port with a /tmp data-dir keeps its endpoint on the orphan
// record, which is what picks its remedy (`kill <pid>`, not kill-imposters).
func TestClassifyDoltOrphanEndpoint(t *testing.T) {
	t.Parallel()
	got, ok := classifyDoltOrphan(doltProcEntry{
		PID: 29490, PPID: 1, Etime: "01:00:00",
		Args: "dolt sql-server --port 3399 --data-dir /tmp/doltprobe/data",
	})
	if !ok {
		t.Fatal("classifyDoltOrphan() ok = false, want true for the leaked test server")
	}
	if got.Port != 3399 || got.DataDir != "/tmp/doltprobe/data" {
		t.Errorf("endpoint = (port %d, data-dir %q), want (3399, /tmp/doltprobe/data)", got.Port, got.DataDir)
	}
	if got.Reason != "orphan" {
		t.Errorf("Reason = %q, want orphan", got.Reason)
	}
	if remedy := StrayDoltRemedy(StrayDoltProcess{PID: got.PID, Port: got.Port, DataDir: got.DataDir}, 3307, "/Users/x/gt/.dolt-data"); remedy != "kill 29490" {
		t.Errorf("remedy = %q, want %q", remedy, "kill 29490")
	}
}

func TestIsBeadsTestConfigPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"/tmp/beads-bd-tests-1739512292/T/dolt-server-config.yaml", true},
		{"/tmp/beads-bd-tests-3865361009/shared-server/dolt-server-config.yaml", true},
		{"/Users/x/gt/.dolt-data/config.yaml", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isBeadsTestConfigPath(tt.path); got != tt.want {
			t.Errorf("isBeadsTestConfigPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestParseDoltProcessTable(t *testing.T) {
	t.Parallel()
	out := `  PID  PPID   ELAPSED COMMAND
    1     0 10-00:00:00 /sbin/launchd
76098     1    18:39:00 dolt sql-server --config /tmp/beads-bd-tests-1739512292/T/dolt-server-config.yaml
 9001  8000       00:05 dolt sql-server --port 3307 --config /Users/x/gt/daemon/../.dolt-data/config.yaml
  not-a-pid 1 00:01:00 dolt sql-server
`
	entries := parseDoltProcessTable(out)
	if len(entries) != 3 {
		t.Fatalf("parseDoltProcessTable() returned %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[1].PID != 76098 || entries[1].PPID != 1 {
		t.Errorf("entries[1] = %+v, want PID=76098 PPID=1", entries[1])
	}
	if !isDoltSQLServerArgs(entries[1].Args) {
		t.Errorf("entries[1].Args = %q, expected a dolt sql-server invocation", entries[1].Args)
	}
	if cfg := doltSQLServerConfigPath(entries[1].Args); cfg != "/tmp/beads-bd-tests-1739512292/T/dolt-server-config.yaml" {
		t.Errorf("config path = %q, want beads-bd-tests path", cfg)
	}
}

func TestClassifyDoltOrphan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		entry      doltProcEntry
		wantOK     bool
		wantReason string
	}{
		{
			name:       "ppid 1 old enough is orphan",
			entry:      doltProcEntry{PID: 100, PPID: 1, Etime: "18:39:00", Args: "dolt sql-server --port 3307"},
			wantOK:     true,
			wantReason: "orphan",
		},
		{
			name:       "beads test config with live parent is still orphan",
			entry:      doltProcEntry{PID: 101, PPID: 500, Etime: "05:00", Args: "dolt sql-server --config /tmp/beads-bd-tests-1/shared-server/dolt-server-config.yaml"},
			wantOK:     true,
			wantReason: "orphan",
		},
		{
			name:       "live parent, non-test config is unexpected not orphan",
			entry:      doltProcEntry{PID: 102, PPID: 500, Etime: "05:00", Args: "dolt sql-server --port 3308"},
			wantOK:     true,
			wantReason: "unexpected",
		},
		{
			name:   "too young is not a candidate",
			entry:  doltProcEntry{PID: 103, PPID: 1, Etime: "00:05", Args: "dolt sql-server --port 3307"},
			wantOK: false,
		},
		{
			// A daemonized production server is also reparented to PPID 1
			// once its launching shell exits, but runs from a real,
			// persistent --config rather than a test scratch dir or no
			// --config at all. PPID<=1 alone must not tag it "orphan" —
			// that's the shape Fix() SIGTERMs (gt-l7za1).
			name:       "ppid 1 with real non-test config is unexpected not orphan",
			entry:      doltProcEntry{PID: 104, PPID: 1, Etime: "18:39:00", Args: "dolt sql-server --config /Users/x/gt/.dolt-data/config.yaml"},
			wantOK:     true,
			wantReason: "unexpected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := classifyDoltOrphan(tt.entry)
			if ok != tt.wantOK {
				t.Fatalf("classifyDoltOrphan() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.Reason != tt.wantReason {
				t.Errorf("classifyDoltOrphan() reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestTownDoltServerPID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if pid := townDoltServerPID(dir); pid != 0 {
		t.Errorf("townDoltServerPID() with no pid file = %d, want 0", pid)
	}

	daemonDir := filepath.Join(dir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "dolt.pid"), []byte("54321\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if pid := townDoltServerPID(dir); pid != 54321 {
		t.Errorf("townDoltServerPID() = %d, want 54321", pid)
	}
}

// TestTownServerPIDsUsesForbiddenTownRoot reproduces gt-l7za1: a hermetic
// sandbox townRoot has no daemon/dolt.pid of its own, so the real live
// town's server PID must still be recognized via workspace.
// ForbiddenTownRoot, or the process table scan (which isn't sandboxed) finds
// it with nothing to match against.
func TestTownServerPIDsUsesForbiddenTownRoot(t *testing.T) {
	t.Parallel()
	sandboxRoot := t.TempDir() // no daemon/dolt.pid — mirrors a hermetic town

	realRoot := t.TempDir()
	realDaemonDir := filepath.Join(realRoot, "daemon")
	if err := os.MkdirAll(realDaemonDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDaemonDir, "dolt.pid"), []byte("35519\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if pids := townServerPIDsWith(sandboxRoot, ""); len(pids) != 0 {
		t.Errorf("townServerPIDs() without ForbiddenTownRoot set = %v, want empty", pids)
	}

	pids := townServerPIDsWith(sandboxRoot, realRoot)
	if !pids[35519] {
		t.Errorf("townServerPIDs() = %v, want to include the real town's PID 35519", pids)
	}
}

// TestFindStaleBeadsTestTempDirs reads a scripted process table, so it also
// asserts that a directory a live dolt sql-server still uses is kept.
func TestFindStaleBeadsTestTempDirs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	staleName := "beads-bd-tests-" + strconv.Itoa(1)
	stalePath := filepath.Join(tmpDir, staleName)
	if err := os.MkdirAll(stalePath, 0755); err != nil {
		t.Fatal(err)
	}
	// Backdate the mtime past doltOrphanMinAge so it's not treated as "still starting up".
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}

	freshName := "beads-bd-tests-" + strconv.Itoa(2)
	freshPath := filepath.Join(tmpDir, freshName)
	if err := os.MkdirAll(freshPath, 0755); err != nil {
		t.Fatal(err)
	}

	unrelated := filepath.Join(tmpDir, "not-a-beads-dir")
	if err := os.MkdirAll(unrelated, 0755); err != nil {
		t.Fatal(err)
	}

	livePath := filepath.Join(tmpDir, "beads-bd-tests-3")
	if err := os.MkdirAll(filepath.Join(livePath, "dolt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(livePath, old, old); err != nil {
		t.Fatal(err)
	}
	snapshot := func() ([]doltProcEntry, error) {
		return []doltProcEntry{
			{PID: 10, PPID: 1, Etime: "01:00:00", Args: "dolt sql-server --config " + filepath.Join(livePath, "dolt", "config.yaml")},
			{PID: 11, PPID: 1, Etime: "01:00:00", Args: "/bin/sleep 60"},
		}, nil
	}

	stale, err := findStaleBeadsTestTempDirs(tmpDir, snapshot)
	if err != nil {
		t.Fatalf("FindStaleBeadsTestTempDirs() error = %v", err)
	}

	foundStale := false
	for _, s := range stale {
		if s == stalePath {
			foundStale = true
		}
		if s == freshPath {
			t.Errorf("FindStaleBeadsTestTempDirs() returned fresh dir %q, should have been filtered by age", freshPath)
		}
		if s == livePath {
			t.Errorf("FindStaleBeadsTestTempDirs() returned %q, which a live dolt sql-server still uses", livePath)
		}
		if s == unrelated {
			t.Errorf("FindStaleBeadsTestTempDirs() returned non-beads dir %q", unrelated)
		}
	}
	if !foundStale {
		t.Errorf("FindStaleBeadsTestTempDirs() = %v, expected to contain %q", stale, stalePath)
	}
}

func TestRemoveStaleBeadsTestTempDirs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	valid := filepath.Join(tmpDir, "beads-bd-tests-123")
	if err := os.MkdirAll(valid, 0755); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(tmpDir, "not-a-beads-dir")
	if err := os.MkdirAll(unsafe, 0755); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveStaleBeadsTestTempDirs([]string{valid, unsafe})
	if err != nil {
		t.Fatalf("RemoveStaleBeadsTestTempDirs() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != valid {
		t.Errorf("RemoveStaleBeadsTestTempDirs() removed = %v, want [%q]", removed, valid)
	}
	if _, err := os.Stat(valid); !os.IsNotExist(err) {
		t.Errorf("expected %q to be removed", valid)
	}
	if _, err := os.Stat(unsafe); err != nil {
		t.Errorf("expected %q to survive (not a beads-bd-tests- dir): %v", unsafe, err)
	}
}
