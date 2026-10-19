package cmd

import (
	"errors"
	"fmt"
	"strings"
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

// TestSessionToAgentID_TownLevel pins the town-level session forms. A session
// name converts to the identity's own address, with no trailing slash: GH#3699
// made the retired mayor's address slashed, and the rule went with the role
// (gt-rwp7z.11). The overseer is the only town-level session left; a leftover
// hq-mayor name is not a seat and falls back to itself (gt-rwp7z.17).
func TestSessionToAgentID_TownLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		session string
		want    string
	}{
		{"hq-overseer", "overseer"},
		{"hq-mayor", "hq-mayor"}, // retired with its role: no address, falls back
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

// TestQueryAssignedIssuesReturnsEveryRowNotJustTheFirstPage: the CV reads all of
// an assignee's rows. Asking bd for 50 stopped the summary at an arbitrary page
// boundary, so a polecat with a long history reported a truncated record of it
// (gt-u3hc1).
func TestQueryAssignedIssuesReturnsEveryRowNotJustTheFirstPage(t *testing.T) {
	t.Parallel()
	const assignee = "gastown/polecats/agate"
	const rows = 55 // one page more than bd's default 50
	db := beadsfake.New(beadsfake.WithActor(assignee))

	var newest string
	for i := 0; i < rows; i++ {
		is, err := db.Create(beads.CreateOptions{
			Title: fmt.Sprintf("job %d", i), Assignee: assignee, Priority: -1,
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		newest = is.ID
		if err := db.Close(is.ID); err != nil {
			t.Fatalf("close %s: %v", is.ID, err)
		}
	}

	got, err := queryAssignedIssues(db, assignee, "closed")
	if err != nil {
		t.Fatalf("queryAssignedIssues: %v", err)
	}
	if len(got) != rows {
		t.Fatalf("got %d issues, want %d: a capped query drops the rest of the CV", len(got), rows)
	}
	if got[0].ID != newest {
		t.Errorf("got[0].ID = %s, want %s (newest first)", got[0].ID, newest)
	}
}

// TestIdentityRenameCloseFailureRestoresHook: the hook transfer runs before the
// old identity closes, so a failed close must put the work back. Otherwise it
// stays assigned to a name whose identity bead the failed run closes, and a
// retry refuses at moveLiveHook's holder guard for good (gt-fkz54).
func TestIdentityRenameCloseFailureRestoresHook(t *testing.T) {
	t.Parallel()
	const (
		oldAssignee = "gastown/polecats/agate"
		newAssignee = "gastown/polecats/basalt"
		oldBeadID   = "gt-gastown-polecat-agate"
		newBeadID   = "gt-gastown-polecat-basalt"
		hookBead    = "gt-hook4"
	)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{ID: oldBeadID, Title: "Polecat agate in gastown", Status: string(beads.StatusOpen)})
	db.Seed(beads.Issue{
		ID: hookBead, Title: "work", Status: string(beads.IssueStatusHooked), Assignee: oldAssignee,
	})
	sentinel := errors.New("dolt unreachable")
	faulty := closeFailsClient{Client: db, id: oldBeadID, err: sentinel}
	fields := &beads.AgentFields{RoleType: "polecat", Rig: "gastown", HookBead: hookBead}

	err := renameIdentityBeads(faulty, oldBeadID, newBeadID, "Polecat basalt in gastown",
		fields, oldAssignee, newAssignee)
	if !errors.Is(err, sentinel) {
		t.Fatalf("renameIdentityBeads = %v, want %v wrapped", err, sentinel)
	}

	got, err := db.Show(hookBead)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != oldAssignee {
		t.Errorf("assignee = %q, want %q: the failed close must leave the hook where it found it", got.Assignee, oldAssignee)
	}
	if got.Status != string(beads.IssueStatusHooked) {
		t.Errorf("status = %q, want the hooked status it had", got.Status)
	}
	// The failed attempt leaves nothing behind for the retry to trip over.
	newBead, err := db.Show(newBeadID)
	if err != nil {
		t.Fatal(err)
	}
	if newBead.Status != string(beads.StatusClosed) {
		t.Errorf("new identity status = %q, want closed once the hook is back on the old name", newBead.Status)
	}
}

// closeFailsClient is a beads.Client whose CloseWithReason reports err for one
// ID and delegates the rest, so a test can fail exactly the close a rename
// makes on the old identity.
type closeFailsClient struct {
	beads.Client
	id  string
	err error
}

func (c closeFailsClient) CloseWithReason(reason string, ids ...string) error {
	for _, id := range ids {
		if id == c.id {
			return c.err
		}
	}
	return c.Client.CloseWithReason(reason, ids...)
}

// transferFailsClient fails TransferIfAssignee when the expected assignee
// matches, so a test can fail exactly the rollback a rename makes after a
// failed close. refuse reports the claim as no longer held instead of erroring.
type transferFailsClient struct {
	beads.Client
	expected string
	refuse   bool
	err      error
}

func (c transferFailsClient) TransferIfAssignee(id, expected, status, assignee string) (bool, error) {
	if expected == c.expected {
		if c.refuse {
			return false, nil
		}
		return false, c.err
	}
	return c.Client.TransferIfAssignee(id, expected, status, assignee)
}

// TestIdentityRenameRestoreFailureLeavesNewIdentityOpen: when the old identity
// will not close and the hook cannot be rolled back, both failures surface and
// the new identity stays open, so the name the work is assigned to still has one
// (gt-fkz54).
func TestIdentityRenameRestoreFailureLeavesNewIdentityOpen(t *testing.T) {
	t.Parallel()
	const (
		oldAssignee = "gastown/polecats/agate"
		newAssignee = "gastown/polecats/basalt"
		oldBeadID   = "gt-gastown-polecat-agate"
		newBeadID   = "gt-gastown-polecat-basalt"
		hookBead    = "gt-hook5"
	)
	closeSentinel := errors.New("dolt unreachable")
	restoreSentinel := errors.New("dolt still unreachable")

	tests := []struct {
		name        string
		refuse      bool
		rollbackErr error
		want        string
	}{
		{name: "rollback errors", rollbackErr: restoreSentinel, want: "dolt still unreachable"},
		{name: "claim no longer held", refuse: true, want: "no longer held"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			db.Seed(beads.Issue{ID: oldBeadID, Title: "Polecat agate in gastown", Status: string(beads.StatusOpen)})
			db.Seed(beads.Issue{
				ID: hookBead, Title: "work", Status: string(beads.IssueStatusHooked), Assignee: oldAssignee,
			})
			faulty := transferFailsClient{
				Client:   closeFailsClient{Client: db, id: oldBeadID, err: closeSentinel},
				expected: newAssignee,
				refuse:   tt.refuse,
				err:      tt.rollbackErr,
			}
			fields := &beads.AgentFields{RoleType: "polecat", Rig: "gastown", HookBead: hookBead}

			err := renameIdentityBeads(faulty, oldBeadID, newBeadID, "Polecat basalt in gastown",
				fields, oldAssignee, newAssignee)
			if !errors.Is(err, closeSentinel) {
				t.Fatalf("renameIdentityBeads = %v, want the close failure %v", err, closeSentinel)
			}
			if tt.rollbackErr != nil && !errors.Is(err, tt.rollbackErr) {
				t.Errorf("renameIdentityBeads = %v, want the rollback failure %v", err, tt.rollbackErr)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to say %q", err, tt.want)
			}

			got, err := db.Show(hookBead)
			if err != nil {
				t.Fatal(err)
			}
			if got.Assignee != newAssignee {
				t.Errorf("assignee = %q, want %q: the failed rollback leaves the work on the new name", got.Assignee, newAssignee)
			}
			newBead, err := db.Show(newBeadID)
			if err != nil {
				t.Fatal(err)
			}
			if newBead.Status != string(beads.StatusOpen) {
				t.Errorf("new identity status = %q, want open: its name still holds the hook", newBead.Status)
			}
			oldBead, err := db.Show(oldBeadID)
			if err != nil {
				t.Fatal(err)
			}
			if oldBead.Status != string(beads.StatusOpen) {
				t.Errorf("old identity status = %q, want open: its close is what failed", oldBead.Status)
			}
		})
	}
}

// TestMoveLiveHookReassignsWorkHeldUnderTheOldName: rename inherits the hook, so
// the work bead's assignee has to follow the new name. Copying the reference
// alone left the renamed identity claiming a bead still assigned to the old one
// (gt-u3hc1).
func TestMoveLiveHookReassignsWorkHeldUnderTheOldName(t *testing.T) {
	t.Parallel()
	const (
		oldAssignee = "gastown/polecats/agate"
		newAssignee = "gastown/polecats/basalt"
		hookBead    = "gt-hook1"
	)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID: hookBead, Title: "work", Status: string(beads.IssueStatusHooked), Assignee: oldAssignee,
	})

	if _, err := moveLiveHook(db, hookBead, oldAssignee, newAssignee); err != nil {
		t.Fatalf("moveLiveHook: %v", err)
	}
	got, err := db.Show(hookBead)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != newAssignee {
		t.Errorf("assignee = %q, want %q", got.Assignee, newAssignee)
	}
	if got.Status != string(beads.IssueStatusHooked) {
		t.Errorf("status = %q, want the hooked status it had", got.Status)
	}
}

