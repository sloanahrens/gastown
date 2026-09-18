package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dog"
)

// =============================================================================
// Test Fixtures
// =============================================================================

// testDogManager creates a dog.Manager with a temporary town root for testing.
func testDogManager(t *testing.T) (*dog.Manager, string) {
	t.Helper()
	tmpDir := t.TempDir()

	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "git@github.com:test/gastown.git"},
			"beads":   {GitURL: "git@github.com:test/beads.git"},
		},
	}

	m := dog.NewManager(tmpDir, rigsConfig)
	return m, tmpDir
}

// setupTestDog creates a dog directory with a state file for testing.
func setupTestDog(t *testing.T, m *dog.Manager, townRoot, name string, state *dog.DogState) {
	t.Helper()

	dogPath := filepath.Join(townRoot, "deacon", "dogs", name)
	if err := os.MkdirAll(dogPath, 0755); err != nil {
		t.Fatalf("Failed to create dog dir: %v", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal state: %v", err)
	}

	statePath := filepath.Join(dogPath, ".dog.json")
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		t.Fatalf("Failed to write state file: %v", err)
	}
}

// =============================================================================
// Dog Name Detection from Path Tests
// =============================================================================

// TestDetectDogNameFromPath tests the path parsing logic used by runDogDone
// to auto-detect the dog name from the current working directory.
func TestDetectDogNameFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantName string
		wantOK   bool
	}{
		{
			name:     "dog worktree root",
			path:     "/Users/user/gt/deacon/dogs/alpha",
			wantName: "alpha",
			wantOK:   true,
		},
		{
			name:     "dog rig worktree",
			path:     "/Users/user/gt/deacon/dogs/alpha/gastown",
			wantName: "alpha",
			wantOK:   true,
		},
		{
			name:     "deep path in dog worktree",
			path:     "/Users/user/gt/deacon/dogs/bravo/beads/internal/cmd",
			wantName: "bravo",
			wantOK:   true,
		},
		{
			name:     "hyphenated dog name",
			path:     "/Users/user/gt/deacon/dogs/my-dog/gastown",
			wantName: "my-dog",
			wantOK:   true,
		},
		{
			name:     "numeric dog name",
			path:     "/Users/user/gt/deacon/dogs/dog123/beads",
			wantName: "dog123",
			wantOK:   true,
		},
		{
			name:     "not a dog path - polecat",
			path:     "/Users/user/gt/gastown/polecats/fixer/internal",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "not a dog path - crew",
			path:     "/Users/user/gt/gastown/crew/george/internal",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "deacon but not dogs directory",
			path:     "/Users/user/gt/deacon/boot",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "dogs without deacon parent",
			path:     "/Users/user/gt/some/dogs/alpha",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "empty path",
			path:     "",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "root path",
			path:     "/",
			wantName: "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOK := detectDogNameFromPath(tt.path)
			if gotName != tt.wantName {
				t.Errorf("detectDogNameFromPath(%q) name = %q, want %q", tt.path, gotName, tt.wantName)
			}
			if gotOK != tt.wantOK {
				t.Errorf("detectDogNameFromPath(%q) ok = %v, want %v", tt.path, gotOK, tt.wantOK)
			}
		})
	}
}

// detectDogNameFromPath extracts the dog name from a filesystem path.
// This mirrors the logic in runDogDone for testability.
// Returns the dog name and true if found, empty string and false otherwise.
func detectDogNameFromPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}

	// Use the same split logic as runDogDone
	parts := splitPathComponents(path)

	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "dogs" && i > 0 && parts[i-1] == "deacon" {
			return parts[i+1], true
		}
	}

	return "", false
}

// splitPath splits a path into its components.
func splitPath(path string) []string {
	// Clean and split the path
	path = filepath.Clean(path)
	var parts []string
	for {
		dir, file := filepath.Split(path)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" || dir == "/" || dir == path {
			break
		}
		path = filepath.Clean(dir)
	}
	return parts
}

// =============================================================================
// Dog Done Command Tests
// =============================================================================

