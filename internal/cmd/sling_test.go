package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func writeBDStub(t *testing.T, binDir string, unixScript string, windowsScript string) string {
	t.Helper()

	var path string
	if runtime.GOOS == "windows" {
		path = filepath.Join(binDir, "bd.cmd")
		if err := os.WriteFile(path, []byte(windowsScript), 0644); err != nil {
			t.Fatalf("write bd stub: %v", err)
		}
		return path
	}

	path = filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(unixScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	return path
}

func setupMutableBDRawSlingTest(t *testing.T, initialDescription string) (townRoot, rigPath, descPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mutable POSIX bd stub")
	}

	townRoot = t.TempDir()
	rigPath = filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"version":1}`), 0644); err != nil {
		t.Fatalf("write town marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir rig beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	rigs := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{
		"gastown": {GitURL: "git@github.com:test/gastown.git", AddedAt: time.Now().Truncate(time.Second), BeadsConfig: &config.BeadsConfig{Repo: "local", Prefix: "gt"}},
	}}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), rigs); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}

	descPath = filepath.Join(townRoot, "description.txt")
	statusPath := filepath.Join(townRoot, "status.txt")
	assigneePath := filepath.Join(townRoot, "assignee.txt")
	if err := os.WriteFile(descPath, []byte(initialDescription), 0644); err != nil {
		t.Fatalf("write description: %v", err)
	}
	if err := os.WriteFile(statusPath, []byte("open"), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}
	if err := os.WriteFile(assigneePath, []byte(""), 0644); err != nil {
		t.Fatalf("write assignee: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
set -eu
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "bd test"
  exit 0
fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allow-stale) shift ;;
    *) break ;;
  esac
done
cmd="${1:-}"
if [ "$#" -gt 0 ]; then shift; fi
case "$cmd" in
  show)
    desc=""
    if [ -f "$BD_DESC_FILE" ]; then
      desc=$(awk 'BEGIN{first=1} {gsub(/\\/,"\\\\"); gsub(/"/,"\\\""); if(!first){printf "\\n"} printf "%s",$0; first=0}' "$BD_DESC_FILE")
    fi
    status="open"
    if [ -f "$BD_STATUS_FILE" ]; then status=$(cat "$BD_STATUS_FILE"); fi
    assignee=""
    if [ -f "$BD_ASSIGNEE_FILE" ]; then assignee=$(cat "$BD_ASSIGNEE_FILE"); fi
    printf '[{"id":"gt-rawrollback","title":"Test issue","status":"%s","assignee":"%s","description":"%s","dependencies":[]}]\n' "$status" "$assignee" "$desc"
    ;;
  update)
    for arg in "$@"; do
      case "$arg" in
        --description=*) printf "%s" "${arg#--description=}" > "$BD_DESC_FILE" ;;
        --status=*) printf "%s" "${arg#--status=}" > "$BD_STATUS_FILE" ;;
        --assignee=*) printf "%s" "${arg#--assignee=}" > "$BD_ASSIGNEE_FILE" ;;
      esac
    done
    ;;
  version)
    echo "bd test"
    ;;
esac
exit 0
`
	writeBDStub(t, binDir, bdScript, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_DESC_FILE", descPath)
	t.Setenv("BD_STATUS_FILE", statusPath)
	t.Setenv("BD_ASSIGNEE_FILE", assigneePath)
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", "")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	return townRoot, rigPath, descPath
}

func readMutableBDDescription(t *testing.T, descPath string) string {
	t.Helper()
	b, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("read description: %v", err)
	}
	return string(b)
}

func assertNoRawReviewMetadata(t *testing.T, desc string) {
	t.Helper()
	if strings.Contains(desc, "no_merge: true") || strings.Contains(desc, "review_only: true") {
		t.Fatalf("stale raw review metadata remains in description:\n%s", desc)
	}
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	if fields != nil && (fields.NoMerge || fields.ReviewOnly) {
		t.Fatalf("parsed stale raw review metadata from description: %+v", fields)
	}
}

func assertHasRawReviewMetadata(t *testing.T, desc string) {
	t.Helper()
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	if fields == nil || !fields.NoMerge || !fields.ReviewOnly {
		t.Fatalf("raw review metadata missing from description:\n%s", desc)
	}
	if fields.AttachedAt == "" {
		t.Fatalf("raw review metadata missing attached_at:\n%s", desc)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields.AttachedAt); err != nil {
		t.Fatalf("attached_at %q is not RFC3339Nano: %v", fields.AttachedAt, err)
	}
}

func containsVarArg(line, key, value string) bool {
	plain := "--var " + key + "=" + value
	if strings.Contains(line, plain) {
		return true
	}
	quoted := "--var \"" + key + "=" + value + "\""
	return strings.Contains(line, quoted)
}

func TestParseWispIDFromJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		json    string
		wantID  string
		wantErr bool
	}{
		{
			name:   "new_epic_id",
			json:   `{"new_epic_id":"gt-wisp-abc","created":7,"phase":"vapor"}`,
			wantID: "gt-wisp-abc",
		},
		{
			name:   "root_id legacy",
			json:   `{"root_id":"gt-wisp-legacy"}`,
			wantID: "gt-wisp-legacy",
		},
		{
			name:   "result_id forward compat",
			json:   `{"result_id":"gt-wisp-result"}`,
			wantID: "gt-wisp-result",
		},
		{
			name:   "precedence prefers new_epic_id",
			json:   `{"root_id":"gt-wisp-legacy","new_epic_id":"gt-wisp-new"}`,
			wantID: "gt-wisp-new",
		},
		{
			name:    "missing id keys",
			json:    `{"created":7,"phase":"vapor"}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			json:    `{"new_epic_id":`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, err := parseWispIDFromJSON([]byte(tt.json))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseWispIDFromJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
			if gotID != tt.wantID {
				t.Fatalf("parseWispIDFromJSON() id = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}

func TestExtractIssueID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"unwraps external format", "external:gt-mol:gt-mol-abc123", "gt-mol-abc123"},
		{"unwraps beads external", "external:beads-task:beads-task-xyz", "beads-task-xyz"},
		{"passes through hq IDs", "hq-abc123", "hq-abc123"},
		{"passes through plain IDs", "gt-abc123", "gt-abc123"},
		{"handles malformed external (only 2 parts)", "external:gt-mol", "external:gt-mol"},
		{"handles empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := beads.ExtractIssueID(tt.id)
			if got != tt.want {
				t.Errorf("ExtractIssueID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestRoutedBeadReadUsesCanonicalShowWithoutUnsupportedAllowStale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell stub uses POSIX syntax")
	}
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	beadID := "gt-new123"
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), filepath.Join(townRoot, ".beads"), rigBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","name":"test"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"gastown/mayor/rig"}`,
		`{"prefix":"hq-","path":"."}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -eu
printf '%s|%s|%s\n' "$(pwd)" "${BEADS_DIR:-}" "$*" >> "${BD_LOG}"
if [ "$1" = "--allow-stale" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 1
fi
case "$1" in
  version)
    echo "bd 1.0.3"
    ;;
  show)
    if [ "${BEADS_DIR:-}" != "${RIG_BEADS}" ]; then
      echo "wrong database: ${BEADS_DIR:-}" >&2
      exit 1
    fi
    echo '[{"id":"gt-new123","title":"Routed bead","status":"open","assignee":"","description":"body","issue_type":"bug","labels":["x"],"dependencies":[{"id":"gt-wisp-old","status":"open"}]}]'
    ;;
esac
`
	_ = writeBDStub(t, binDir, bdScript, "")

	t.Setenv("BD_LOG", logPath)
	t.Setenv("RIG_BEADS", rigBeadsDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DIR", filepath.Join(townRoot, ".beads"))
	t.Setenv("GT_ROOT", townRoot)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if err := verifyBeadExists(beadID); err != nil {
		t.Fatalf("verifyBeadExists: %v", err)
	}
	info, err := getBeadInfoFromTownRoot(townRoot, beadID)
	if err != nil {
		t.Fatalf("getBeadInfoFromTownRoot: %v", err)
	}
	if info.Title != "Routed bead" || info.IssueType != "bug" || len(info.Labels) != 1 || len(info.Dependencies) != 1 {
		t.Fatalf("info = %+v, want routed issue fields preserved", info)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	showCalls := 0
	for _, line := range strings.Split(strings.TrimSpace(string(logBytes)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || !strings.Contains(parts[2], "show "+beadID) {
			continue
		}
		showCalls++
		if strings.Contains(parts[2], "--allow-stale") {
			t.Fatalf("bd show used unsupported --allow-stale: %q", line)
		}
		if parts[1] != rigBeadsDir {
			t.Fatalf("bd show BEADS_DIR = %q, want %q (line %q)", parts[1], rigBeadsDir, line)
		}
	}
	if showCalls == 0 {
		t.Fatalf("no bd show calls logged: %s", logBytes)
	}
}

func TestSlingRejectsBeadMissingFromTargetRigBeforeSpawn(t *testing.T) {
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigs := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"gastown": {
				GitURL:  "git@github.com:test/gastown.git",
				AddedAt: time.Now().Truncate(time.Second),
				BeadsConfig: &config.BeadsConfig{
					Repo:   "local",
					Prefix: "zz-",
				},
			},
		},
	}
	if err := config.SaveRigsConfig(rigsPath, rigs); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatalf("mkdir target rig dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"."}`,
		`{"prefix":"zz-","path":"gastown/mayor/rig"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
echo "$*" >> "${BD_LOG}"
cmd="$1"
shift || true
if [ "$cmd" = "--allow-stale" ]; then
  cmd="$1"
  shift || true
fi
case "$cmd" in
  show)
    if [ "${BEADS_DIR:-}" = "${TARGET_BEADS_DIR}" ]; then
      # The direct target-rig DB lookup must fail: the bead only resolves from HQ.
      exit 1
    fi
    echo '[{"title":"HQ-owned issue","status":"open","assignee":"","description":""}]'
    ;;
  mol|update|cook)
    echo "unexpected side effect: $cmd" >&2
    exit 2
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
echo %*>>"%BD_LOG%"
set "cmd=%1"
if "%cmd%"=="show" (
  if "%BEADS_DIR%"=="%TARGET_BEADS_DIR%" exit /b 1
  echo [{"title":"HQ-owned issue","status":"open","assignee":"","description":""}]
  exit /b 0
)
if "%cmd%"=="mol" exit /b 2
if "%cmd%"=="update" exit /b 2
if "%cmd%"=="cook" exit /b 2
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("TARGET_BEADS_DIR", filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevNoConvoy := slingNoConvoy
	prevNoBoot := slingNoBoot
	prevSpawn := spawnPolecatForSling
	t.Cleanup(func() {
		slingNoConvoy = prevNoConvoy
		slingNoBoot = prevNoBoot
		spawnPolecatForSling = prevSpawn
	})
	slingNoConvoy = true
	slingNoBoot = true

	spawnCalled := false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawnCalled = true
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "toast", ClonePath: filepath.Join(townRoot, "fake-polecat")}, nil
	}

	err = runSling(nil, []string{"gt-r2405", "gastown"})
	if err == nil {
		t.Fatal("expected target-rig database validation error")
	}
	if !strings.Contains(err.Error(), "not present in target rig") {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawnCalled {
		t.Fatal("spawnPolecatForSling was called before target-rig database validation rejected the bead")
	}
}

func TestTargetRigDatabaseAllowsRouteResolvedGtBead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell stub uses POSIX env logging")
	}
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	for _, dir := range []string{filepath.Join(townRoot, ".beads"), filepath.Join(townRoot, "mayor", "rig"), filepath.Join(rigDir, ".beads")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"gastown/mayor/rig"}`,
		`{"prefix":"hq-","path":"."}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"dolt_database":"gastown","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`), 0644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
if [ "$1" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo 'bd version 1.0.0'
  exit 0
fi
printf '%s|%s|%s|%s|%s|%s\n' "$*" "$(pwd)" "${BEADS_DIR:-}" "${BEADS_DOLT_DATA_DIR:-}" "${GT_DOLT_DATA:-}" "${BEADS_DOLT_SERVER_DATABASE:-}" >> "${BD_LOG}"
cmd="$1"
shift || true
if [ "$cmd" = "--allow-stale" ]; then
  cmd="$1"
  shift || true
fi
case "$cmd" in
  show)
    echo '[{"title":"Route issue","status":"open","assignee":"","description":""}]'
    ;;
  *)
    echo "unexpected command: $cmd" >&2
    exit 2
    ;;
esac
`
	_ = writeBDStub(t, binDir, bdScript, "")

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_DOLT_DATA", filepath.Join(townRoot, ".dolt-data"))
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(townRoot, "wrong-data"))
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "hq")

	if err := verifyBeadExistsInTargetRigDatabase("gt-hq-oy83-cleanup", "gastown", townRoot); err != nil {
		t.Fatalf("verifyBeadExistsInTargetRigDatabase: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	line := strings.TrimSpace(string(logBytes))
	parts := strings.Split(line, "|")
	if len(parts) != 6 {
		t.Fatalf("malformed bd log: %q", line)
	}
	if !strings.Contains(parts[0], "show gt-hq-oy83-cleanup --json") {
		t.Fatalf("bd args = %q, want route-resolved show", parts[0])
	}
	if parts[1] != rigDir {
		t.Fatalf("bd cwd = %q, want %q", parts[1], rigDir)
	}
	if want := filepath.Join(rigDir, ".beads"); parts[2] != want {
		t.Fatalf("BEADS_DIR = %q, want %q", parts[2], want)
	}
	if parts[3] != "" {
		t.Fatalf("BEADS_DOLT_DATA_DIR leaked: %q", parts[3])
	}
	if parts[4] != "" {
		t.Fatalf("GT_DOLT_DATA leaked: %q", parts[4])
	}
	if parts[5] != "gastown" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want gastown", parts[5])
	}
}

func setupCrossDatabaseSlingGuardTest(t *testing.T) (townRoot, logPath string) {
	t.Helper()

	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigs := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"gastown": {
				GitURL:  "git@github.com:test/gastown.git",
				AddedAt: time.Now().Truncate(time.Second),
				BeadsConfig: &config.BeadsConfig{
					Repo:   "local",
					Prefix: "zz-",
				},
			},
		},
	}
	if err := config.SaveRigsConfig(rigsPath, rigs); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatalf("mkdir target rig dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"."}`,
		`{"prefix":"zz-","path":"gastown/mayor/rig"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath = filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
echo "$*" >> "${BD_LOG}"
cmd="$1"
shift || true
if [ "$cmd" = "--allow-stale" ]; then
  cmd="$1"
  shift || true
fi
case "$cmd" in
  show)
    if [ "${BEADS_DIR:-}" = "${TARGET_BEADS_DIR}" ]; then
      exit 1
    fi
    echo '[{"title":"HQ-owned issue","status":"open","assignee":"","description":""}]'
    ;;
  create|update|cook|mol|close|dep)
    echo "unexpected side effect: $cmd" >&2
    exit 2
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
echo %*>>"%BD_LOG%"
set "cmd=%1"
if "%cmd%"=="show" (
  if "%BEADS_DIR%"=="%TARGET_BEADS_DIR%" exit /b 1
  echo [{"title":"HQ-owned issue","status":"open","assignee":"","description":""}]
  exit /b 0
)
if "%cmd%"=="create" exit /b 2
if "%cmd%"=="update" exit /b 2
if "%cmd%"=="cook" exit /b 2
if "%cmd%"=="mol" exit /b 2
if "%cmd%"=="close" exit /b 2
if "%cmd%"=="dep" exit /b 2
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("TARGET_BEADS_DIR", filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	return townRoot, logPath
}

func TestScheduleBeadRejectsMissingTargetRigDatabaseBeforeContext(t *testing.T) {
	_, logPath := setupCrossDatabaseSlingGuardTest(t)

	err := scheduleBead("gt-r2405", "gastown", ScheduleOptions{})
	if err == nil {
		t.Fatal("expected target-rig database validation error")
	}
	if !strings.Contains(err.Error(), "not present in target rig") {
		t.Fatalf("unexpected error: %v", err)
	}

	logBytes, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read bd log: %v", readErr)
	}
	log := string(logBytes)
	for _, sideEffect := range []string{"create", "update", "cook", "mol", "close", "dep"} {
		if strings.Contains(log, sideEffect) {
			t.Fatalf("bd side effect %q ran before target-rig database validation rejected the bead; log:\n%s", sideEffect, log)
		}
	}
}

func TestBatchSlingRejectsMissingTargetRigDatabaseBeforeSpawn(t *testing.T) {
	townRoot, _ := setupCrossDatabaseSlingGuardTest(t)

	prevDryRun := slingDryRun
	prevForce := slingForce
	prevSpawn := spawnPolecatForSling
	t.Cleanup(func() {
		slingDryRun = prevDryRun
		slingForce = prevForce
		spawnPolecatForSling = prevSpawn
	})
	slingDryRun = false
	slingForce = false

	spawnCalled := false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawnCalled = true
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "toast", ClonePath: filepath.Join(townRoot, "fake-polecat")}, nil
	}

	err := runBatchSling([]string{"gt-r2405"}, "gastown", filepath.Join(townRoot, ".beads"))
	if err == nil {
		t.Fatal("expected target-rig database validation error")
	}
	if !strings.Contains(err.Error(), "not present in target rig") {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawnCalled {
		t.Fatal("spawnPolecatForSling was called before target-rig database validation rejected the bead")
	}
}

func TestSchedulerRejectsReviewOnlyForEpicConvoy(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("review-only", false, "")
	if err := cmd.Flags().Set("review-only", "true"); err != nil {
		t.Fatalf("set review-only flag: %v", err)
	}

	for _, mode := range []string{"epic", "convoy"} {
		err := validateNoTaskOnlySchedulerFlags(cmd, mode)
		if err == nil {
			t.Fatalf("validateNoTaskOnlySchedulerFlags(%s) accepted --review-only", mode)
		}
		if !strings.Contains(err.Error(), "--review-only") {
			t.Fatalf("validateNoTaskOnlySchedulerFlags(%s) error = %v, want --review-only", mode, err)
		}
	}
}

func TestResolveTargetRejectsLivePolecatMissingTargetRigDatabase(t *testing.T) {
	townRoot, _ := setupCrossDatabaseSlingGuardTest(t)

	prevResolve := resolveTargetAgentFn
	t.Cleanup(func() { resolveTargetAgentFn = prevResolve })
	resolveTargetAgentFn = func(target string) (string, string, string, error) {
		return "gastown/polecats/toast", "%1", filepath.Join(townRoot, "gastown", "polecats", "toast"), nil
	}

	for _, target := range []string{"gastown/polecats/toast", "gastown/toast", "gt-gastown-polecat-toast"} {
		t.Run(target, func(t *testing.T) {
			_, err := resolveTarget(target, ResolveTargetOptions{
				BeadID:   "gt-r2405",
				TownRoot: townRoot,
			})
			if err == nil {
				t.Fatal("expected target-rig database validation error")
			}
			if !strings.Contains(err.Error(), "not present in target rig") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestResolveTargetCreateSpawnsPolecatShorthandWhenPaneMissing(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevResolve := resolveTargetAgentFn
	prevSpawn := spawnPolecatForSling
	t.Cleanup(func() {
		resolveTargetAgentFn = prevResolve
		spawnPolecatForSling = prevSpawn
	})
	resolveTargetAgentFn = func(target string) (string, string, string, error) {
		return "", "", "", errors.New("getting pane for gt-toast: exit status 1")
	}

	spawnCalled := false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawnCalled = true
		if rigName != "gastown" {
			t.Fatalf("rigName = %q, want gastown", rigName)
		}
		if !opts.Create {
			t.Fatal("expected Create option to be preserved")
		}
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "toast", ClonePath: filepath.Join(townRoot, "fake-polecat")}, nil
	}

	got, err := resolveTarget("gastown/toast", ResolveTargetOptions{Create: true, NoBoot: true})
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if !spawnCalled {
		t.Fatal("expected spawnPolecatForSling to be called")
	}
	if got.Agent != "gastown/polecats/toast" {
		t.Fatalf("Agent = %q, want gastown/polecats/toast", got.Agent)
	}
}

func TestResolveTargetCreateDoesNotSpawnCrewShorthandWhenPaneMissing(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "crew", "toast"), 0755); err != nil {
		t.Fatalf("mkdir crew: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevResolve := resolveTargetAgentFn
	prevSpawn := spawnPolecatForSling
	t.Cleanup(func() {
		resolveTargetAgentFn = prevResolve
		spawnPolecatForSling = prevSpawn
	})
	resolveTargetAgentFn = func(target string) (string, string, string, error) {
		return "", "", "", errors.New("getting pane for gt-crew-toast: exit status 1")
	}

	spawnCalled := false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawnCalled = true
		return nil, errors.New("unexpected spawn")
	}

	_, err = resolveTarget("gastown/toast", ResolveTargetOptions{Create: true, NoBoot: true})
	if err == nil {
		t.Fatal("expected resolve error for missing crew pane")
	}
	if spawnCalled {
		t.Fatal("crew shorthand must not spawn a polecat")
	}
}

func TestTargetRigDatabaseLookupFailsClosedWithoutTownRoot(t *testing.T) {
	t.Parallel()
	err := verifyBeadExistsInTargetRigDatabase("gt-r2405", "gastown", "")
	if err == nil {
		t.Fatal("expected fail-closed error without town root")
	}
	if !strings.Contains(err.Error(), "town root is unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRestoreRollbackRawWorkflowFieldsFromCurrentRestoresOriginalValues(t *testing.T) {
	current := strings.Join([]string{
		"no_merge: true",
		"review_only: true",
		"dispatched_by: mayor/",
		"",
		"Keep this body.",
	}, "\n")
	townRoot, _, descPath := setupMutableBDRawSlingTest(t, current)
	original := &beadInfo{Description: strings.Join([]string{
		"no_merge: true",
		"",
		"Original body.",
	}, "\n")}

	restoreRollbackRawWorkflowFieldsFromCurrent("gt-rawrollback", townRoot, filepath.Join(townRoot, "gastown", "polecats", "toast"), original)

	desc := readMutableBDDescription(t, descPath)
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	if fields == nil || !fields.NoMerge || fields.ReviewOnly {
		t.Fatalf("rollback did not restore original workflow values: %+v\n%s", fields, desc)
	}
	if !strings.Contains(desc, "dispatched_by: mayor/") || !strings.Contains(desc, "Keep this body.") {
		t.Fatalf("rollback did not preserve current metadata/body:\n%s", desc)
	}
}

func TestSlingFormulaRollsBackSpawnedPolecatOnWispFailure(t *testing.T) {
	townRoot := t.TempDir()

	// Minimal workspace marker so workspace.FindFromCwd() succeeds.
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// Register rig so IsRigName("gastown") succeeds.
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigs := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"gastown": {
				GitURL:    "git@github.com:test/gastown.git",
				LocalRepo: "",
				AddedAt:   time.Now().Truncate(time.Second),
				BeadsConfig: &config.BeadsConfig{
					Repo:   "local",
					Prefix: "gt-",
				},
			},
		},
	}
	if err := config.SaveRigsConfig(rigsPath, rigs); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir rig beads dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown"), 0755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}

	// Stub bd: cook succeeds; mol wisp fails to simulate missing required vars.
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
set -e
cmd="$1"
shift || true
case "$cmd" in
  cook)
    exit 0
    ;;
  mol)
    sub="$1"
    shift || true
    case "$sub" in
      wisp)
        echo "missing required vars" 1>&2
        exit 1
        ;;
    esac
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
set "cmd=%1"
set "sub=%2"
if "%cmd%"=="cook" exit /b 0
if "%cmd%"=="mol" (
  if "%sub%"=="wisp" (
    echo missing required vars 1>&2
    exit /b 1
  )
)
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Ensure we don't leak global flag/seam state across tests.
	prevNoBoot := slingNoBoot
	prevDryRun := slingDryRun
	prevSpawn := spawnPolecatForSling
	prevRollback := rollbackSlingArtifactsFn
	t.Cleanup(func() {
		slingNoBoot = prevNoBoot
		slingDryRun = prevDryRun
		spawnPolecatForSling = prevSpawn
		rollbackSlingArtifactsFn = prevRollback
	})

	slingDryRun = false
	slingNoBoot = true

	fakeWorkDir := filepath.Join(townRoot, "fake-polecat")
	if err := os.MkdirAll(fakeWorkDir, 0755); err != nil {
		t.Fatalf("mkdir fakeWorkDir: %v", err)
	}
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		return &SpawnedPolecatInfo{
			RigName:     rigName,
			PolecatName: "Toast",
			ClonePath:   fakeWorkDir,
		}, nil
	}

	rollbackCalled := false
	rollbackSlingArtifactsFn = func(spawnInfo *SpawnedPolecatInfo, beadID, hookWorkDir, convoyID string) {
		rollbackCalled = true
		if spawnInfo == nil || spawnInfo.PolecatName != "Toast" {
			t.Fatalf("unexpected spawnInfo in rollback: %+v", spawnInfo)
		}
		if beadID != "" {
			t.Fatalf("unexpected beadID in rollback: %q", beadID)
		}
		if hookWorkDir != fakeWorkDir {
			t.Fatalf("unexpected hookWorkDir in rollback: got %q want %q", hookWorkDir, fakeWorkDir)
		}
	}

	err = runSlingFormula(context.Background(), []string{"mol-anything", "gastown"})
	if err == nil {
		t.Fatalf("expected error from runSlingFormula")
	}
	if !rollbackCalled {
		t.Fatalf("expected rollbackSlingArtifactsFn to be called")
	}
}

func TestRunSlingFormulaPersistsVarContext(t *testing.T) {
	townRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}

	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
echo "$PWD|$*" >> "${BD_LOG}"
cmd="$1"
shift || true
case "$cmd" in
  formula)
    echo '{"name":"mol-anything"}'
    ;;
  cook)
    exit 0
    ;;
  list|query)
    echo '[]'
    ;;
  mol)
    sub="$1"
    shift || true
    case "$sub" in
      wisp)
        echo '{"new_epic_id":"gt-wisp-xyz"}'
        ;;
    esac
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo %CD%^|%*>>"%BD_LOG%"
set "cmd=%1"
set "sub=%2"
if "%cmd%"=="formula" (
  echo {"name":"mol-anything"}
  exit /b 0
)
if "%cmd%"=="list" (
  echo []
  exit /b 0
)
if "%cmd%"=="query" (
  echo []
  exit /b 0
)
if "%cmd%"=="cook" exit /b 0
if "%cmd%"=="mol" (
  if "%sub%"=="wisp" (
    echo {"new_epic_id":"gt-wisp-xyz"}
    exit /b 0
  )
)
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	attachedLogPath := filepath.Join(townRoot, "attached-molecule.log")
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", attachedLogPath)
	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevVars := slingVars
	prevDryRun := slingDryRun
	prevNoBoot := slingNoBoot
	prevRalph := slingRalph
	t.Cleanup(func() {
		slingVars = prevVars
		slingDryRun = prevDryRun
		slingNoBoot = prevNoBoot
		slingRalph = prevRalph
	})

	slingVars = []string{"version=1.2.3", "channel=stable"}
	slingDryRun = false
	slingNoBoot = true
	slingRalph = true

	if err := runSlingFormula(context.Background(), []string{"mol-anything"}); err != nil {
		t.Fatalf("runSlingFormula: %v", err)
	}

	attachmentBytes, err := os.ReadFile(attachedLogPath)
	if err != nil {
		t.Fatalf("read attachment log: %v", err)
	}
	attachment := string(attachmentBytes)

	if !strings.Contains(attachment, "attached_formula: mol-anything") {
		t.Fatalf("formula attachment missing from persisted description:\n%s", attachment)
	}
	if !strings.Contains(attachment, "version=1.2.3") || !strings.Contains(attachment, "channel=stable") {
		t.Fatalf("formula vars missing from persisted description:\n%s", attachment)
	}
	if !strings.Contains(attachment, "mode: ralph") {
		t.Fatalf("ralph mode missing from persisted standalone formula description:\n%s", attachment)
	}
}

func TestRunSlingFormulaNoOpWhenSameFormulaAlreadyHooked(t *testing.T) {
	townRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}

	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
echo "$PWD|$*" >> "${BD_LOG}"
cmd="$1"
shift || true
case "$cmd" in
  cook|mol|update)
    exit 0
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo %CD%^|%*>>"%BD_LOG%"
set "cmd=%1"
if "%cmd%"=="cook" exit /b 0
if "%cmd%"=="mol" exit /b 0
if "%cmd%"=="update" exit /b 0
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevDryRun := slingDryRun
	prevNoBoot := slingNoBoot
	prevForce := slingForce
	prevFindSingleton := findHookedFormulaSingletonFn
	t.Cleanup(func() {
		slingDryRun = prevDryRun
		slingNoBoot = prevNoBoot
		slingForce = prevForce
		findHookedFormulaSingletonFn = prevFindSingleton
	})

	slingDryRun = false
	slingNoBoot = true
	slingForce = false
	findHookedFormulaSingletonFn = func(workDir, targetAgent, formulaName string) (*beads.Issue, error) {
		return &beads.Issue{ID: "gt-wisp-existing"}, nil
	}

	if err := runSlingFormula(context.Background(), []string{"mol-anything"}); err != nil {
		t.Fatalf("runSlingFormula: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read bd log: %v", err)
	}
	log := string(logBytes)

	if strings.Contains(log, "cook ") || strings.Contains(log, "mol wisp") || strings.Contains(log, "update ") {
		t.Fatalf("expected same-formula sling to no-op before creating a new wisp, got:\n%s", log)
	}
}

func TestRunSlingFormulaUpdatesModeWhenSameFormulaAlreadyHooked(t *testing.T) {
	townRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
exit 0
`
	bdScriptWindows := `@echo off
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	attachedLogPath := filepath.Join(townRoot, "attached-molecule.log")
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", attachedLogPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevDryRun := slingDryRun
	prevNoBoot := slingNoBoot
	prevForce := slingForce
	prevRalph := slingRalph
	prevFindSingleton := findHookedFormulaSingletonFn
	t.Cleanup(func() {
		slingDryRun = prevDryRun
		slingNoBoot = prevNoBoot
		slingForce = prevForce
		slingRalph = prevRalph
		findHookedFormulaSingletonFn = prevFindSingleton
	})

	slingDryRun = false
	slingNoBoot = true
	slingForce = false
	slingRalph = false
	findHookedFormulaSingletonFn = func(workDir, targetAgent, formulaName string) (*beads.Issue, error) {
		return &beads.Issue{ID: "gt-wisp-existing", Description: "attached_formula: mol-anything\nmode: ralph"}, nil
	}

	if err := runSlingFormula(context.Background(), []string{"mol-anything"}); err != nil {
		t.Fatalf("runSlingFormula: %v", err)
	}

	attachmentBytes, err := os.ReadFile(attachedLogPath)
	if err != nil {
		t.Fatalf("read attachment log: %v", err)
	}
	if strings.Contains(string(attachmentBytes), "mode: ralph") {
		t.Fatalf("same-formula normal sling should clear stale ralph mode, got:\n%s", string(attachmentBytes))
	}
}

// TestSlingFormulaOnBeadPassesFeatureAndIssueVars verifies that when using
// gt sling <formula> --on <bead>, both --var feature=<title> and --var issue=<beadID>
// are passed to the canonical bd mol bond command.
func TestSlingFormulaOnBeadPassesFeatureAndIssueVars(t *testing.T) {
	townRoot := t.TempDir()

	// Minimal workspace marker so workspace.FindFromCwd() succeeds.
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// Create a rig path that owns gt-* beads, and a routes.jsonl pointing to it.
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatalf("mkdir rigDir: %v", err)
	}
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"gastown/mayor/rig"}`,
		`{"prefix":"hq-","path":"."}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	// Stub bd so we can observe the arguments passed to mol bond.
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	// The stub returns a specific title so we can verify it appears in --var feature=
	bdScript := `#!/bin/sh
set -e
echo "ARGS:$*" >> "${BD_LOG}"
cmd="$1"
shift || true
case "$cmd" in
  show)
    echo '[{"title":"My Test Feature","status":"open","assignee":"","description":""}]'
    ;;
  formula)
    # formula show <name> - must output something for verifyFormulaExists
    echo '{"name":"mol-review"}'
    exit 0
    ;;
  cook)
    exit 0
    ;;
	  mol)
		sub="$1"
		shift || true
		case "$sub" in
		  wisp)
			echo 'legacy mol wisp should not be called' >&2
			exit 1
			;;
		  bond)
			echo '{"result_id":"gt-abc123","id_mapping":{"mol-review":"gt-wisp-xyz"}}'
			;;
		esac
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo ARGS:%*>>"%BD_LOG%"
set "cmd=%1"
set "sub=%2"
if "%cmd%"=="show" (
  echo [{^"title^":^"My Test Feature^",^"status^":^"open^",^"assignee^":^"^",^"description^":^"^"}]
  exit /b 0
)
if "%cmd%"=="formula" (
  echo {^"name^":^"mol-review^"}
  exit /b 0
)
if "%cmd%"=="cook" exit /b 0
if "%cmd%"=="mol" (
  if "%sub%"=="wisp" (
    echo legacy mol wisp should not be called 1>&2
    exit /b 1
  )
  if "%sub%"=="bond" (
    echo {^"result_id^":^"gt-abc123^",^"id_mapping^":{^"mol-review^":^"gt-wisp-xyz^"}}
    exit /b 0
  )
)
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	attachedLogPath := filepath.Join(townRoot, "attached-molecule.log")
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", attachedLogPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "") // Prevent inheriting real tmux pane from test runner

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Ensure we don't leak global flag state across tests.
	prevOn := slingOnTarget
	prevVars := slingVars
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	t.Cleanup(func() {
		slingOnTarget = prevOn
		slingVars = prevVars
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
	})

	slingDryRun = false
	slingNoConvoy = true
	slingVars = nil
	slingOnTarget = "gt-abc123"

	// Prevent real tmux nudge from firing during tests (causes agent self-interruption)
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1") // Stub bd doesn't track state

	if err := runSling(nil, []string{"mol-review"}); err != nil {
		t.Fatalf("runSling: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}

	// Find the mol bond command and verify both --var arguments
	logLines := strings.Split(string(logBytes), "\n")
	var bondLine string
	for _, line := range logLines {
		if strings.Contains(line, "mol bond") {
			bondLine = line
			break
		}
	}

	if bondLine == "" {
		t.Fatalf("mol bond command not found in log: %s", string(logBytes))
	}

	// Verify --var feature=<title> is present
	if !containsVarArg(bondLine, "feature", "My Test Feature") {
		t.Errorf("mol bond missing --var feature=<title>\ngot: %s", bondLine)
	}

	// Verify --var issue=<beadID> is present
	if !containsVarArg(bondLine, "issue", "gt-abc123") {
		t.Errorf("mol bond missing --var issue=<beadID>\ngot: %s", bondLine)
	}
}

// TestVerifyBeadExistsAllowStale reproduces the bug in gtl-ncq where beads
// visible via regular bd show fail due to database staleness.
// The fix uses --allow-stale to skip the staleness check for existence verification.
func TestVerifyBeadExistsAllowStale(t *testing.T) {
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	townRoot := t.TempDir()

	// Create minimal workspace structure
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// Create a stub bd that always succeeds for "show" commands.
	// The real test is that verifyBeadExists calls bd show and parses
	// the output correctly. The --allow-stale flag may or may not be
	// present depending on BdSupportsAllowStale() cache state.
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
set -e
cmd="$1"
shift || true
# Strip --allow-stale if present (it's a global flag before subcommand)
if [ "$cmd" = "--allow-stale" ]; then
  cmd="$1"
  shift || true
fi
case "$cmd" in
  show)
    echo '[{"title":"Test bead","status":"open","assignee":""}]'
    ;;
  version)
    echo "bd 0.1.0"
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
set "cmd=%1"
if "%cmd%"=="--allow-stale" set "cmd=%2"
if "%cmd%"=="show" (
  echo [{"title":"Test bead","status":"open","assignee":""}]
  exit /b 0
)
if "%cmd%"=="version" (
  echo bd 0.1.0
  exit /b 0
)
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// EXPECTED: verifyBeadExists should use --allow-stale and succeed
	beadID := "jv-v599"
	err = verifyBeadExists(beadID)
	if err != nil {
		t.Errorf("verifyBeadExists(%q) failed: %v\nExpected --allow-stale to skip sync check", beadID, err)
	}
}