// TestMoveLiveHookRefusesWhatItCannotShowIsTheRenamedPolecatsWork: in-progress
// work moves too, the caller's session check having established the holder is
// gone, but a reference that cannot be read, or a claim held by someone else,
// aborts the rename rather than being adopted by the new name.
func TestMoveLiveHookRefusesWhatItCannotShowIsTheRenamedPolecatsWork(t *testing.T) {
	t.Parallel()
	const (
		oldAssignee = "gastown/polecats/agate"
		newAssignee = "gastown/polecats/basalt"
	)
	newDB := func() *beadsfake.Fake {
		return beadsfake.New(beadsfake.WithPrefix("gt"), beadsfake.WithActor(oldAssignee))
	}

	t.Run("in-progress claim moves", func(t *testing.T) {
		t.Parallel()
		db := newDB()
		db.Seed(beads.Issue{ID: "gt-hook2", Title: "work", Status: string(beads.StatusInProgress), Assignee: oldAssignee})
		if _, err := moveLiveHook(db, "gt-hook2", oldAssignee, newAssignee); err != nil {
			t.Fatalf("moveLiveHook: %v", err)
		}
		got, err := db.Show("gt-hook2")
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignee != newAssignee {
			t.Errorf("assignee = %q, want %q", got.Assignee, newAssignee)
		}
	})

	t.Run("terminal reference carries as-is", func(t *testing.T) {
		t.Parallel()
		db := newDB()
		db.Seed(beads.Issue{ID: "gt-done1", Title: "shipped", Status: string(beads.StatusClosed), Assignee: oldAssignee})
		if _, err := moveLiveHook(db, "gt-done1", oldAssignee, newAssignee); err != nil {
			t.Fatalf("moveLiveHook: %v", err)
		}
		got, err := db.Show("gt-done1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignee != oldAssignee {
			t.Errorf("assignee = %q, want %q untouched: a closed bead is nobody's live work", got.Assignee, oldAssignee)
		}
	})

	t.Run("unreadable reference is refused", func(t *testing.T) {
		t.Parallel()
		if _, err := moveLiveHook(newDB(), "gt-gone1", oldAssignee, newAssignee); err == nil {
			t.Error("moveLiveHook on a missing hook bead = nil, want a refusal")
		}
	})

	t.Run("claim held by another polecat is refused", func(t *testing.T) {
		t.Parallel()
		db := newDB()
		db.Seed(beads.Issue{ID: "gt-hook3", Title: "someone else's", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/emerald"})
		_, err := moveLiveHook(db, "gt-hook3", oldAssignee, newAssignee)
		if err == nil {
			t.Fatal("moveLiveHook on another polecat's claim = nil, want a refusal")
		}
		if !strings.Contains(err.Error(), "emerald") {
			t.Errorf("error = %v, want it to name the holder", err)
		}
	})

	t.Run("no hook is a no-op", func(t *testing.T) {
		t.Parallel()
		if _, err := moveLiveHook(newDB(), "", oldAssignee, newAssignee); err != nil {
			t.Errorf("moveLiveHook(\"\") = %v, want nil", err)
		}
	})
}

// TestEnsureNewIdentityAbsentReportsALookupError: the rename's "is the new name
// free" check reads the bead and must believe only a real answer. Swallowing the
// error let a transient failure look like "no bead there", and the create that
// followed overwrote the description of a live identity (gt-u3hc1).
func TestEnsureNewIdentityAbsentReportsALookupError(t *testing.T) {
	t.Parallel()
	const newBeadID = "gt-gastown-polecat-basalt"

	t.Run("failed lookup aborts", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New(beadsfake.WithPrefix("gt"))
		sentinel := errors.New("dolt unreachable")
		faulty := showFailsClient{Client: db, id: newBeadID, err: sentinel}

		if err := ensureNewIdentityAbsent(faulty, newBeadID); !errors.Is(err, sentinel) {
			t.Errorf("ensureNewIdentityAbsent = %v, want %v wrapped", err, sentinel)
		}
	})

	t.Run("live identity is refused", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New(beadsfake.WithPrefix("gt"))
		db.Seed(beads.Issue{
			ID: newBeadID, Title: "Polecat basalt", Status: string(beads.StatusOpen),
			Labels:      []string{"gt:agent"},
			Description: beads.FormatAgentDescription("Polecat basalt", &beads.AgentFields{RoleType: "polecat", Rig: "gastown"}),
		})
		if err := ensureNewIdentityAbsent(db, newBeadID); err == nil {
			t.Error("ensureNewIdentityAbsent on a live bead = nil, want a refusal")
		}
	})

	t.Run("closed identity is reusable", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New(beadsfake.WithPrefix("gt"))
		db.Seed(beads.Issue{
			ID: newBeadID, Title: "Polecat basalt", Status: string(beads.StatusClosed),
			Labels:      []string{"gt:agent"},
			Description: beads.FormatAgentDescription("Polecat basalt", &beads.AgentFields{RoleType: "polecat", Rig: "gastown"}),
		})
		if err := ensureNewIdentityAbsent(db, newBeadID); err != nil {
			t.Errorf("ensureNewIdentityAbsent on a closed bead = %v, want nil: a nuked identity is reused", err)
		}
	})
}