// TestDogDone_AlreadyIdle verifies that dogDone handles the case where
// a dog is already idle gracefully.
func TestDogDone_AlreadyIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:       "alpha",
		State:      dog.StateIdle,
		Work:       "",
		LastActive: now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Get the dog and verify it's idle
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if d.State != dog.StateIdle {
		t.Errorf("State = %q, want %q", d.State, dog.StateIdle)
	}
	if d.Work != "" {
		t.Errorf("Work = %q, want empty", d.Work)
	}

	// ClearWork on already-idle dog should succeed without error
	if err := m.ClearWork("alpha"); err != nil {
		t.Fatalf("ClearWork() error = %v", err)
	}

	// Verify still idle
	d, _ = m.Get("alpha")
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
}

// TestDogDone_WorkingToIdle verifies that dogDone transitions a working
// dog back to idle state.
func TestDogDone_WorkingToIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:       "alpha",
		State:      dog.StateWorking,
		Work:       "hq-convoy-xyz",
		LastActive: now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Verify dog is working
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if d.State != dog.StateWorking {
		t.Errorf("Initial State = %q, want %q", d.State, dog.StateWorking)
	}
	if d.Work != "hq-convoy-xyz" {
		t.Errorf("Initial Work = %q, want 'hq-convoy-xyz'", d.Work)
	}

	// Clear work
	if err := m.ClearWork("alpha"); err != nil {
		t.Fatalf("ClearWork() error = %v", err)
	}

	// Verify now idle with no work
	d, _ = m.Get("alpha")
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
	if d.Work != "" {
		t.Errorf("After ClearWork: Work = %q, want empty", d.Work)
	}
}

// TestDogDone_NotFound verifies error handling for non-existent dog.
func TestDogDone_NotFound(t *testing.T) {
	m, _ := testDogManager(t)

	err := m.ClearWork("nonexistent")
	if err != dog.ErrDogNotFound {
		t.Errorf("ClearWork() error = %v, want ErrDogNotFound", err)
	}
}