func TestBdCmdStripsUnsupportedAllowStale(t *testing.T) {
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	townRoot := t.TempDir()
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}

	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
for arg in "$@"; do
  if [ "$arg" = "--allow-stale" ]; then
    echo "Error: unknown flag: --allow-stale" >&2
    exit 1
  fi
done
case "$1" in
  show)
    echo '[{"title":"Routed bead","status":"open","assignee":""}]'
    ;;
  version)
    echo "bd version 1.0.3"
    ;;
  *)
    exit 1
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
>>"%BD_LOG%" echo %*
for %%A in (%*) do if "%%~A"=="--allow-stale" (
  echo Error: unknown flag: --allow-stale 1>&2
  exit /b 1
)
if "%1"=="show" (
  echo [{"title":"Routed bead","status":"open","assignee":""}]
  exit /b 0
)
if "%1"=="version" (
  echo bd version 1.0.3
  exit /b 0
)
exit /b 1
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := BdCmd("show", "gt-rca-epic-routing.3", "--json").
		AllowStale().
		Dir(townRoot).
		Output()
	if err != nil {
		t.Fatalf("BdCmd show with unsupported --allow-stale failed: %v", err)
	}
	if !strings.Contains(string(out), "Routed bead") {
		t.Fatalf("BdCmd show output = %q, want routed bead JSON", string(out))
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	log := string(logBytes)
	if strings.Contains(log, "show gt-rca-epic-routing.3 --json --allow-stale") {
		t.Fatalf("BdCmd passed unsupported --allow-stale to show command:\n%s", log)
	}
	if !strings.Contains(log, "show gt-rca-epic-routing.3 --json") {
		t.Fatalf("BdCmd did not run expected show command:\n%s", log)
	}
}

