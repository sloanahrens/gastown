package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gtconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestReadBeadsRuntimeConfigServerMetadata(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_server_host": "192.0.2.10",
  "dolt_server_port": 4311,
  "dolt_database": "gastown"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cfg, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Database != "gastown" {
		t.Fatalf("Database = %q, want gastown", cfg.Database)
	}
	if cfg.Host != "192.0.2.10" {
		t.Fatalf("Host = %q, want 192.0.2.10", cfg.Host)
	}
	if cfg.Port != 4311 {
		t.Fatalf("Port = %d, want 4311", cfg.Port)
	}
}

func TestReadBeadsRuntimeConfigDefaultServerAddr(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "database": "dolt"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cfg, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != doltserver.DefaultPort {
		t.Fatalf("Port = %d, want default %d", cfg.Port, doltserver.DefaultPort)
	}
}

func TestReadBeadsRuntimeConfigPortFileFallback(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "database": "dolt"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("43113\n"), 0600); err != nil {
		t.Fatalf("write port file: %v", err)
	}

	cfg, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Port != 43113 {
		t.Fatalf("Port = %d, want port file 43113", cfg.Port)
	}
}

func TestReadBeadsRuntimeConfigIgnoresEmbeddedMetadata(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "embedded",
  "dolt_database": "gastown"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	if _, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir); ok {
		t.Fatal("embedded metadata should not be reported as shared-server config")
	}
}

