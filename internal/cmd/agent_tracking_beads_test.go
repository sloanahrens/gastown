package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentTrackingTown lays out a town whose refinery rig worktree redirects
// its .beads to the rig's mayor clone, and returns the town root, the town
// beads, the rig worktree and the rig beads it redirects to.
func agentTrackingTown(t *testing.T) (townRoot, townBeads, rigWorkDir, rigBeads string) {
	t.Helper()
	townRoot = filepath.Join(t.TempDir(), "gt")
	townBeads = filepath.Join(townRoot, ".beads")
	rigWorkDir = filepath.Join(townRoot, "gastown", "refinery", "rig")
	rigRedirect := filepath.Join(rigWorkDir, ".beads")
	rigBeads = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, dir := range []string{townBeads, rigRedirect, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(rigRedirect, "redirect"), []byte("../../mayor/rig/.beads"), 0o644); err != nil {
		t.Fatalf("write rig redirect: %v", err)
	}
	return townRoot, townBeads, rigWorkDir, rigBeads
}

func TestResolveAgentTrackingBeadsDirPrefersCwdRigRedirectOverBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot, townBeads, rigWorkDir, rigBeads := agentTrackingTown(t)

	gotWorkDir, err := findBeadsWorkDirFrom(rigWorkDir)
	if err != nil || gotWorkDir != rigWorkDir {
		t.Fatalf("findBeadsWorkDirFrom() = %q, %v; want %q", gotWorkDir, err, rigWorkDir)
	}

	gotBeadsDir, err := resolveAgentTrackingBeadsDirFrom(rigWorkDir, townBeads)
	if err != nil || gotBeadsDir != rigBeads {
		t.Fatalf("resolveAgentTrackingBeadsDirFrom() = %q, %v; want %q", gotBeadsDir, err, rigBeads)
	}

	// The project-work resolver keeps the inherited BEADS_DIR first.
	gotLocalWorkDir, err := localBeadsWorkDir(rigWorkDir, townBeads)
	if err != nil || gotLocalWorkDir != townRoot {
		t.Fatalf("localBeadsWorkDir() = %q, %v; want env parent %q", gotLocalWorkDir, err, townRoot)
	}
}

// TestResolveAgentTrackingBeadsDirFallsBackToBeadsDir: with no .beads above
// the cwd, the inherited BEADS_DIR is the database.
func TestResolveAgentTrackingBeadsDirFallsBackToBeadsDir(t *testing.T) {
	t.Parallel()
	_, townBeads, _, _ := agentTrackingTown(t)
	got, err := resolveAgentTrackingBeadsDirFrom(t.TempDir(), townBeads)
	if err != nil || got != townBeads {
		t.Fatalf("resolveAgentTrackingBeadsDirFrom() = %q, %v; want %q", got, err, townBeads)
	}
}

// TestModifyAgentStatePinsTheRigDatabase: the read and the write of an agent
// state change both run against the rig database the cwd resolved to, the
// read pinned read-only and the write pinned as a mutation.
func TestModifyAgentStatePinsTheRigDatabase(t *testing.T) {
	t.Parallel()
	_, townBeads, rigWorkDir, rigBeads := agentTrackingTown(t)
	metadata := []byte(`{"dolt_database":"rigdb","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`)
	if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), metadata, 0o644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	beadsDir, err := resolveAgentTrackingBeadsDirFrom(rigWorkDir, townBeads)
	if err != nil {
		t.Fatal(err)
	}

	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		if cmd == "show" {
			return bdOut(`[{"labels":["gt:agent","idle:2"]}]`)
		}
		return bdOut("")
	}}
	rec := &callsBD{bd: bd}
	if err := modifyAgentState(rec.run, io.Discard, "gt-gastown-refinery", beadsDir, agentLabelOps{set: []string{"idle=0"}}, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("modifyAgentState() error = %v", err)
	}

	calls := rec.recorded()
	if len(calls) != 2 {
		t.Fatalf("bd calls = %d, want a show and an update; log:\n%s", len(calls), bd.log())
	}
	for _, c := range calls {
		if got := callEnv(c, "BEADS_DIR"); got != rigBeads {
			t.Errorf("bd %v BEADS_DIR = %q, want rig beads %q", c.Args, got, rigBeads)
		}
		if got := callEnv(c, "BEADS_DOLT_SERVER_DATABASE"); got != "rigdb" {
			t.Errorf("bd %v database = %q, want rigdb", c.Args, got)
		}
	}
	if got := callEnv(calls[0], "BD_READONLY"); got != "true" {
		t.Errorf("bd show BD_READONLY = %q, want true", got)
	}
	if got := callEnv(calls[1], "BD_DOLT_AUTO_COMMIT"); got != "on" {
		t.Errorf("bd update BD_DOLT_AUTO_COMMIT = %q, want on", got)
	}
	update := strings.Join(calls[1].Args, " ")
	for _, want := range []string{"update gt-gastown-refinery", "--set-labels=gt:agent", "--set-labels=idle:0", "--set-labels=heartbeat:1700000000"} {
		if !strings.Contains(update, want) {
			t.Errorf("update %q lacks %q", update, want)
		}
	}
}