// failFirstShowClient fails one ID's first Show and delegates every later read,
// the shape of a transient error: the lookup that should refuse the write fails
// while the rewrite that follows can still reach the bead.
type failFirstShowClient struct {
	beads.Client
	id   string
	err  error
	seen bool
}

func (c *failFirstShowClient) Show(id string) (*beads.Issue, error) {
	if id == c.id && !c.seen {
		c.seen = true
		return nil, c.err
	}
	return c.Client.Show(id)
}

// TestIdentityAddReturnsReadError: gt polecat identity add must believe only a
// real answer to "is this identity free". Swallowing the lookup error let a
// transient failure look like absence, and the CreateOrReopenAgentBead that
// followed rewrote the existing bead's description with hook_bead and
// cleanup_status cleared (gt-gfcz8).
func TestIdentityAddReturnsReadError(t *testing.T) {
	t.Parallel()
	const beadID = "gt-gastown-polecat-basalt"
	title := "Polecat basalt in gastown"

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	existing := &beads.AgentFields{
		RoleType:      "polecat",
		Rig:           "gastown",
		AgentState:    "working",
		HookBead:      "gt-work1",
		CleanupStatus: "clean",
	}
	original := beads.FormatAgentDescription(title, existing)
	db.Seed(beads.Issue{
		ID:          beadID,
		Title:       title,
		Status:      string(beads.StatusOpen),
		Labels:      []string{"gt:agent"},
		Description: original,
	})

	sentinel := errors.New("dolt unreachable")
	faulty := &failFirstShowClient{Client: db, id: beadID, err: sentinel}

	_, err := addPolecatIdentity(faulty, beadID, title, &beads.AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "idle",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("addPolecatIdentity on a failed read = %v, want %v wrapped", err, sentinel)
	}

	got, err := db.Show(beadID)
	if err != nil {
		t.Fatalf("reading the existing bead: %v", err)
	}
	if got.Description != original {
		t.Errorf("description after a failed read was rewritten:\n%s\nwant it untouched:\n%s", got.Description, original)
	}
}