// TestLooksLikeBeadID tests the bead ID pattern recognition function.
// This ensures gt sling accepts bead IDs even when routing-based verification fails.
// Fixes: gt sling bd-ka761 failing with 'not a valid bead or formula'
//
// Note: looksLikeBeadID is a fallback check in sling. The actual sling flow is:
// 1. Try verifyBeadExists (routing-based lookup)
// 2. Try verifyFormulaExists (formula check)
// 3. Fall back to looksLikeBeadID pattern match
// So "mol-release" matches the pattern but won't be treated as bead in practice
// because it would be caught by formula verification first.
func TestLooksLikeBeadID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		// Valid bead IDs - should return true
		{"gt-abc123", true},
		{"bd-ka761", true},
		{"hq-cv-abc", true},
		{"ap-qtsup.16", true},
		{"beads-xyz", true},
		{"jv-v599", true},
		{"gt-9e8s5", true},
		{"hq-00gyg", true},

		// Short prefixes that match pattern (but may be formulas in practice)
		{"mol-release", true}, // 3-char prefix matches pattern (formula check runs first in sling)
		{"mol-abc123", true},  // 3-char prefix matches pattern

		// Non-bead strings - should return false
		{"formula-name", false}, // "formula" is 7 chars (> 5)
		{"mayor", false},        // no hyphen
		{"gastown", false},      // no hyphen
		{"deacon/dogs", false},  // contains slash
		{"", false},             // empty
		{"-abc", false},         // starts with hyphen
		{"GT-abc", false},       // uppercase prefix
		{"123-abc", false},      // numeric prefix
		{"a-", false},           // nothing after hyphen
		{"aaaaaa-b", false},     // prefix too long (6 chars)

		// Injection / invalid suffix characters - should return false
		{"gt-abc;rm -rf /", false}, // shell injection in suffix
		{"gt-abc$(cmd)", false},    // command substitution in suffix
		{"gt-abc&bg", false},       // ampersand in suffix
		{"gt-abc|pipe", false},     // pipe in suffix
		{"gt-abc`tick`", false},    // backtick in suffix
		{"gt-abc>redir", false},    // redirect in suffix
		{"gt-abc<redir", false},    // redirect in suffix
		{"gt-abc'quote", false},    // single quote in suffix
		{"gt-abc\"dquote", false},  // double quote in suffix
		{"gt-abc\\slash", false},   // backslash in suffix
		{"gt-abc xyz", false},      // space in suffix
		{"gt-ABC", false},          // uppercase in suffix
		{"gt-abc/path", false},     // slash in suffix
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := looksLikeBeadID(tt.input)
			if got != tt.want {
				t.Errorf("looksLikeBeadID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestSlingSetsDoltAutoCommitOff verifies that gt sling sets BD_DOLT_AUTO_COMMIT=off
// for all child bd processes. Under concurrent load (batch slinging), auto-commits
// from individual bd writes cause manifest contention and 'database is read only'
// errors. The Dolt server handles commits — individual auto-commits are unnecessary.
// Fixes: gt-u6n6a

// TestCheckCrossRigGuard verifies that cross-rig sling is rejected when a bead's
// prefix doesn't match the target rig. This prevents slinging beads-codebase issues
// to gastown polecats, which cannot fix code in a different rig's repo.
// Fixes: gt-myecw
func TestCheckCrossRigGuard(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"bd-","path":"beads/mayor/rig"}
{"prefix":"hq-","path":"."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		beadID      string
		targetAgent string
		wantErr     bool
	}{
		{
			name:        "same rig: gt bead to gastown polecat",
			beadID:      "gt-abc123",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     false,
		},
		{
			name:        "same rig: bd bead to beads polecat",
			beadID:      "bd-ka761",
			targetAgent: "beads/polecats/obsidian",
			wantErr:     false,
		},
		{
			name:        "cross-rig: bd bead to gastown polecat",
			beadID:      "bd-ka761",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     true,
		},
		{
			name:        "cross-rig: gt bead to beads polecat",
			beadID:      "gt-abc123",
			targetAgent: "beads/polecats/obsidian",
			wantErr:     true,
		},
		{
			// Known town-root prefix: warn but allow. A crew member with a broken
			// redirect chain may create hq-* beads that legitimately target a rig
			// polecat (gt-gbu). Hard-rejecting silently drops all their polecat work.
			name:        "town-level: hq bead to rig (warns but allows — gt-gbu)",
			beadID:      "hq-abc123",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     false,
		},
		{
			// Truly unknown prefix (not in routes.jsonl): hard reject.
			name:        "unknown prefix: rejected (no route exists at all)",
			beadID:      "xx-unknown",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     true,
		},
		{
			name:        "empty bead prefix: allowed",
			beadID:      "nohyphen",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCrossRigGuard(tc.beadID, tc.targetAgent, tmpDir)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkCrossRigGuard(%q, %q) error = %v, wantErr %v", tc.beadID, tc.targetAgent, err, tc.wantErr)
			}
			if err != nil && tc.wantErr {
				errMsg := err.Error()
				if !strings.Contains(errMsg, "cross-rig mismatch") && !strings.Contains(errMsg, "not in routes") {
					t.Errorf("expected cross-rig mismatch or unknown-prefix error, got: %v", err)
				}
				if !strings.Contains(errMsg, "--force") {
					t.Errorf("error should mention --force override, got: %v", err)
				}
				if !strings.Contains(errMsg, "bd create") {
					t.Errorf("error should mention bd create, got: %v", err)
				}
			}
		})
	}
}