func TestBeadsScopeHint_HQWarnsAgainstGlobal(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(string(filepath.Separator), "custom", "town root")
	hint := beadsScopeHint("hq", townRoot)

	for _, want := range []string{"database hq", "bd -C " + gtconfig.ShellQuote(townRoot), "bd --global", "beads_global"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("beadsScopeHint() missing %q in:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "~/gt") {
		t.Fatalf("beadsScopeHint() should not hardcode ~/gt:\n%s", hint)
	}
}

func TestBeadsScopeHint_NonHQEmpty(t *testing.T) {
	t.Parallel()
	if hint := beadsScopeHint("gastown", "/custom/town"); hint != "" {
		t.Fatalf("beadsScopeHint() = %q, want empty", hint)
	}
}

func TestBeadsClientSummary_BracketsIPv6Host(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  doltserver.BeadsRuntimeConfig
		want string
	}{
		{
			name: "IPv4 host",
			cfg:  doltserver.BeadsRuntimeConfig{Source: "/t/.beads/metadata.json", Database: "gastown", Host: "127.0.0.1", Port: 3307},
			want: "server metadata, database gastown, 127.0.0.1:3307, from /t/.beads/metadata.json",
		},
		{
			name: "IPv6 loopback host",
			cfg:  doltserver.BeadsRuntimeConfig{Source: "/t/.beads/metadata.json", Database: "gastown", Host: "::1", Port: 3307},
			want: "server metadata, database gastown, [::1]:3307, from /t/.beads/metadata.json",
		},
		{
			name: "no port omits endpoint",
			cfg:  doltserver.BeadsRuntimeConfig{Database: "gastown", Host: "::1"},
			want: "server metadata, database gastown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := beadsClientSummary(tt.cfg); got != tt.want {
				t.Fatalf("beadsClientSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusDatabases_PrefersLiveList(t *testing.T) {
	t.Parallel()
	served := []string{"be", "gt", "hq", "om"}
	cached := []string{"be", "gt", "hq", "om", "gastown"} // stale snapshot from server start

	dbs, label := statusDatabases(served, nil, cached)

	if strings.Join(dbs, ",") != strings.Join(served, ",") {
		t.Fatalf("statusDatabases() = %v, want live list %v", dbs, served)
	}
	if !strings.Contains(label, "live") {
		t.Fatalf("label %q should annotate the list as live", label)
	}
}

func TestStatusDatabases_TrustsEmptyLiveList(t *testing.T) {
	t.Parallel()
	cached := []string{"gastown"}

	dbs, _ := statusDatabases(nil, nil, cached)

	if len(dbs) != 0 {
		t.Fatalf("statusDatabases() = %v, want empty live list (not stale cache)", dbs)
	}
}

func TestStatusDatabases_FallsBackToCachedOnQueryError(t *testing.T) {
	t.Parallel()
	cached := []string{"be", "gt"}

	dbs, label := statusDatabases(nil, errors.New("connection refused"), cached)

	if strings.Join(dbs, ",") != strings.Join(cached, ",") {
		t.Fatalf("statusDatabases() = %v, want cached fallback %v", dbs, cached)
	}
	if !strings.Contains(label, "cached") {
		t.Fatalf("label %q should annotate the list as cached", label)
	}
}

// stubArchiveBehindReader supplies the archive lag without a Dolt server.
type stubArchiveBehindReader struct {
	commits map[string]int64
	events  map[string]int64
	errs    map[string]error
}

func (s stubArchiveBehindReader) commitsBehind(_ context.Context, db string, _ daemon.ArchiveWatermark) (int64, error) {
	return s.commits[db], s.errs[db]
}

func (s stubArchiveBehindReader) eventsBehind(_ context.Context, db string, _ daemon.EventsArchiveWatermark) (int64, error) {
	return s.events[db], s.errs[db]
}

// TestDoltStatusShowsArchiveWatermark: each database's status line carries both
// archive watermarks and how many rows it is behind each; a count that cannot
// be read is shown as such rather than as caught up.
func TestDoltStatusShowsArchiveWatermark(t *testing.T) {
	t.Parallel()
	wms := daemon.ArchiveWatermarks{
		Commits: map[string]daemon.ArchiveWatermark{
			"gt": {Date: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), Hashes: []string{"abc"}},
			"hq": {Date: time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), Hashes: []string{"def"}},
			"be": {Date: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Hashes: []string{"ghi"}},
			"om": {Date: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), Hashes: []string{"jkl"}},
		},
		Events: map[string]daemon.EventsArchiveWatermark{
			"gt": {
				CreatedAt: "2026-11-02 09:15:00",
				Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "99"}},
			},
			"hq": {
				CreatedAt: "2026-11-01 22:00:00",
				Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-11-01 22:00:00", ID: "7"}},
			},
		},
	}
	reader := stubArchiveBehindReader{
		commits: map[string]int64{"gt": 3, "hq": 0, "om": 1, "new-db": 5},
		events:  map[string]int64{"gt": 12, "hq": 0, "om": 4, "new-db": 9},
		errs:    map[string]error{"be": errors.New("connection refused")},
	}
	notes := archiveBehindNotes(context.Background(), reader, wms, []string{"gt", "hq", "be", "om", "new-db"})

	line := databaseStatusLine("gt", "gastown", notes["gt"])
	for _, want := range []string{
		"archived through 2026-11-02, 3 commits behind",
		"events archived through 2026-11-02 09:15:00, 12 events behind",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("status line = %q, want it to hold %q", line, want)
		}
	}
	if want := "archived through 2026-11-01, 0 commits behind; events archived through 2026-11-01 22:00:00, 0 events behind"; notes["hq"] != want {
		t.Errorf("hq note = %q, want %q", notes["hq"], want)
	}
	if want := "archived through 2026-10-30, 1 commit behind; events not archived yet, 4 events behind"; notes["om"] != want {
		t.Errorf("om note = %q, want %q", notes["om"], want)
	}

	// A failed count is not "0 commits behind": that would read as caught up.
	if strings.Contains(notes["be"], "0 commits behind") || !strings.Contains(notes["be"], "connection refused") {
		t.Errorf("be note = %q, want the read error rather than a zero", notes["be"])
	}
	// A database the archive has never covered is entirely behind: it must
	// carry a line saying so, or the pre-flatten check skips it (gt-7nxj4).
	if want := "not archived yet, 5 commits behind; events not archived yet, 9 events behind"; notes["new-db"] != want {
		t.Errorf("never-archived note = %q, want %q", notes["new-db"], want)
	}
	if got := databaseStatusLine("hq", "", notes["hq"]); !strings.HasPrefix(got, "    - hq; archived through") {
		t.Errorf("owner-less line = %q, want the note after the name", got)
	}
}
