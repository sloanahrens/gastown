package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

const sandboxLogsTestRigs = `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/gastown.git","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"gt"}}}}`

// sandboxLogsTestSeat is the session name the registry's prefix gives
// gastown/max.
const sandboxLogsTestSeat = "gt-max"

// sandboxLogsTown is a town whose rig registry names gastown, prefix gt.
func sandboxLogsTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(sandboxLogsTestRigs), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// writeSandboxLogsRecord writes rec to the town's registry.
func writeSandboxLogsRecord(t *testing.T, town string, rec session.SandboxRecord) {
	t.Helper()
	if err := session.WriteSandboxRecord(town, rec); err != nil {
		t.Fatal(err)
	}
}

// writeSandboxLogsRaw writes raw at seat's record path, so a test can hold a
// record the reader cannot read.
func writeSandboxLogsRaw(t *testing.T, town, seat, raw string) {
	t.Helper()
	dir := session.SandboxRegistryDir(town)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, seat+".json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxLogsNeedsATown: a town the command cannot find is an error, not a
// read of the caller's directory, so a seat outside a town never reaches
// another town's records.
func TestSandboxLogsNeedsATown(t *testing.T) {
	t.Parallel()
	err := runSandboxLogsCmdWith(&cobra.Command{}, []string{"gastown/max"}, func() (string, error) {
		return "", errors.New("not in a Gas Town workspace")
	})
	if err == nil || !strings.Contains(err.Error(), "sandbox logs: not in a Gas Town workspace") {
		t.Fatalf("no town: err = %v, want the town lookup's failure under the command's name", err)
	}
}

// TestSandboxLogs drives one seat's logs through the production call with the
// fake openshell CLI: the record names the sandbox gt-max on gw1, so the one
// call is `logs gt-max --since 5m` there, and its text reaches stdout
// unchanged (gt-fldibt).
func TestSandboxLogs(t *testing.T) {
	t.Parallel()
	if f := sandboxLogsCmd.Flags().Lookup("since"); f == nil || f.DefValue != "15m0s" {
		t.Fatalf("the --since default is not 15m: %v", f)
	}
	if sandboxLogsCmd.Parent() != sandboxCmd {
		t.Fatal("gt sandbox logs is not a subcommand of gt sandbox")
	}

	town := sandboxLogsTown(t)
	writeSandboxLogsRecord(t, town, session.SandboxRecord{
		Session: sandboxLogsTestSeat, Rig: "gastown", Polecat: "max", Gateway: "gw1",
	})
	const lines = "supervisor: seat max is up\nocsf: exec allowed\n"
	fake := &sandbox.Fake{Reply: map[string]string{"logs": lines}}

	var out bytes.Buffer
	if err := executeSandboxLogs(context.Background(), &out, town, "gastown/max", 5*time.Minute, fake.Runner()); err != nil {
		t.Fatalf("sandbox logs gastown/max: %v", err)
	}
	if out.String() != lines {
		t.Errorf("stdout = %q, want the gateway's text unchanged %q", out.String(), lines)
	}
	calls := fake.Calls()
	want := []string{"openshell", "-g", "gw1", "logs", sandboxLogsTestSeat, "--since", "5m", "--source", "sandbox"}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Errorf("gateway calls = %q, want one %q", calls, want)
	}
}

// TestSandboxLogsRefusals drives each way a seat's logs cannot be printed. Every
// row exits with an error naming the seat and prints nothing, so a caller never
// reads a refused read as a seat with no log lines (gt-fldibt).
func TestSandboxLogsRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr string
		// rigs replaces the town's rig registry when set; record is written
		// under the seat's session name, or raw at that path when it is set.
		rigs   string
		record *session.SandboxRecord
		raw    string
		fake   *sandbox.Fake
		// wantErr is a phrase the error must carry; wantNotInErr is text the
		// error must not carry.
		wantErr      string
		wantNotInErr string
	}{
		{
			name:    "address with no rig",
			addr:    "max",
			wantErr: "unknown seat: want <rig>/<polecat>",
		},
		{
			name:    "address with an empty rig",
			addr:    "/max",
			wantErr: "unknown seat: want <rig>/<polecat>",
		},
		{
			name:    "address with an empty polecat",
			addr:    "gastown/",
			wantErr: "unknown seat: want <rig>/<polecat>",
		},
		{
			name:    "address with an extra part",
			addr:    "gastown/polecats/max",
			wantErr: "unknown seat: want <rig>/<polecat>",
		},
		{
			name:    "rig registry the town cannot read",
			addr:    "gastown/max",
			rigs:    "{ not a registry",
			wantErr: "reading the town's rig registry",
		},
		{
			name:    "rig the registry does not name",
			addr:    "nowhere/max",
			wantErr: `unknown seat: the town's rig registry does not name rig "nowhere"`,
		},
		{
			name:    "seat with no sandbox record",
			addr:    "gastown/max",
			wantErr: "no sandbox record",
		},
		{
			name:    "unreadable record",
			addr:    "gastown/max",
			raw:     "{ not a record",
			wantErr: "reading its sandbox record",
		},
		{
			name:    "record that names no sandbox",
			addr:    "gastown/max",
			raw:     `{"session":"","rig":"gastown","polecat":"max"}`,
			wantErr: `names sandbox "", not "gt-max"`,
		},
		{
			name:    "record that names another sandbox",
			addr:    "gastown/max",
			raw:     `{"session":"gt-toast","rig":"gastown","polecat":"max"}`,
			wantErr: `names sandbox "gt-toast", not "gt-max"`,
		},
		{
			name: "logs call fails",
			addr: "gastown/max",
			record: &session.SandboxRecord{
				Session: sandboxLogsTestSeat, Rig: "gastown", Polecat: "max", Gateway: "gw1",
			},
			fake: &sandbox.Fake{
				Reply:  map[string]string{"logs": "log text that must not reach the error"},
				Err:    map[string]error{"logs": errors.New("gateway unreachable")},
				Stderr: map[string]string{"logs": "connection refused"},
			},
			wantErr:      "gateway unreachable",
			wantNotInErr: "log text that must not reach the error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := sandboxLogsTown(t)
			if tt.rigs != "" {
				if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(tt.rigs), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			switch {
			case tt.raw != "":
				writeSandboxLogsRaw(t, town, sandboxLogsTestSeat, tt.raw)
			case tt.record != nil:
				writeSandboxLogsRecord(t, town, *tt.record)
			}
			fake := tt.fake
			if fake == nil {
				fake = &sandbox.Fake{}
			}

			var out bytes.Buffer
			err := executeSandboxLogs(context.Background(), &out, town, tt.addr, 15*time.Minute, fake.Runner())

			if err == nil {
				t.Fatalf("sandbox logs %s: no error; the seat cannot be refused this way", tt.addr)
			}
			if !strings.Contains(err.Error(), tt.addr) {
				t.Errorf("error %q does not name the seat %s", err, tt.addr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not say %q", err, tt.wantErr)
			}
			if tt.wantNotInErr != "" && strings.Contains(err.Error(), tt.wantNotInErr) {
				t.Errorf("error %q carries the gateway's log text", err)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q before refusing", out.String())
			}
		})
	}
}