func TestIsHookedAgentDead_UnknownFormat(t *testing.T) {
	t.Parallel()
	// Unknown assignee formats should return false (conservative)
	tests := []struct {
		name     string
		assignee string
	}{
		{"empty", ""},
		{"unknown_single", "foobar"},
		{"four_parts", "a/b/c/d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isHookedAgentDead(tt.assignee) {
				t.Errorf("isHookedAgentDead(%q) = true, want false (unknown format)", tt.assignee)
			}
		})
	}
}

func TestIsHookedAgentDead_NoTmuxSession(t *testing.T) {
	t.Parallel()
	// For a known assignee format where no tmux session exists,
	// isHookedAgentDead should return true (session is dead).
	// Use a highly unlikely polecat name to ensure no collision with real sessions.
	result := isHookedAgentDead("nonexistent_rig_xyz/polecats/ghost_polecat_999")
	// This might return true (no session) or false (tmux not available).
	// We just verify it doesn't panic.
	_ = result
}

func TestHookBeadWithRetryForcesAutoCommit(t *testing.T) {
	townRoot := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")

	bdScript := `#!/usr/bin/env sh
printf 'ENV:BD_DOLT_AUTO_COMMIT=%s|%s\n' "$BD_DOLT_AUTO_COMMIT" "$*" >> "$BD_LOG"
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo ENV:BD_DOLT_AUTO_COMMIT=%BD_DOLT_AUTO_COMMIT%^|%*>>"%BD_LOG%"
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_DOLT_AUTO_COMMIT", "off")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	if err := hookBeadWithRetry("gt-test123", "gastown/polecats/toast", townRoot); err != nil {
		t.Fatalf("hookBeadWithRetry: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "ENV:BD_DOLT_AUTO_COMMIT=on|") {
		t.Fatalf("hook update did not force auto-commit; log:\n%s", logText)
	}
}

func TestBuildSlingFieldUpdatesIncludesConvoyFields(t *testing.T) {
	t.Parallel()
	got := buildSlingFieldUpdates(
		"mayor",
		"review this",
		[]string{"feature=test"},
		"gt-wisp-test",
		"mol-polecat-work",
		false,
		false,
		"ralph",
		"feature=test",
		"hq-cv-test1",
		"local",
		true,
	)

	if got.ConvoyID != "hq-cv-test1" {
		t.Fatalf("ConvoyID = %q, want %q", got.ConvoyID, "hq-cv-test1")
	}
	if got.MergeStrategy != "local" {
		t.Fatalf("MergeStrategy = %q, want %q", got.MergeStrategy, "local")
	}
	if !got.ConvoyOwned {
		t.Fatal("ConvoyOwned = false, want true")
	}
	if got.Mode == nil || *got.Mode != "ralph" {
		t.Fatalf("Mode = %v, want ralph", got.Mode)
	}
}

func TestStoreFieldsInBeadConvoyFields(t *testing.T) {
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", filepath.Join(t.TempDir(), "mol.log"))
	logPath := os.Getenv("GT_TEST_ATTACHED_MOLECULE_LOG")

	if err := storeFieldsInBead("gt-test123", beadFieldUpdates{
		ConvoyID:      "hq-cv-test1",
		MergeStrategy: "local",
		ConvoyOwned:   true,
	}); err != nil {
		t.Fatalf("storeFieldsInBead: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "convoy_id: hq-cv-test1") {
		t.Fatalf("missing convoy_id in description:\n%s", text)
	}
	if !strings.Contains(text, "merge_strategy: local") {
		t.Fatalf("missing merge_strategy in description:\n%s", text)
	}
	if !strings.Contains(text, "convoy_owned: true") {
		t.Fatalf("missing convoy_owned in description:\n%s", text)
	}
}

func TestBeadFieldModeUpdateCanClearStaleRalphMode(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{Description: "attached_formula: mol-polecat-work\nmode: ralph"}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		t.Fatal("expected attachment fields")
	}
	mode := ""
	updates := beadFieldUpdates{Mode: &mode}
	if updates.Mode != nil {
		fields.Mode = *updates.Mode
	}
	desc := beads.SetAttachmentFields(issue, fields)
	if strings.Contains(desc, "mode: ralph") || strings.Contains(desc, "mode:") {
		t.Fatalf("expected stale ralph mode to be cleared, got:\n%s", desc)
	}
	if !strings.Contains(desc, "attached_formula: mol-polecat-work") {
		t.Fatalf("expected unrelated attachment fields preserved, got:\n%s", desc)
	}
}

func TestStoreFieldsInBeadFormulaSetsAttachedAt(t *testing.T) {
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", filepath.Join(t.TempDir(), "mol.log"))
	logPath := os.Getenv("GT_TEST_ATTACHED_MOLECULE_LOG")

	if err := storeFieldsInBead("gt-test123", beadFieldUpdates{
		AttachedFormula: "mol-dog-reaper",
	}); err != nil {
		t.Fatalf("storeFieldsInBead: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: string(body)})
	if fields == nil || fields.AttachedFormula != "mol-dog-reaper" || fields.AttachedAt == "" {
		t.Fatalf("formula attachment fields = %#v, want formula and attached_at", fields)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields.AttachedAt); err != nil {
		t.Fatalf("attached_at %q is not RFC3339Nano: %v", fields.AttachedAt, err)
	}
}

func TestStoreFieldsInBeadRawReviewRefreshesAttachedAt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw-review.log")
	t.Setenv("GT_TEST_ATTACHED_MOLECULE_LOG", logPath)

	stale := "2026-06-30T12:00:00Z"
	if err := os.WriteFile(logPath, []byte("attached_at: "+stale+"\nno_merge: true\nreview_only: true\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if err := storeFieldsInBead("gt-test123", beadFieldUpdates{
		NoMerge:    true,
		ReviewOnly: true,
	}); err != nil {
		t.Fatalf("storeFieldsInBead: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: string(body)})
	if fields == nil || !fields.NoMerge || !fields.ReviewOnly {
		t.Fatalf("raw review fields = %#v", fields)
	}
	if fields.AttachedAt == "" || fields.AttachedAt == stale {
		t.Fatalf("attached_at = %q, want refreshed from %q", fields.AttachedAt, stale)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields.AttachedAt); err != nil {
		t.Fatalf("attached_at %q is not RFC3339Nano: %v", fields.AttachedAt, err)
	}
}

// TestResolveTargetSelfSlingByPane verifies that a named target resolving to the
// caller's own tmux pane sets IsSelfSling=true (GH#3839). Without this, gt sling
// deacon (from the deacon itself) injects the ack prompt into the running agent's
// pane, wedging it mid-command.
func TestResolveTargetSelfSlingByPane(t *testing.T) {
	const callerPane = "%42"

	prev := resolveTargetAgentFn
	t.Cleanup(func() { resolveTargetAgentFn = prev })

	t.Run("named_target_same_pane_is_self_sling", func(t *testing.T) {
		resolveTargetAgentFn = func(_ string) (string, string, string, error) {
			return "deacon/", callerPane, "/home/deacon", nil
		}
		t.Setenv("TMUX_PANE", callerPane)

		result, err := resolveTarget("deacon", ResolveTargetOptions{})
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if !result.IsSelfSling {
			t.Error("expected IsSelfSling=true when named target pane matches caller pane")
		}
	})

	t.Run("named_target_different_pane_is_not_self_sling", func(t *testing.T) {
		resolveTargetAgentFn = func(_ string) (string, string, string, error) {
			return "deacon/", "%99", "/home/deacon", nil
		}
		t.Setenv("TMUX_PANE", callerPane)

		result, err := resolveTarget("deacon", ResolveTargetOptions{})
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if result.IsSelfSling {
			t.Error("expected IsSelfSling=false when named target pane differs from caller pane")
		}
	})

	t.Run("empty_pane_is_not_self_sling", func(t *testing.T) {
		resolveTargetAgentFn = func(_ string) (string, string, string, error) {
			return "deacon/", "", "/home/deacon", nil
		}
		t.Setenv("TMUX_PANE", callerPane)

		result, err := resolveTarget("deacon", ResolveTargetOptions{})
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if result.IsSelfSling {
			t.Error("expected IsSelfSling=false when resolved pane is empty (no tmux)")
		}
	})
}
