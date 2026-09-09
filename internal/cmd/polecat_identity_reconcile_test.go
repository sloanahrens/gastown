package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// setupReconcileTown builds a town with a "gastown" rig routed via
// routes.jsonl and a fake `bd` on PATH that answers `show` for
// gt-gastown-polecat-garnet from either the rig or town database (per
// BEADS_DIR), always refuses `show gt-wisp-0yhh` (simulating a deleted MR
// bead), and tracks town-side `delete` so a subsequent `show` correctly
// reports not-found. rigDesc/townDesc are appended to the standard agent
// preamble; an empty townDesc simulates "no legacy town row".
func setupReconcileTown(t *testing.T, rigDesc, townDesc string) (townRoot, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}

	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), townBeadsDir, rigBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{{Prefix: "gt-", Path: "gastown/mayor/rig"}}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	for _, dir := range []string{townBeadsDir, rigBeadsDir} {
		if err := os.WriteFile(filepath.Join(dir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
			t.Fatalf("write types sentinel: %v", err)
		}
	}

	const id = "gt-gastown-polecat-garnet"
	rigJSON := agentBeadJSON(t, id, rigDesc)
	var townJSON string
	if townDesc != "" {
		townJSON = agentBeadJSON(t, id, townDesc)
	}

	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "bd.log")
	deletedMarker := filepath.Join(binDir, "town-deleted")
	script := fmt.Sprintf(`#!/bin/sh
LOG=%q
RIG_DIR=%q
TOWN_DIR=%q
DELETED_MARKER=%q
RIG_JSON='%s'
TOWN_JSON='%s'
printf 'beads_dir=%%s args=%%s\n' "${BEADS_DIR:-<unset>}" "$*" >> "$LOG"
cmd=""
id=""
for arg in "$@"; do
  case "$arg" in
    --*) continue ;;
  esac
  if [ -z "$cmd" ]; then
    cmd="$arg"
  elif [ -z "$id" ]; then
    id="$arg"
    break
  fi
done
case "$cmd" in
  version|update|reopen)
    exit 0
    ;;
  delete)
    if [ "${BEADS_DIR:-}" = "$TOWN_DIR" ]; then
      : > "$DELETED_MARKER"
    fi
    exit 0
    ;;
  show)
    if [ "$id" = "gt-wisp-0yhh" ]; then
      echo 'not found' >&2
      exit 1
    fi
    if [ "${BEADS_DIR:-}" = "$RIG_DIR" ]; then
      printf '%%s\n' "$RIG_JSON"
      exit 0
    fi
    if [ "${BEADS_DIR:-}" = "$TOWN_DIR" ]; then
      if [ -f "$DELETED_MARKER" ] || [ -z "$TOWN_JSON" ]; then
        echo 'not found' >&2
        exit 1
      fi
      printf '%%s\n' "$TOWN_JSON"
      exit 0
    fi
    echo 'not found' >&2
    exit 1
    ;;
  list)
    printf '%%s\n' '[]'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, logPath, rigBeadsDir, townBeadsDir, deletedMarker, rigJSON, townJSON)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return townRoot, logPath
}

// agentBeadJSON builds a single-line `bd show --json` array response for one
// agent bead, as a string safe to embed inside a single-quoted shell literal
// (bd's JSON output never contains a literal single quote).
func agentBeadJSON(t *testing.T, id, desc string) string {
	t.Helper()
	issue := map[string]any{
		"id":          id,
		"title":       "polecat garnet",
		"issue_type":  "task",
		"status":      "open",
		"labels":      []string{"gt:agent"},
		"description": "polecat garnet\n\nrole_type: polecat\nrig: gastown\n" + desc,
	}
	b, err := json.Marshal([]any{issue})
	if err != nil {
		t.Fatalf("marshal fixture issue: %v", err)
	}
	if strings.Contains(string(b), "'") {
		t.Fatalf("fixture JSON must not contain a single quote: %s", b)
	}
	return string(b)
}

func TestReconcile_DryRunPrintsTableAndWritesNothing(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n", // rig row
		"agent_state: done\nactive_mr: null\n")         // town row
	var out bytes.Buffer
	err := runReconcile(&out, townRoot, "gastown", "garnet", false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "active_mr") || !strings.Contains(out.String(), "clear") {
		t.Fatalf("table must show active_mr with winner clear:\n%s", out.String())
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "args=update") || strings.Contains(string(log), "args=delete") {
		t.Fatalf("dry-run must not write; log:\n%s", log)
	}
}

func TestReconcile_ApplyUpdatesRigThenArchivesThenDeletesTown(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: done\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, townRoot, "gastown", "garnet", true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	townBeads := filepath.Join(townRoot, ".beads")
	upd := strings.Index(string(log), "beads_dir="+rigBeads+" args=update")
	del := strings.Index(string(log), "beads_dir="+townBeads+" args=delete")
	if upd == -1 || del == -1 || upd > del {
		t.Fatalf("expected rig update BEFORE town delete; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
}

func TestReconcile_RefusesWhenNoTownRow(t *testing.T) {
	townRoot, _ := setupReconcileTown(t, "agent_state: done\n", "") // empty => town show exits 1
	err := runReconcile(io.Discard, townRoot, "gastown", "garnet", true)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
}