// TestAddPolecatIdentityCreatesOnlyAFreeID: the add path's two settled answers.
// An ID with no bead is created, and an ID a live identity holds is refused
// with that identity's description left as it was (gt-gfcz8).
func TestAddPolecatIdentityCreatesOnlyAFreeID(t *testing.T) {
	t.Parallel()
	const beadID = "gt-gastown-polecat-basalt"
	title := "Polecat basalt in gastown"
	fields := &beads.AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "idle",
	}

	t.Run("absent identity is created", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New(beadsfake.WithPrefix("gt"))

		issue, err := addPolecatIdentity(db, beadID, title, fields)
		if err != nil {
			t.Fatalf("addPolecatIdentity on a free ID = %v, want nil", err)
		}
		if issue == nil || issue.ID != beadID {
			t.Fatalf("addPolecatIdentity = %+v, want the bead %s", issue, beadID)
		}
		got, err := db.Show(beadID)
		if err != nil {
			t.Fatalf("reading the created bead: %v", err)
		}
		if got.Title != title {
			t.Errorf("title = %q, want %q", got.Title, title)
		}
	})

	t.Run("live identity is refused and left untouched", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New(beadsfake.WithPrefix("gt"))
		original := beads.FormatAgentDescription(title, &beads.AgentFields{
			RoleType:      "polecat",
			Rig:           "gastown",
			AgentState:    "working",
			HookBead:      "gt-work1",
			CleanupStatus: "clean",
		})
		db.Seed(beads.Issue{
			ID:          beadID,
			Title:       title,
			Status:      string(beads.StatusOpen),
			Labels:      []string{"gt:agent"},
			Description: original,
		})

		_, err := addPolecatIdentity(db, beadID, title, fields)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("addPolecatIdentity on a live ID = %v, want an already-exists refusal", err)
		}

		got, err := db.Show(beadID)
		if err != nil {
			t.Fatalf("reading the existing bead: %v", err)
		}
		if got.Description != original {
			t.Errorf("description of a live identity was rewritten:\n%s\nwant it untouched:\n%s", got.Description, original)
		}
	})
}
