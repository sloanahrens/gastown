package doctor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// mockSocketLister implements socketSessionLister for testing.
type mockSocketLister struct {
	sessions []string
	listErr  error
	killed   []string
	killErr  error
}

func (m *mockSocketLister) ListSessions() ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.sessions, nil
}

func (m *mockSocketLister) KillSessionWithProcesses(name string) error {
	m.killed = append(m.killed, name)
	return m.killErr
}

// --- Run() tests ---

func TestSocketSplitBrainCheck_EmptySocket(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = ""

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "no split-brain possible") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestSocketSplitBrainCheck_DefaultSocket(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "default"

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "no split-brain possible") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestSocketSplitBrainCheck_NoTownServer(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{listErr: fmt.Errorf("no server running")}
	check.defaultListerForTest = &mockSocketLister{sessions: []string{}}

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected Skipped, got %v: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestSocketSplitBrainCheck_NoDefaultServer(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{sessions: []string{"ga-crew-max"}}
	check.defaultListerForTest = &mockSocketLister{listErr: fmt.Errorf("no server running")}

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected Skipped, got %v: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestSocketSplitBrainCheck_NoDuplicates(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{sessions: []string{"ga-crew-max"}}
	check.defaultListerForTest = &mockSocketLister{sessions: []string{"personal-stuff"}}

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "No split-brain") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestSocketSplitBrainCheck_DetectsDuplicates(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{sessions: []string{"ga-crew-max"}}
	check.defaultListerForTest = &mockSocketLister{sessions: []string{"ga-crew-max"}}

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Fatalf("expected Error, got %v: %s", result.Status, result.Message)
	}
	if len(result.Details) == 0 {
		t.Fatal("expected details to be non-empty")
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "DUPLICATE") && strings.Contains(d, "ga-crew-max") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected DUPLICATE detail for ga-crew-max, got: %v", result.Details)
	}
}

func TestSocketSplitBrainCheck_DetectsOrphans(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{sessions: []string{}}
	check.defaultListerForTest = &mockSocketLister{sessions: []string{"ga-nux"}}

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Fatalf("expected Error, got %v: %s", result.Status, result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "ORPHAN") && strings.Contains(d, "ga-nux") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ORPHAN detail for ga-nux, got: %v", result.Details)
	}
}

func TestSocketSplitBrainCheck_MixedWithNonGastown(t *testing.T) {
	t.Parallel()

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{sessions: []string{}}
	check.defaultListerForTest = &mockSocketLister{sessions: []string{"ga-crew-max", "personal-stuff"}}

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Fatalf("expected Error, got %v: %s", result.Status, result.Message)
	}

	// ga-crew-max should be flagged as orphan
	hasGaWitness := false
	for _, d := range result.Details {
		if strings.Contains(d, "ga-crew-max") {
			hasGaWitness = true
		}
		if strings.Contains(d, "personal-stuff") {
			t.Errorf("personal-stuff should be ignored, but appeared in details: %v", result.Details)
		}
	}
	if !hasGaWitness {
		t.Errorf("expected ga-crew-max in details, got: %v", result.Details)
	}

	// Only 1 stale session (personal-stuff is ignored)
	if len(check.staleSessions) != 1 {
		t.Errorf("expected 1 stale session, got %d: %v", len(check.staleSessions), check.staleSessions)
	}
}

// --- Fix() tests ---

func TestSocketSplitBrainCheck_Fix_NoStale(t *testing.T) {
	t.Parallel()
	check := NewSocketSplitBrainCheck()
	mock := &mockSocketLister{}
	check.defaultListerForTest = mock

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if len(mock.killed) != 0 {
		t.Errorf("expected no kills, got: %v", mock.killed)
	}
}

func TestSocketSplitBrainCheck_Fix_KillsStale(t *testing.T) {
	t.Parallel()
	check := NewSocketSplitBrainCheck()
	check.staleSessions = []string{"ga-nux", "ga-crew-max"}

	mock := &mockSocketLister{}
	check.defaultListerForTest = mock

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	if len(mock.killed) != 2 {
		t.Fatalf("expected 2 kills, got %d: %v", len(mock.killed), mock.killed)
	}
	// staleSessions is sorted, so kills should be in order
	if mock.killed[0] != "ga-nux" || mock.killed[1] != "ga-crew-max" {
		t.Errorf("unexpected kill order: %v", mock.killed)
	}
}

