package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// writeTestRigsConfig sets up a fake town root at dir with mayor/rigs.json
// registering the given rig names, plus a directory for each rig.
func writeTestRigsConfig(t *testing.T, townRoot string, rigNames ...string) {
	t.Helper()

	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}

	var entries []string
	for _, name := range rigNames {
		rigDir := filepath.Join(townRoot, name)
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatalf("mkdir rig %s: %v", name, err)
		}
		entries = append(entries, fmt.Sprintf(`"%s": {"git_url": "https://example.com/%s.git", "beads": {"prefix": "%s"}}`, name, name, name[:2]))
	}

	rigsJSON := fmt.Sprintf(`{"version": 1, "rigs": {%s}}`, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}
}

func TestGetTTL(t *testing.T) {
	t.Parallel()
	ttls := defaultTTLs

	tests := []struct {
		wispType string
		want     time.Duration
	}{
		{"heartbeat", 6 * time.Hour},
		{"ping", 6 * time.Hour},
		{"patrol", 24 * time.Hour},
		{"gc_report", 24 * time.Hour},
		{"error", 7 * 24 * time.Hour},
		{"recovery", 7 * 24 * time.Hour},
		{"escalation", 7 * 24 * time.Hour},
		{"default", 24 * time.Hour},
		{"", 24 * time.Hour},        // empty falls back to default
		{"unknown", 24 * time.Hour}, // unknown falls back to default
	}

	for _, tc := range tests {
		t.Run(tc.wispType, func(t *testing.T) {
			got := getTTL(ttls, tc.wispType)
			if got != tc.want {
				t.Errorf("getTTL(%q) = %v, want %v", tc.wispType, got, tc.want)
			}
		})
	}
}

func TestWispAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		updatedAt string
		wantAge   time.Duration
		wantErr   bool
	}{
		{
			name:      "RFC3339",
			updatedAt: "2026-02-07T06:00:00Z",
			wantAge:   6 * time.Hour,
		},
		{
			name:      "one day old",
			updatedAt: "2026-02-06T12:00:00Z",
			wantAge:   24 * time.Hour,
		},
		{
			name:      "invalid",
			updatedAt: "not-a-date",
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &beads.Issue{UpdatedAt: tc.updatedAt}
			got, err := wispAge(w, now)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantAge {
				t.Errorf("wispAge = %v, want %v", got, tc.wantAge)
			}
		})
	}
}

func TestHasKeepLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"no labels", nil, false},
		{"other labels", []string{"bug", "urgent"}, false},
		{"keep label", []string{"keep"}, true},
		{"gt:keep label", []string{"bug", "gt:keep"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &beads.Issue{Labels: tc.labels}
			if got := hasKeepLabel(w); got != tc.want {
				t.Errorf("hasKeepLabel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasComments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		count int
		want  bool
	}{
		{"no comments", 0, false},
		{"has comments", 3, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &beads.Issue{CommentCount: tc.count}
			if got := hasComments(w); got != tc.want {
				t.Errorf("hasComments = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsReferenced(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		depCnt  int
		deptCnt int
		want    bool
	}{
		{"no refs", 0, 0, false},
		{"has dependents", 0, 1, true},
		{"has dependencies", 1, 0, true},
		{"both", 2, 3, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &beads.Issue{
				DependencyCount: tc.depCnt,
				DependentCount:  tc.deptCnt,
			}
			if got := isReferenced(w); got != tc.want {
				t.Errorf("isReferenced = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompactTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"short ASCII", "short", 10, "short"},
		{"exact length", "exactly10!", 10, "exactly10!"},
		{"ASCII too long", "this is too long", 10, "this is..."},
		{"short maxLen", "ab", 3, "ab"},
		{"maxLen 3", "abcdef", 3, "abc"},
		// Multi-byte UTF-8: emoji is 1 rune, not 4 bytes
		{"emoji within limit", "🤝 HANDOFF", 10, "🤝 HANDOFF"},
		{"emoji truncated", "🤝 HANDOFF: Routine cycle for witness", 15, "🤝 HANDOFF: R..."},
		// CJK characters: each is 1 rune, 3 bytes
		{"CJK within limit", "日本語テスト", 10, "日本語テスト"},
		{"CJK truncated", "日本語テストデータ", 6, "日本語..."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactTruncate(tc.s, tc.maxLen); got != tc.want {
				t.Errorf("compactTruncate(%q, %d) = %q, want %q", tc.s, tc.maxLen, got, tc.want)
			}
		})
	}
}

func TestExtractJSONArray(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			"clean JSON array",
			`[{"id":"test"}]`,
			`[{"id":"test"}]`,
		},
		{
			"warning prefix before JSON",
			"Warning: no route found for prefix \"gt-\"\n[{\"id\":\"test\"}]",
			`[{"id":"test"}]`,
		},
		{
			"unicode warning prefix",
			"⚠ Warning: something with 🤝 emoji\n[{\"id\":\"test\"}]",
			`[{"id":"test"}]`,
		},
		{
			"no array in data",
			"just some text without json",
			"just some text without json",
		},
		{
			"empty data",
			"",
			"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(extractJSONArray([]byte(tc.data)))
			if got != tc.want {
				t.Errorf("extractJSONArray(%q) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

// recordingListClient captures the ListOptions a caller passes, so a test can
// pin the filter without a real bd.
type recordingListClient struct {
	beads.Client
	opts beads.ListOptions
}

func (c *recordingListClient) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.opts = opts
	return c.Client.List(opts)
}

// listWisps must reach the wisp plane, which bd's default list view omits:
// a seeded wisp comes back, a durable issue does not, and a closed wisp
// survives the status filter (gt-ekep1).
func TestListWispsReadsTheWispPlane(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-wisp-open", Title: "heartbeat", Status: "open", Ephemeral: true, WispType: "heartbeat"},
		beads.Issue{ID: "gt-wisp-closed", Title: "done patrol", Status: "closed", Ephemeral: true, WispType: "patrol"},
		beads.Issue{ID: "gt-task", Title: "durable work", Status: "open"},
	)

	wisps, err := listWisps(db)
	if err != nil {
		t.Fatalf("listWisps: %v", err)
	}

	got := make(map[string]bool, len(wisps))
	for _, w := range wisps {
		got[w.ID] = true
	}
	if len(wisps) != 2 || !got["gt-wisp-open"] || !got["gt-wisp-closed"] {
		t.Fatalf("listWisps = %v, want the two wisps and no durable issue", ids(wisps))
	}
}

// ids names issues by ID for a failure message.
func ids(issues []*beads.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.ID)
	}
	return out
}

// A priority the caller never set must not narrow the read: -1 is
// ListOptions' no-filter value, and 0 would keep only P0 wisps (gt-ekep1).
func TestListWispsLeavesPriorityUnfiltered(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-wisp-p0", Status: "open", Ephemeral: true, Priority: 0},
		beads.Issue{ID: "gt-wisp-p4", Status: "open", Ephemeral: true, Priority: 4},
	)
	rec := &recordingListClient{Client: db}

	wisps, err := listWisps(rec)
	if err != nil {
		t.Fatalf("listWisps: %v", err)
	}
	if rec.opts.Priority != -1 {
		t.Errorf("List Priority = %d, want -1 (no filter)", rec.opts.Priority)
	}
	if !rec.opts.Ephemeral || rec.opts.Status != "all" {
		t.Errorf("List opts = %+v, want Ephemeral with Status all", rec.opts)
	}
	if len(wisps) != 2 {
		t.Fatalf("listWisps = %v, want both priorities", ids(wisps))
	}
}

func TestLoadTTLConfigDefaults(t *testing.T) {
	t.Parallel()
	// With empty town root, should return defaults
	ttls := loadTTLConfig("", "")

	if ttls["heartbeat"] != 6*time.Hour {
		t.Errorf("heartbeat TTL = %v, want 6h", ttls["heartbeat"])
	}
	if ttls["patrol"] != 24*time.Hour {
		t.Errorf("patrol TTL = %v, want 24h", ttls["patrol"])
	}
	if ttls["error"] != 7*24*time.Hour {
		t.Errorf("error TTL = %v, want 168h", ttls["error"])
	}
}

func TestLoadTTLConfigWithRoleDefaults(t *testing.T) {
	t.Parallel()
	// With empty town root, should return hardcoded defaults
	ttls := loadTTLConfigWithRole("", "")

	for k, want := range defaultTTLs {
		if got := ttls[k]; got != want {
			t.Errorf("loadTTLConfigWithRole TTLs[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestLoadTTLConfigWithRoleSkipsInvalidPaths(t *testing.T) {
	t.Parallel()
	// With a failing rig bead lookup, the TTLs fall back to the defaults
	townRoot := t.TempDir()
	useFailingTownBD(t, townRoot)
	ttls := loadTTLConfigWithRole(townRoot, "myrig")

	// Should still have defaults even though lookups failed
	if ttls["patrol"] != defaultTTLs["patrol"] {
		t.Errorf("patrol TTL = %v, want %v", ttls["patrol"], defaultTTLs["patrol"])
	}
	if ttls["error"] != defaultTTLs["error"] {
		t.Errorf("error TTL = %v, want %v", ttls["error"], defaultTTLs["error"])
	}
}

// TestResolveCompactTargetsReachesAllRegisteredRigs is the regression test
// for gt-vee: a bare "gt compact" run from a directory that resolves to the
// town-level database (e.g. the deacon's ~/gt/deacon) must still reach every
// registered rig's database, not just the ambient one.
func TestResolveCompactTargetsReachesAllRegisteredRigs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, "gastown", "beads")

	// Simulate the deacon: cwd is inside the town root but not any rig.
	deaconDir := filepath.Join(townRoot, "deacon")
	if err := os.MkdirAll(deaconDir, 0755); err != nil {
		t.Fatalf("mkdir deacon: %v", err)
	}

	targets, err := resolveCompactTargets(townRoot, deaconDir, "")
	if err != nil {
		t.Fatalf("resolveCompactTargets: %v", err)
	}

	labels := make(map[string]string) // label -> workDir
	for _, tgt := range targets {
		labels[tgt.label] = tgt.workDir
	}

	if labels["current"] != deaconDir {
		t.Errorf("expected ambient target %q, got %q", deaconDir, labels["current"])
	}
	if got, want := labels["gastown"], filepath.Join(townRoot, "gastown"); got != want {
		t.Errorf("expected gastown rig target %q, got %q", want, got)
	}
	if got, want := labels["beads"], filepath.Join(townRoot, "beads"); got != want {
		t.Errorf("expected beads rig target %q, got %q", want, got)
	}
	if len(targets) != 3 {
		t.Errorf("expected 3 targets (current + 2 rigs), got %d: %+v", len(targets), targets)
	}
}

// TestResolveCompactTargetsDedupesAmbientRig ensures that running "gt
// compact" from inside a registered rig's own directory doesn't compact
// that rig's database twice.
func TestResolveCompactTargetsDedupesAmbientRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, "gastown", "beads")

	gastownDir := filepath.Join(townRoot, "gastown")

	targets, err := resolveCompactTargets(townRoot, gastownDir, "")
	if err != nil {
		t.Fatalf("resolveCompactTargets: %v", err)
	}

	count := 0
	for _, tgt := range targets {
		if filepath.Clean(tgt.workDir) == filepath.Clean(gastownDir) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected gastown's database to appear exactly once, appeared %d times: %+v", count, targets)
	}
	if len(targets) != 2 {
		t.Errorf("expected 2 targets (gastown + beads), got %d: %+v", len(targets), targets)
	}
}

// TestResolveCompactTargetsExplicitRig ensures --rig scopes to only that rig.
func TestResolveCompactTargetsExplicitRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, "gastown", "beads")

	targets, err := resolveCompactTargets(townRoot, townRoot, "beads")
	if err != nil {
		t.Fatalf("resolveCompactTargets: %v", err)
	}

	if len(targets) != 1 {
		t.Fatalf("expected exactly 1 target, got %d: %+v", len(targets), targets)
	}
	if targets[0].label != "beads" || targets[0].workDir != filepath.Join(townRoot, "beads") {
		t.Errorf("unexpected target: %+v", targets[0])
	}
}

// TestResolveCompactTargetsNoTownRoot falls back to just the ambient target.
func TestResolveCompactTargetsNoTownRoot(t *testing.T) {
	t.Parallel()
	targets, err := resolveCompactTargets("", "/some/dir", "")
	if err != nil {
		t.Fatalf("resolveCompactTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].workDir != "/some/dir" {
		t.Fatalf("expected single ambient target, got %+v", targets)
	}
}

func TestResolveRigPathNotFound(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, "gastown")

	if _, err := resolveRigPath(townRoot, "nonexistent"); err == nil {
		t.Error("expected error for unregistered rig, got nil")
	}
}

func TestResolveRigPathNoTownRoot(t *testing.T) {
	t.Parallel()
	if _, err := resolveRigPath("", "gastown"); err == nil {
		t.Error("expected error when town root is unknown, got nil")
	}
}
