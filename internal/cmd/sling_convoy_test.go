package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	convoyops "github.com/steveyegge/gastown/internal/convoy"
)

// callsBD records each bd call whole (dir, environment, argv) before the
// in-process bd answers it, for tests whose rule is where a call goes rather
// than what it says.
type callsBD struct {
	mu    sync.Mutex
	calls []beads.BDCall
	bd    *inprocBD
}

func (r *callsBD) run(ctx context.Context, c beads.BDCall) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	return r.bd.run(ctx, c)
}

// recorded is every call so far, --allow-stale probes included.
func (r *callsBD) recorded() []beads.BDCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]beads.BDCall(nil), r.calls...)
}

// callEnv is the value of key in a call's environment, "" when unset.
func callEnv(c beads.BDCall, key string) string {
	for _, kv := range c.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// sqlRowsBD is a bd whose `bd sql` answers rows, or fails when code is set.
func sqlRowsBD(rows string, code int) *inprocBD {
	return &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		if cmd == "sql" {
			return bdAnswer{stdout: rows, code: code}
		}
		return bdOut("[]")
	}}
}

// TestConvoyTracksBead: a convoy tracks a bead when the raw tracks-dep rows
// name it, directly or wrapped as external:<rig>:<id>; a failing bd reads as
// not tracked.
func TestConvoyTracksBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows string
		code int
		want bool
	}{
		{"exact match", `[{"depends_on_id":"gt-abc123"}]`, 0, true},
		{"external ref", `[{"depends_on_id":"external:gt-abc:gt-abc123"}]`, 0, true},
		{"other bead", `[{"depends_on_id":"gt-other456"}]`, 0, false},
		{"no deps", `[]`, 0, false},
		{"among several", `[{"depends_on_id":"gt-other1"},{"depends_on_id":"external:gt-abc:gt-abc123"},{"depends_on_id":"gt-other2"}]`, 0, true},
		{"bd fails", ``, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := sqlRowsBD(tc.rows, tc.code)
			town := slingConvoyTown{root: t.TempDir(), bd: bd.run}
			if got := town.convoyTracksBead("hq-cv-test", "gt-abc123"); got != tc.want {
				t.Fatalf("convoyTracksBead = %v, want %v; bd log:\n%s", got, tc.want, bd.log())
			}
			if !strings.Contains(bd.log(), "sql --json SELECT") || !strings.Contains(bd.log(), "hq-cv-test") {
				t.Fatalf("tracked deps were not read by raw sql on the convoy:\n%s", bd.log())
			}
		})
	}
}

// TestBdDepListRawIDsValidation verifies that bdDepListRawIDs rejects
// invalid bead IDs to prevent SQL injection.
func TestBdDepListRawIDsValidation(t *testing.T) {
	t.Parallel()
	_, err := convoyops.DepListRawIDs("/tmp", "'; DROP TABLE deps; --", "down", "tracks")
	if err == nil {
		t.Error("bdDepListRawIDs should reject SQL injection attempts")
	}

	_, err = convoyops.DepListRawIDs("/tmp", "valid-id", "down", "'; DROP TABLE deps; --")
	if err == nil {
		t.Error("bdDepListRawIDs should reject SQL injection in depType")
	}
}

// TestDepListRawIDsTurnsAutoCommitOnOverStaleEnv: the raw dep query runs with
// Dolt auto-commit on and read-only mode off even when the inherited
// environment says otherwise, and unwraps external:<rig>:<id> targets.
func TestDepListRawIDsTurnsAutoCommitOnOverStaleEnv(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	beadsDir := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"hq"}`), 0644); err != nil {
		t.Fatal(err)
	}
	rec := &callsBD{bd: sqlRowsBD(`[{"depends_on_id":"external:ag:ag-95s.1"}]`, 0)}
	town := convoyops.Town{
		Root: workDir,
		Env:  []string{"BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off"},
		Run:  rec.run,
	}

	ids, err := town.DepListRawIDs(workDir, "hq-cv-test", "down", "tracks")
	if err != nil {
		t.Fatalf("DepListRawIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "ag-95s.1" {
		t.Fatalf("ids = %v, want [ag-95s.1]", ids)
	}
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if len(c.Args) < 2 || c.Args[0] != "sql" || !strings.HasPrefix(c.Args[len(c.Args)-1], "SELECT COALESCE") {
		t.Fatalf("argv = %q, want sql SELECT COALESCE...", c.Args)
	}
	if got := callEnv(c, "BD_READONLY"); got != "" {
		t.Errorf("BD_READONLY = %q, want it stripped", got)
	}
	if got := callEnv(c, "BD_DOLT_AUTO_COMMIT"); got != "on" {
		t.Errorf("BD_DOLT_AUTO_COMMIT = %q, want on", got)
	}
}