// TestSocketSplitBrainCheck_AgreesWithLegacyCleanup runs one table of
// default-socket names through both kill paths: gt doctor's split-brain check
// (Run, then Fix, over fake listers) and the legacy default-socket cleanup
// (session.CleanupDefaultSocketForTest, over a scripted tmux). Each must kill
// exactly the table's stale rows, so neither can start deciding for itself
// (gt-2n5go).
func TestSocketSplitBrainCheck_AgreesWithLegacyCleanup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		onDefault []string // sessions on the "default" socket
		onTown    []string // sessions on the town socket, making the same names duplicates
		want      []string // what both paths must kill
	}{
		{
			name: "town roles, the live overseer, rig sessions and names that are not town sessions",
			onDefault: []string{
				"hq-mayor", "hq-deacon", "hq-boot", "hq-dog-alpha", "hq-overseer",
				"ga-witness", "ga-crew-max",
				"gt-unregistered", "bd-thing", "gthq-thing", "personal-stuff", "dotfiles-main",
			},
			onTown: []string{"hq-overseer", "ga-witness", "ga-crew-max"},
			want:   []string{"ga-crew-max", "ga-witness", "hq-boot", "hq-deacon", "hq-dog-alpha", "hq-mayor"},
		},
		{
			name:      "the live overseer, duplicated on the town socket",
			onDefault: []string{"hq-overseer"},
			onTown:    []string{"hq-overseer"},
		},
		{
			name:      "the overseer with no live copy",
			onDefault: []string{"hq-overseer"},
		},
		{
			name:      "rig sessions only",
			onDefault: []string{"ga-nux", "ga-crew-max"},
			onTown:    []string{"ga-nux"},
			want:      []string{"ga-crew-max", "ga-nux"},
		},
		{
			name:      "names that are not town sessions",
			onDefault: []string{"gt-unregistered", "bd-thing", "gthq-thing", "personal-stuff", "dotfiles-main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := session.NewPrefixRegistry()
			reg.Register("ga", "gastown")

			want := slices.Clone(tt.want)
			slices.Sort(want)

			if got := socketSplitBrainKills(t, reg, tt.onTown, tt.onDefault); !slices.Equal(got, want) {
				t.Errorf("gt doctor killed %v, want %v", got, want)
			}
			if got := legacyCleanupKills(t, reg, tt.onDefault); !slices.Equal(got, want) {
				t.Errorf("legacy cleanup killed %v, want %v", got, want)
			}
		})
	}
}

// socketSplitBrainKills returns, sorted, what the split-brain check's Fix kills
// when onDefault sits on the "default" socket and onTown on the town socket.
func socketSplitBrainKills(t *testing.T, reg *session.PrefixRegistry, onTown, onDefault []string) []string {
	t.Helper()
	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-town"
	check.townListerForTest = &mockSocketLister{sessions: onTown}
	defaultLister := &mockSocketLister{sessions: onDefault}
	check.defaultListerForTest = defaultLister

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: reg}
	if result := check.Run(ctx); result.Status != StatusOK && result.Status != StatusError {
		t.Fatalf("Run() = %v: %s %v", result.Status, result.Message, result.Details)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	kills := slices.Clone(defaultLister.killed)
	slices.Sort(kills)
	return kills
}

// legacyCleanupKills returns, sorted, what the legacy cleanup's own kill loop
// kills when onDefault sits on the "default" socket: the real
// CleanupLegacyDefaultSocket path, with tmux answering through its exec seam.
func legacyCleanupKills(t *testing.T, reg *session.PrefixRegistry, onDefault []string) []string {
	t.Helper()
	remaining := slices.Clone(onDefault)
	var killed []string
	execFn := func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		switch {
		case slices.Contains(args, "list-sessions"):
			return []byte(strings.Join(remaining, "\n")), nil, nil
		case slices.Contains(args, "kill-session"):
			name := strings.TrimPrefix(args[len(args)-1], "=")
			killed = append(killed, name)
			remaining = slices.DeleteFunc(remaining, func(s string) bool { return s == name })
			return nil, nil, nil
		}
		// display-message, set-option and set-hook: the session has no pane to
		// read, so KillSessionWithProcesses falls back to kill-session.
		return nil, []byte("can't find pane"), errors.New("exit status 1")
	}

	cleaned := session.CleanupDefaultSocketForTest(reg, "gt-town", func(socket string) *tmux.Tmux {
		return tmux.NewTmuxWithExec(socket, execFn)
	})
	if cleaned != len(killed) {
		t.Fatalf("cleanup reported %d cleaned, killed %v", cleaned, killed)
	}
	slices.Sort(killed)
	return killed
}

// gt doctor's split-brain check used to kill every IsKnownSession match on the
// default socket, the live overseer included. The shared predicate narrows
// that: the overseer is running, and unregistered gt-, bd- and gthq- names are
// not town sessions. Nothing here is stale (gt-2n5go).
func TestSocketSplitBrainCheck_LeavesLiveAndUnregisteredSessions(t *testing.T) {
	t.Parallel()

	reg := session.NewPrefixRegistry()
	reg.Register("ga", "gastown")

	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.townListerForTest = &mockSocketLister{}
	mock := &mockSocketLister{sessions: []string{
		"hq-overseer", "gt-unregistered", "bd-thing", "gthq-thing", "personal-stuff",
	}}
	check.defaultListerForTest = mock

	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: reg}
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected OK, got %v: %s %v", result.Status, result.Message, result.Details)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if len(mock.killed) != 0 {
		t.Errorf("killed %v, want none", mock.killed)
	}
}
