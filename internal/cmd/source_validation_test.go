package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestRoutedIssueBeadsUsesTownRoutesForCustomPrefix(t *testing.T) {
	t.Parallel()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)

	_, gotCurrent, gotRouted := routedIssueBeads(workDir, "bd-source")
	if gotCurrent != currentBeadsDir {
		t.Fatalf("current beads dir = %q, want %q", gotCurrent, currentBeadsDir)
	}
	if gotRouted != ownerBeadsDir {
		t.Fatalf("routed beads dir = %q, want %q", gotRouted, ownerBeadsDir)
	}
}

func TestSourceRouteContextNamesCurrentAndRoutedDB(t *testing.T) {
	t.Parallel()
	context := sourceRouteContext("/town/gastown/.beads", "/town/beads/.beads")
	for _, want := range []string{"current_db=/town/gastown/.beads", "routed_db=/town/beads/.beads"} {
		if !strings.Contains(context, want) {
			t.Fatalf("source route context %q missing %q", context, want)
		}
	}
}

// submitSourceBD answers bd the way the two stores in a routed town do: the
// current rig's store holds a mirror of bd-source, the owner's store the real
// one (or nothing, when ownerMissing). Every call is recorded as
// "<BEADS_DIR>\t<args>".
type submitSourceBD struct {
	currentBeadsDir, ownerBeadsDir string
	ownerMissing                   bool

	mu    sync.Mutex
	calls []string
}

type submitSourceBDExit int

func (e submitSourceBDExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e submitSourceBDExit) ExitCode() int { return int(e) }

func (f *submitSourceBD) run(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
	args := c.Args
	if len(args) > 0 && args[0] == "--allow-stale" {
		args = args[1:]
	}
	beadsDir := ""
	for _, kv := range c.Env {
		if v, ok := strings.CutPrefix(kv, "BEADS_DIR="); ok {
			beadsDir = v
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, beadsDir+"\t"+strings.Join(args, " "))
	f.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "show" && args[1] == "bd-source":
		switch {
		case beadsDir == f.currentBeadsDir:
			return []byte(`[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task"}]`), nil, nil
		case beadsDir == f.ownerBeadsDir && !f.ownerMissing:
			return []byte(`[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task"}]`), nil, nil
		}
		return nil, []byte("Issue not found in " + beadsDir), submitSourceBDExit(1)
	case len(args) >= 1 && args[0] == "close":
		return nil, nil, nil
	}
	return nil, []byte("unexpected bd command: " + strings.Join(args, " ")), submitSourceBDExit(1)
}

func (f *submitSourceBD) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n") + "\n"
}

func TestResolveSubmitSourceIssueIgnoresCurrentRigMirror(t *testing.T) {
	t.Parallel()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	bd := &submitSourceBD{currentBeadsDir: currentBeadsDir, ownerBeadsDir: ownerBeadsDir}

	source, err := resolveSubmitSourceIssueRun(workDir, "bd-source", bd.run)
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}
	if source.Issue.Title != "owner source" {
		t.Fatalf("source title = %q, want routed owner source (current-rig mirror must be ignored)", source.Issue.Title)
	}
	if source.CurrentBeadsDir != currentBeadsDir || source.RoutedBeadsDir != ownerBeadsDir {
		t.Fatalf("route = current %q routed %q, want current %q routed %q", source.CurrentBeadsDir, source.RoutedBeadsDir, currentBeadsDir, ownerBeadsDir)
	}
}

func TestResolveSubmitSourceIssueFailureNamesRoutingContext(t *testing.T) {
	t.Parallel()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	bd := &submitSourceBD{currentBeadsDir: currentBeadsDir, ownerBeadsDir: ownerBeadsDir, ownerMissing: true}

	_, err := resolveSubmitSourceIssueRun(workDir, "bd-source", bd.run)
	if err == nil {
		t.Fatal("resolveSubmitSourceIssue succeeded, want routed owner lookup failure")
	}
	errText := err.Error()
	for _, want := range []string{"source_issue bd-source could not be resolved", "current_db=" + currentBeadsDir, "routed_db=" + ownerBeadsDir} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error %q missing %q", errText, want)
		}
	}
}

func TestDoneNoMRClosePathUsesRoutedSourceBeads(t *testing.T) {
	t.Parallel()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	bd := &submitSourceBD{currentBeadsDir: currentBeadsDir, ownerBeadsDir: ownerBeadsDir}

	source, err := resolveSubmitSourceIssueRun(workDir, "bd-source", bd.run)
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}
	if skipReason, fatal := doneSourceCloseSkipReason(source.BD, "bd-source", source.Issue); skipReason != "" || fatal {
		t.Fatalf("doneSourceCloseSkipReason = %q, %v; want close allowed", skipReason, fatal)
	}
	if err := source.BD.ForceCloseWithReason("done", "bd-source"); err != nil {
		t.Fatalf("routed source close: %v", err)
	}

	log := bd.log()
	assertBDLogContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogContains(t, log, ownerBeadsDir, "close bd-source")
	assertBDLogNotContains(t, log, currentBeadsDir, "close bd-source")
}

func setupRoutedSourceTestTown(t *testing.T) (workDir, currentBeadsDir, ownerBeadsDir string) {
	t.Helper()
	townRoot := t.TempDir()
	// Resolve symlinks now so every path derived below matches what
	// resolveDonePolecatWorktreeAt produces: it canonicalizes cwd via
	// filepath.EvalSymlinks before deriving BEADS_DIR routing. On macOS,
	// t.TempDir() lives under /var/folders/..., a symlink to
	// /private/var/folders/...; without this, runDone's canonicalized cwd
	// diverges from the non-canonical paths the bd stub below expects,
	// and every routed bd lookup falsely reports "issue not found".
	if resolved, err := filepath.EvalSymlinks(townRoot); err == nil {
		townRoot = resolved
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write town sentinel: %v", err)
	}

	workDir = filepath.Join(townRoot, "gastown", "polecats", "refuge", "gastown")
	currentBeadsDir = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	ownerBeadsDir = filepath.Join(townRoot, "beads", "mayor", "rig", ".beads")
	townBeadsDir := filepath.Join(townRoot, ".beads")
	for _, dir := range []string{filepath.Join(workDir, ".beads"), currentBeadsDir, ownerBeadsDir, townBeadsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, ".beads", "redirect"), []byte("../../../mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	return workDir, currentBeadsDir, ownerBeadsDir
}

func routedSourceTestTownRoot(workDir string) string {
	return filepath.Clean(filepath.Join(workDir, "..", "..", "..", ".."))
}

func assertBDLogContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if !strings.Contains(log, needle) {
		t.Fatalf("bd log missing %q:\n%s", needle, log)
	}
}

func assertBDLogNotContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if strings.Contains(log, needle) {
		t.Fatalf("bd log unexpectedly contains %q:\n%s", needle, log)
	}
}
