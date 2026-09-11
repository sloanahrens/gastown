package feed

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupFakeBd installs a fake `bd` binary on PATH that logs every invocation
// (args + cwd) to BD_TEST_LOG and answers `list` calls based on which label
// filter was requested, using canned JSON from BD_TEST_MQ_JSON and
// BD_TEST_CONVOY_JSON. This lets tests assert both on what was returned and
// on exactly which bd calls were made (count and arguments) — the point of
// gt-3ony's fix is fewer, more targeted bd calls.
func setupFakeBd(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd script uses /bin/sh, not available on Windows")
	}

	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "bd-calls.log")

	script := `#!/bin/sh
echo "ARGS:$* CWD:$(pwd)" >> "` + logPath + `"

cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done

case "$cmd" in
  list)
    case " $* " in
      *" --label=gt:merge-request "*|*"--label=gt:merge-request"*)
        printf '%s' "$BD_TEST_MQ_JSON"
        ;;
      *"--label=gt:convoy"*)
        printf '%s' "$BD_TEST_CONVOY_JSON"
        ;;
      *)
        echo '[]'
        ;;
    esac
    exit 0
    ;;
  dep|show)
    echo '[]'
    exit 0
    ;;
  *)
    echo '[]'
    exit 0
    ;;
esac
`
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readLogLines(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath) //nolint:gosec // test fixture, path constructed by test
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading bd call log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestListConvoys_FiltersServerSideByLabelAndClosedAfter(t *testing.T) {
	logPath := setupFakeBd(t)
	t.Setenv("BD_TEST_CONVOY_JSON", `[{"id":"hq-conv1","title":"Convoy One","status":"closed","closed_at":"2026-09-10 12:00","issue_type":"convoy"}]`)

	beadsDir := t.TempDir()
	items, err := listConvoys(beadsDir, "closed", "2026-09-09T00:00:00Z")
	if err != nil {
		t.Fatalf("listConvoys() error: %v", err)
	}
	if len(items) != 1 || items[0].ID != "hq-conv1" {
		t.Fatalf("listConvoys() = %+v, want single hq-conv1", items)
	}

	lines := readLogLines(t, logPath)
	listCalls := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "ARGS:list ") {
			continue
		}
		listCalls++
		if !strings.Contains(line, "--label=gt:convoy") {
			t.Errorf("listConvoys() call missing --label=gt:convoy filter: %s", line)
		}
		if !strings.Contains(line, "--status=closed") {
			t.Errorf("listConvoys() call missing --status=closed: %s", line)
		}
		if !strings.Contains(line, "--closed-after=2026-09-09T00:00:00Z") {
			t.Errorf("listConvoys() call missing --closed-after filter: %s", line)
		}
	}
	if listCalls != 1 {
		t.Fatalf("expected exactly 1 bd list call, got %d: %v", listCalls, lines)
	}
}

func TestListConvoys_NoClosedAfterWhenOpen(t *testing.T) {
	logPath := setupFakeBd(t)
	t.Setenv("BD_TEST_CONVOY_JSON", `[]`)

	beadsDir := t.TempDir()
	if _, err := listConvoys(beadsDir, "open", ""); err != nil {
		t.Fatalf("listConvoys() error: %v", err)
	}

	lines := readLogLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 bd call, got %d: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "--closed-after") {
		t.Errorf("open-status query should not include --closed-after: %s", lines[0])
	}
}

func TestFetchMQEntries_OneCallPerRigWithCombinedStatus(t *testing.T) {
	logPath := setupFakeBd(t)
	t.Setenv("BD_TEST_MQ_JSON", `[{"id":"gt-mr-001","title":"polecat/nux/branch","status":"open","assignee":"gastown/polecats/nux"}]`)

	townRoot := t.TempDir()
	rigNames := []string{"rigA", "rigB", "rigC"}
	rigsJSON := `{"version":1,"rigs":{`
	for i, rig := range rigNames {
		if i > 0 {
			rigsJSON += ","
		}
		rigsJSON += `"` + rig + `":{"git_url":"https://github.com/example/` + rig + `.git","added_at":"2026-01-01T00:00:00Z"}`
		if err := os.MkdirAll(filepath.Join(townRoot, rig), 0755); err != nil {
			t.Fatalf("mkdir rig dir: %v", err)
		}
	}
	rigsJSON += `}}`

	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0600); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	entries := fetchMQEntries(townRoot)

	if len(entries) != len(rigNames) {
		t.Fatalf("fetchMQEntries() returned %d entries, want %d (one per rig): %+v", len(entries), len(rigNames), entries)
	}
	seenRigs := make(map[string]bool)
	for _, e := range entries {
		seenRigs[e.Rig] = true
	}
	for _, rig := range rigNames {
		if !seenRigs[rig] {
			t.Errorf("missing MQ entry for rig %s", rig)
		}
	}

	lines := readLogLines(t, logPath)
	mqCalls := 0
	for _, line := range lines {
		if !strings.Contains(line, "--label=gt:merge-request") {
			continue
		}
		mqCalls++
		if !strings.Contains(line, "--status=open,in_progress") {
			t.Errorf("expected combined status filter, got: %s", line)
		}
	}
	if mqCalls != len(rigNames) {
		t.Fatalf("expected exactly %d bd calls for merge-request beads (one per rig), got %d: %v", len(rigNames), mqCalls, lines)
	}
}