// TestFormulaWispIDs_FiltersToAttachedFormula verifies the gt-da2x decision
// logic behind closeDogFormulaWisps: only hooked beads carrying
// attached_formula metadata (real `gt sling` formula wisps) are closed.
// Other hooked ephemeral beads on the dog's hook are unrelated work and must
// be left alone.
//
// The predicate lives in internal/beads (shared with the daemon's dispatch
// path); this pins that `gt dog done` selects exactly the same beads.
func TestFormulaWispIDs_FiltersToAttachedFormula(t *testing.T) {
	tests := []struct {
		name  string
		beads []*beads.Issue
		want  []string
	}{
		{
			name:  "no hooked beads",
			beads: nil,
			want:  nil,
		},
		{
			name: "hooked bead with no attachment metadata is not a wisp",
			beads: []*beads.Issue{
				{ID: "hq-task-a", Description: "just a regular hooked task"},
			},
			want: nil,
		},
		{
			name: "hooked bead with attached_formula is a wisp",
			beads: []*beads.Issue{
				{ID: "wisp-a", Description: "attached_formula: mol-dog-reaper\nattached_molecule: wisp-a\n"},
			},
			want: []string{"wisp-a"},
		},
		{
			name: "only formula wisps are selected, in query order",
			beads: []*beads.Issue{
				{ID: "hq-task-b", Description: "unrelated work"},
				{ID: "wisp-b", Description: "attached_formula: mol-dog-backup\n"},
				{ID: "wisp-c", Description: "attached_molecule: wisp-c\nattached_at: 2026-01-01T00:00:00Z\n"},
				{ID: "wisp-d", Description: "attached_formula: mol-polecat-work\n"},
			},
			want: []string{"wisp-b", "wisp-d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := beads.FormulaWispIDs(tt.beads)
			if len(got) != len(tt.want) {
				t.Fatalf("FormulaWispIDs() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("FormulaWispIDs() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// setupDogDoneWispTest points the process at a fixture town and installs a
// mock bd that records its argv to logPath before running body, so
// closeDogFormulaWisps' real subprocess path runs without a bd/Dolt backend.
func setupDogDoneWispTest(t *testing.T, body string) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script; skipping on Windows")
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("creating mayor dir: %v", err)
	}
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("creating .beads dir: %v", err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
	t.Chdir(townRoot)

	binDir := t.TempDir()
	logPath = filepath.Join(t.TempDir(), "bd.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" + body
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestCloseDogFormulaWisps_ClosesHookedFormulaWisp is the gt-da2x regression
// at the `gt dog done` end. The bug was a dog that went idle while its formula
// wisp stayed hooked, making it invisible to dispatch — so this covers the
// real body, not the predicate: it must close the wisp root AND its steps
// (children first), and leave a plain hooked bead alone.
func TestCloseDogFormulaWisps_ClosesHookedFormulaWisp(t *testing.T) {
	logPath := setupDogDoneWispTest(t, `
if [ "$1" = "query" ]; then
  echo '[{"id":"hq-task","status":"hooked","description":"unrelated"},{"id":"hq-wisp-admuv","status":"hooked","description":"attached_formula: mol-dog-reaper\n"}]'
  exit 0
fi
if [ "$1" = "show" ]; then
  case "$2" in
    hq-wisp-admuv)
      echo '{"hq-wisp-admuv":[{"id":"hq-wisp-admuv.1","status":"closed"},{"id":"hq-wisp-admuv.2","status":"open"}],"schema_version":1}'
      exit 0
      ;;
  esac
  echo '{"schema_version":1}'
  exit 0
fi
exit 0
`)

	closed, err := closeDogFormulaWisps("alpha")
	if err != nil {
		t.Fatalf("closeDogFormulaWisps: %v", err)
	}
	// The root and the one still-open step; the already-closed step is skipped.
	if closed != 2 {
		t.Errorf("closeDogFormulaWisps() = %d, want 2", closed)
	}

	calls := readBdCalls(t, logPath)
	for _, want := range []string{
		"show hq-wisp-admuv --children --json",
		"close hq-wisp-admuv.2 hq-wisp-admuv --force --reason dog done",
	} {
		if !containsCall(calls, want) {
			t.Errorf("fake bd calls %q missing; got %q", want, calls)
		}
	}
	if containsCall(calls, "close hq-wisp-admuv.1 hq-wisp-admuv.2 hq-wisp-admuv --force --reason dog done") {
		t.Errorf("fake bd calls = %q, want the already-closed step left out of the close", calls)
	}
}

// TestCloseDogFormulaWisps_IgnoresNonFormulaHooks confirms `gt dog done` does
// not force-close a hooked bead that is not a formula wisp — that hook may be
// unrelated work the dog is still holding.
func TestCloseDogFormulaWisps_IgnoresNonFormulaHooks(t *testing.T) {
	logPath := setupDogDoneWispTest(t, `
if [ "$1" = "query" ]; then
  echo '[{"id":"hq-task","status":"hooked","description":"someone elses work"}]'
  exit 0
fi
exit 0
`)

	closed, err := closeDogFormulaWisps("alpha")
	if err != nil {
		t.Fatalf("closeDogFormulaWisps: %v", err)
	}
	if closed != 0 {
		t.Errorf("closeDogFormulaWisps() = %d, want 0", closed)
	}
	for _, call := range readBdCalls(t, logPath) {
		if strings.HasPrefix(call, "close ") {
			t.Errorf("fake bd calls = %q, want no close: no hooked bead carried attached_formula", call)
		}
	}
}

// TestCloseDogFormulaWisps_NoWorkspaceIsNoOp pins the guard: run outside a
// Gas Town workspace, `gt dog done` must not go looking for beads to close.
func TestCloseDogFormulaWisps_NoWorkspaceIsNoOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script; skipping on Windows")
	}
	logPath := filepath.Join(t.TempDir(), "bd.log")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DIR", "")
	t.Chdir(t.TempDir()) // no mayor/ anywhere up the tree

	closed, err := closeDogFormulaWisps("alpha")
	if err != nil {
		t.Fatalf("closeDogFormulaWisps: %v", err)
	}
	if closed != 0 {
		t.Errorf("closeDogFormulaWisps() = %d, want 0 outside a workspace", closed)
	}
	if calls := readBdCalls(t, logPath); len(calls) != 0 {
		t.Errorf("fake bd calls = %q, want none outside a workspace", calls)
	}
}

// containsCall reports whether want is one of the recorded bd argv lines.
func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

// readBdCalls returns the argv lines a setupDogDoneWispTest fake bd recorded.
func readBdCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading fake bd log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// =============================================================================
// Dog Clear Tests
// =============================================================================

// TestDogClear_WorkingToIdle verifies that dogClear transitions a working
// dog back to idle state.
func TestDogClear_WorkingToIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:       "alpha",
		State:      dog.StateWorking,
		Work:       constants.MolConvoyFeed,
		LastActive: now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Verify dog is working
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if d.State != dog.StateWorking {
		t.Errorf("Initial State = %q, want %q", d.State, dog.StateWorking)
	}

	// Clear the dog (simulates gt dog clear alpha)
	err = m.ClearWork("alpha")
	if err != nil {
		t.Fatalf("ClearWork() error = %v", err)
	}

	// Verify dog is now idle
	d, err = m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() after clear error = %v", err)
	}
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
	if d.Work != "" {
		t.Errorf("After ClearWork: Work = %q, want empty", d.Work)
	}
}

// TestDogClear_AlreadyIdle verifies that dogClear handles the case where
// a dog is already idle gracefully.
func TestDogClear_AlreadyIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:       "alpha",
		State:      dog.StateIdle,
		Work:       "",
		LastActive: now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Get the dog and verify it's idle
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if d.State != dog.StateIdle {
		t.Errorf("Initial State = %q, want %q", d.State, dog.StateIdle)
	}

	// ClearWork on an already idle dog should succeed (idempotent)
	err = m.ClearWork("alpha")
	if err != nil {
		t.Errorf("ClearWork() on idle dog error = %v, want nil", err)
	}

	// Verify dog is still idle
	d, err = m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() after clear error = %v", err)
	}
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
}

// TestDogClear_NotFound verifies error handling for non-existent dog.
func TestDogClear_NotFound(t *testing.T) {
	m, _ := testDogManager(t)

	err := m.ClearWork("nonexistent")
	if err != dog.ErrDogNotFound {
		t.Errorf("ClearWork() error = %v, want ErrDogNotFound", err)
	}
}

// =============================================================================
// Path Splitting Tests
// =============================================================================

func TestSplitPath(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{
			path: "/Users/user/gt/deacon/dogs/alpha",
			want: []string{"Users", "user", "gt", "deacon", "dogs", "alpha"},
		},
		{
			path: "/a/b/c",
			want: []string{"a", "b", "c"},
		},
		{
			path: "relative/path",
			want: []string{"relative", "path"},
		},
		{
			path: "/",
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := splitPath(tt.path)
			if len(got) != len(tt.want) {
				t.Errorf("splitPath(%q) = %v (len %d), want %v (len %d)",
					tt.path, got, len(got), tt.want, len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitPath(%q)[%d] = %q, want %q",
						tt.path, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// =============================================================================
// Dog Format Time Ago Tests
// =============================================================================

func TestDogFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		want   string
	}{
		{"just now", 30 * time.Second, "just now"},
		{"1 minute ago", 1 * time.Minute, "1 minute ago"},
		{"5 minutes ago", 5 * time.Minute, "5 minutes ago"},
		{"1 hour ago", 1 * time.Hour, "1 hour ago"},
		{"3 hours ago", 3 * time.Hour, "3 hours ago"},
		{"1 day ago", 24 * time.Hour, "1 day ago"},
		{"5 days ago", 5 * 24 * time.Hour, "5 days ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testTime := time.Now().Add(-tt.offset)
			got := dogFormatTimeAgo(testTime)
			if got != tt.want {
				t.Errorf("dogFormatTimeAgo(%v ago) = %q, want %q", tt.offset, got, tt.want)
			}
		})
	}
}

func TestDogFormatTimeAgo_ZeroTime(t *testing.T) {
	got := dogFormatTimeAgo(time.Time{})
	if got != "(unknown)" {
		t.Errorf("dogFormatTimeAgo(zero) = %q, want '(unknown)'", got)
	}
}
