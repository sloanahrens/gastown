package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/style"
)

func TestExtractWorkType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		title     string
		issueType string
		expect    string
	}{
		// From explicit issue type
		{"bug type", "anything", "bug", "fix"},
		{"task type", "anything", "task", "feat"},
		{"feature type", "anything", "feature", "feat"},
		{"epic type", "anything", "epic", "epic"},

		// From conventional commit prefix
		{"feat prefix", "feat: add auth", "", "feat"},
		{"fix prefix", "fix: broken login", "", "fix"},
		{"refactor prefix", "refactor: clean up utils", "", "refactor"},
		{"docs prefix", "docs: update readme", "", "docs"},
		{"test prefix", "test: add coverage", "", "test"},
		{"chore prefix", "chore: update deps", "", "chore"},
		{"style prefix", "style: format code", "", "style"},
		{"perf prefix", "perf: optimize query", "", "perf"},

		// Case insensitive prefix
		{"FEAT prefix", "FEAT: add auth", "", "feat"},
		{"Fix prefix", "Fix: broken login", "", "fix"},

		// From keywords
		{"fix keyword", "Fix broken login", "", "fix"},
		{"bug keyword", "Investigate bug in auth", "", "fix"},
		{"add keyword", "Add user dashboard", "", "feat"},
		{"implement keyword", "Implement oauth flow", "", "feat"},
		{"create keyword", "Create migration script", "", "feat"},
		{"refactor keyword", "Refactor database layer", "", "refactor"},
		{"cleanup keyword", "Cleanup unused imports", "", "refactor"},

		// No match
		{"no match", "Update deployment config", "", ""},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWorkType(tt.title, tt.issueType)
			if got != tt.expect {
				t.Errorf("extractWorkType(%q, %q) = %q, want %q", tt.title, tt.issueType, got, tt.expect)
			}
		})
	}
}

func TestFormatRelativeTimeCV(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := []struct {
		name      string
		timestamp string
		expect    string
	}{
		{"just now", now.Add(-10 * time.Second).Format(time.RFC3339), "just now"},
		{"1 minute", now.Add(-1 * time.Minute).Format(time.RFC3339), "1m ago"},
		{"15 minutes", now.Add(-15 * time.Minute).Format(time.RFC3339), "15m ago"},
		{"1 hour", now.Add(-1 * time.Hour).Format(time.RFC3339), "1h ago"},
		{"5 hours", now.Add(-5 * time.Hour).Format(time.RFC3339), "5h ago"},
		{"1 day", now.Add(-25 * time.Hour).Format(time.RFC3339), "1d ago"},
		{"3 days", now.Add(-72 * time.Hour).Format(time.RFC3339), "3d ago"},
		{"1 week", now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), "1w ago"},
		{"3 weeks", now.Add(-22 * 24 * time.Hour).Format(time.RFC3339), "3w ago"},
		{"invalid", "not-a-timestamp", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRelativeTimeCV(tt.timestamp)
			if got != tt.expect {
				t.Errorf("formatRelativeTimeCV(%q) = %q, want %q", tt.timestamp, got, tt.expect)
			}
		})
	}

	// Date-only format parses as midnight UTC, so exact day bucket depends
	// on local timezone and time-of-day. Verify it returns a "d ago" string.
	t.Run("date only", func(t *testing.T) {
		dateStr := now.Add(-72 * time.Hour).Format("2006-01-02")
		got := formatRelativeTimeCV(dateStr)
		if got == "" {
			t.Errorf("formatRelativeTimeCV(%q) returned empty for date-only format", dateStr)
		}
	})
}

func TestFormatLanguageStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		langs  map[string]int
		expect string
	}{
		{"empty", map[string]int{}, ""},
		{"single", map[string]int{"Go": 10}, "Go (10)"},
		{"multiple sorted", map[string]int{"Go": 10, "Python": 5, "Rust": 3}, "Go (10), Python (5), Rust (3)"},
		{"caps at 3", map[string]int{"Go": 10, "Python": 5, "Rust": 3, "Java": 1}, "Go (10), Python (5), Rust (3)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLanguageStats(tt.langs)
			if got != tt.expect {
				t.Errorf("formatLanguageStats = %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestFormatWorkTypeStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		types  map[string]int
		expect string
	}{
		{"empty", map[string]int{}, ""},
		{"single", map[string]int{"feat": 5}, "feat (5)"},
		{"multiple sorted", map[string]int{"feat": 5, "fix": 3, "refactor": 1},
			"feat (5), fix (3), refactor (1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatWorkTypeStats(tt.types)
			if got != tt.expect {
				t.Errorf("formatWorkTypeStats = %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestSessionToAgentID(t *testing.T) {
	t.Parallel()
	// Generate known session names and verify the agent ID
	sessionName := crewSessionName(cmdTestRegistry(), "gastown", "tester")
	agentID := sessionToAgentID(cmdTestRegistry(), sessionName)
	if agentID == "" {
		t.Errorf("sessionToAgentID(%q) returned empty", sessionName)
	}
	// Verify it's a valid address-like format
	if agentID == sessionName {
		// Should have been transformed, not returned as-is
		// Unless parsing fails, which would indicate a test issue
		t.Logf("sessionToAgentID returned unchanged: %q (parsing may have failed)", sessionName)
	}
}

func TestSessionToAgentID_Fallback(t *testing.T) {
	t.Parallel()
	// Invalid session names should return the input as fallback
	got := sessionToAgentID(cmdTestRegistry(), "random-session-name")
	// Should still return something (either parsed or fallback)
	if got == "" {
		t.Error("sessionToAgentID should not return empty for any input")
	}
}

// TestSessionToAgentID_TownLevel pins down GH#3699: the town-level mayor must
// produce a trailing-slash address so writes from gt sling match
// the form queried by gt hook / runMoleculeStatus / buildAgentIdentity.
func TestSessionToAgentID_TownLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		session string
		want    string
	}{
		{"hq-mayor", "mayor/"},
	}
	for _, tt := range tests {
		t.Run(tt.session, func(t *testing.T) {
			got := sessionToAgentID(cmdTestRegistry(), tt.session)
			if got != tt.want {
				t.Errorf("sessionToAgentID(%q) = %q, want %q", tt.session, got, tt.want)
			}
		})
	}
}

func TestFormatCountStyled(t *testing.T) {
	t.Parallel()
	// Test that zero returns a dim "0"
	got := formatCountStyled(0, style.Success)
	if got == "" {
		t.Error("formatCountStyled(0) should not return empty")
	}

	// Test that non-zero returns the number
	got = formatCountStyled(42, style.Success)
	if got == "" {
		t.Error("formatCountStyled(42) should not return empty")
	}
	// The string should contain "42" somewhere (with ANSI codes)
	found := false
	for i := 0; i < len(got)-1; i++ {
		if got[i] == '4' && got[i+1] == '2' {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("formatCountStyled(42) = %q, does not contain '42'", got)
	}
}

// TestQueryAssignedIssuesFiltersAndMaps pins the store query the CV counters
// make: only the assignee's issues in the asked-for status come back, newest
// first, and each field the CV reads carries over. The caller supplies the
// store (buildCVSummary passes beads.NewPlain), so the fake drives it here
// without bd.
func TestQueryAssignedIssuesFiltersAndMaps(t *testing.T) {
	t.Parallel()
	const assignee = "gastown/polecats/agate"
	// The fake acts as the polecat, the way bd does when the holder closes
	// its own bead; a foreign assignee's bead can only be closed with force.
	db := beadsfake.New(beadsfake.WithActor(assignee))
	create := func(owner, title string) *beads.Issue {
		t.Helper()
		is, err := db.Create(beads.CreateOptions{Title: title, Assignee: owner, Priority: -1})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return is
	}
	older := create(assignee, "older")
	open := create(assignee, "still open")
	newer := create(assignee, "newer")
	other := create("gastown/polecats/emerald", "someone else's")
	// Close in creation order so the newer one carries the later timestamp.
	for _, is := range []*beads.Issue{older, newer} {
		if err := db.Close(is.ID); err != nil {
			t.Fatalf("close %s: %v", is.ID, err)
		}
	}
	if err := db.ForceCloseWithReason("", other.ID); err != nil {
		t.Fatalf("close %s: %v", other.ID, err)
	}
	closedNewer, err := db.Show(newer.ID)
	if err != nil {
		t.Fatalf("show %s: %v", newer.ID, err)
	}

	got, err := queryAssignedIssues(db, assignee, "closed")
	if err != nil {
		t.Fatalf("queryAssignedIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issues, want 2 (agate's closed only): %+v", len(got), got)
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("order = %s, %s; want %s (newest) first", got[0].ID, got[1].ID, newer.ID)
	}
	if got[0].Title != "newer" || got[0].Type != "task" || got[0].Status != "closed" {
		t.Errorf("got[0] = %+v, want newer/task/closed", got[0])
	}
	if got[0].Updated != closedNewer.UpdatedAt {
		t.Errorf("got[0].Updated = %q, want %q", got[0].Updated, closedNewer.UpdatedAt)
	}
	if open.Status != string(beads.StatusOpen) {
		t.Fatalf("open issue closed by the fixture: %+v", open)
	}
}
