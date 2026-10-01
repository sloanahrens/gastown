package tmux

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// processTree is a fakeServer whose session gt-x runs a shell (the pane
// process) with a child node and grandchild claude, plus:
//   - reparented: in the pane's process group but orphaned to init (PPID 1)
//   - groupmate:  in the pane's process group with a live unrelated parent
//   - stranger:   in its own group
func processTree() (f *fakeServer, pane, node, claude, reparented, groupmate, stranger string) {
	f = newFakeServer()
	s := f.addSession("gt-x", "bash")
	pane = s.panes[0].pid
	node = f.spawn(pane, "node")
	claude = f.spawn(node, "claude")
	reparented = f.spawn(pane, "sleep")
	f.setProc(reparented, "1", pane)
	parent := f.spawn("1", "launchd-job")
	groupmate = f.spawn(parent, "sleep")
	f.setProc(groupmate, parent, pane)
	stranger = f.spawn("1", "sleep")
	return
}

func psExec(f *fakeServer) execFunc { return newScripted(f.answer).exec }

func TestHasDescendantWithNames(t *testing.T) {
	t.Parallel()
	f, pane, _, _, _, _, _ := processTree()
	ex := psExec(f)
	if hasDescendantWithNames(ex, "999999999", []string{"node", "claude"}, 0) {
		t.Error("hasDescendantWithNames should return false for nonexistent PID")
	}
	if hasDescendantWithNames(ex, pane, []string{}, 0) || hasDescendantWithNames(ex, pane, nil, 0) {
		t.Error("hasDescendantWithNames should return false for empty names")
	}
	if !hasDescendantWithNames(ex, pane, []string{"claude"}, 0) {
		t.Error("hasDescendantWithNames missed a grandchild")
	}
	if hasDescendantWithNames(ex, pane, []string{"launchd-job"}, 0) {
		t.Error("hasDescendantWithNames matched a process outside the tree")
	}
}

func TestGetAllDescendants(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, reparented, _, _ := processTree()
	ex := psExec(f)
	if got := getAllDescendants(ex, "999999999"); len(got) != 0 {
		t.Errorf("getAllDescendants(nonexistent) = %v, want empty", got)
	}
	got := getAllDescendants(ex, pane)
	// Deepest first: claude before node. The reparented process is no
	// longer a child in the tree.
	if want := []string{claude, node}; !reflect.DeepEqual(got, want) {
		t.Errorf("getAllDescendants = %v, want %v (reparented %s excluded)", got, want, reparented)
	}
}

func TestDescendantsFromPS(t *testing.T) {
	t.Parallel()

	snapshot := []byte(`
1 0 root
2 1 bash
3 1 zsh
4 2 node
5 4 claude
2 1 bash-duplicate
bad header row
8 bad nope
1 5 root-cycle
7 99 node
`)

	got := descendantsFromPS("1", snapshot)
	want := []string{"5", "4", "2", "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descendantsFromPS deepest-first = %v, want %v", got, want)
	}

	if got := descendantsFromPS("42", snapshot); len(got) != 0 {
		t.Fatalf("descendantsFromPS missing root = %v, want empty", got)
	}

	if got := descendantsFromPS("not-a-pid", snapshot); len(got) != 0 {
		t.Fatalf("descendantsFromPS invalid root = %v, want empty", got)
	}
}

func TestHasDescendantWithNamesFromPS(t *testing.T) {
	t.Parallel()

	snapshot := []byte(`
1 0 bash
2 1 /usr/local/bin/node
3 2 /Users/peter/bin/claude
4 1 tmux: client
5 1 claude-helper
6 5 nodejs
7 99 node
8 0 opencode
9 8 /opt/homebrew/bin/bun
10 9 opencode
1 10 root-cycle
bad header row
11 bad nope
`)

	tests := []struct {
		name  string
		pid   string
		names []string
		depth int
		want  bool
	}{
		{name: "direct child basename", pid: "1", names: []string{"node"}, want: true},
		{name: "grandchild basename path", pid: "1", names: []string{"claude"}, want: true},
		{name: "multi word comm", pid: "1", names: []string{"tmux: client"}, want: true},
		{name: "exact match only", pid: "1", names: []string{"nodejs"}, want: true},
		{name: "no substring match", pid: "1", names: []string{"claude-helper"}, want: true},
		{name: "unrelated ambient ignored", pid: "42", names: []string{"node", "opencode", "bun"}, want: false},
		{name: "empty names", pid: "1", names: []string{"", "  "}, want: false},
		{name: "depth limit preserves current semantics", pid: "8", names: []string{"opencode"}, depth: 10, want: false},
		{name: "invalid root", pid: "nope", names: []string{"node"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hasDescendantWithNamesFromPS(tt.pid, tt.names, tt.depth, snapshot)
			if got != tt.want {
				t.Fatalf("hasDescendantWithNamesFromPS(%q, %v, %d) = %v, want %v", tt.pid, tt.names, tt.depth, got, tt.want)
			}
		})
	}

	substringSnapshot := []byte(`
1 0 bash
2 1 claude-helper
3 1 nodejs
`)
	if hasDescendantWithNamesFromPS("1", []string{"claude", "node"}, 0, substringSnapshot) {
		t.Fatal("hasDescendantWithNamesFromPS matched a process name substring")
	}
}

func TestHasDescendantWithNamesFromPSDoesNotMatchRoot(t *testing.T) {
	t.Parallel()

	snapshot := []byte(`1 0 node`)
	if hasDescendantWithNamesFromPS("1", []string{"node"}, 0, snapshot) {
		t.Fatal("hasDescendantWithNamesFromPS matched the root as its own descendant")
	}
}

func TestHasDescendantWithNamesPosixCheckedReportsSnapshotError(t *testing.T) {
	t.Parallel()
	broken := newScripted(func(tmuxCall) reply {
		return reply{err: errors.New("exec: \"ps\": executable file not found in $PATH")}
	})
	found, err := hasDescendantWithNamesPosixChecked(broken.exec, "1", []string{"node"}, 0)
	if err == nil {
		t.Fatal("hasDescendantWithNamesPosixChecked with missing ps error = nil, want error")
	}
	if found {
		t.Fatal("hasDescendantWithNamesPosixChecked with missing ps found match, want false")
	}
	if _, err := hasDescendantWithNamesPosixChecked(broken.exec, "nope", []string{"node"}, 0); err == nil {
		t.Fatal("invalid pid: error = nil")
	}
}

func TestProcessMatchesNames(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	pid := f.spawn("1", "/Users/x/.local/bin/claude")
	ex := psExec(f)
	if !processMatchesNames(ex, pid, []string{"node", "claude"}) {
		t.Error("processMatchesNames missed the binary basename")
	}
	if processMatchesNames(ex, pid, nil) || processMatchesNames(ex, pid, []string{"node"}) {
		t.Error("processMatchesNames matched the wrong names")
	}
	// A missing pid is ps's no-match exit (status 1): false, not an error.
	if ok, err := processMatchesNamesChecked(ex, "999999", []string{"claude"}); ok || err != nil {
		t.Errorf("missing pid = %v, %v; want false, nil", ok, err)
	}
}

func TestGetProcessGroupID(t *testing.T) {
	t.Parallel()
	f, pane, node, _, _, _, _ := processTree()
	ex := psExec(f)
	if got := getProcessGroupID(ex, node); got != pane {
		t.Errorf("getProcessGroupID(child) = %q, want the pane's group %q", got, pane)
	}
	if got := getProcessGroupID(ex, "999999999"); got != "" {
		t.Errorf("expected empty PGID for nonexistent process, got %q", got)
	}
}

func TestGetProcessGroupMembers(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, reparented, groupmate, stranger := processTree()
	members := getProcessGroupMembers(psExec(f), pane)
	got := map[string]bool{}
	for _, m := range members {
		got[m] = true
	}
	for _, want := range []string{pane, node, claude, reparented, groupmate} {
		if !got[want] {
			t.Errorf("group %s members %v missing %s", pane, members, want)
		}
	}
	if got[stranger] {
		t.Errorf("group members %v include a process from another group", members)
	}
}

func TestGetParentPID(t *testing.T) {
	t.Parallel()
	f, pane, node, _, _, _, _ := processTree()
	ex := psExec(f)
	if got := getParentPID(ex, node); got != pane {
		t.Errorf("getParentPID(node) = %q, want %q", got, pane)
	}
	if got := getParentPID(ex, "999999999"); got != "" {
		t.Errorf("expected empty PPID for nonexistent process, got %q", got)
	}
}

// TestCollectReparentedGroupMembers: only group members orphaned to init and
// not already known are collected; a groupmate with a live unrelated parent
// is left alone.
func TestCollectReparentedGroupMembers(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, reparented, _, _ := processTree()
	known := map[string]bool{pane: true, node: true, claude: true}
	got := collectReparentedGroupMembers(psExec(f), pane, known)
	if !reflect.DeepEqual(got, []string{reparented}) {
		t.Errorf("collectReparentedGroupMembers = %v, want [%s]", got, reparented)
	}
}

// killWith runs kill on a fake clock, driving it through the grace periods.
func killWith(t *testing.T, f *fakeServer, kill func(tm *Tmux) error) (*scripted, error) {
	t.Helper()
	clk := newFixedClock()
	tm, s := f.tmux(clk)
	return s, driven(t, clk, processKillGracePeriod, func() error { return kill(tm) })
}

// signaled returns the pids sent sig, in order.
func signaled(f *fakeServer, sig string) []string {
	var out []string
	for _, k := range f.kills() {
		if s, pid, _ := strings.Cut(k, " "); s == sig {
			out = append(out, pid)
		}
	}
	return out
}

// TestKillSessionWithProcesses checks the whole sequence: respawn disarmed
// first, the tree TERMed deepest-first plus the reparented group member, then
// KILLed, then the pane process, then the session.
func TestKillSessionWithProcesses(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, reparented, groupmate, stranger := processTree()
	s, err := killWith(t, f, func(tm *Tmux) error { return tm.KillSessionWithProcesses("gt-x") })
	if err != nil {
		t.Fatalf("KillSessionWithProcesses: %v", err)
	}
	if f.has("gt-x") {
		t.Error("session survived KillSessionWithProcesses")
	}
	if want := []string{claude, node, reparented, pane}; !reflect.DeepEqual(signaled(f, "TERM"), want) {
		t.Errorf("TERM order = %v, want %v", signaled(f, "TERM"), want)
	}
	if want := []string{claude, node, reparented, pane}; !reflect.DeepEqual(signaled(f, "KILL"), want) {
		t.Errorf("KILL order = %v, want %v", signaled(f, "KILL"), want)
	}
	for _, k := range f.kills() {
		if strings.HasSuffix(k, " "+groupmate) || strings.HasSuffix(k, " "+stranger) {
			t.Errorf("signaled an unrelated process: %s", k)
		}
	}
	subs := s.subs()
	if len(subs) < 2 || subs[0] != "set-option" || subs[1] != "set-hook" {
		t.Errorf("first calls = %v, want remain-on-exit off then the pane-died hook unset", subs)
	}
}

func TestKillSessionWithProcesses_NonexistentSession(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	if _, err := killWith(t, f, func(tm *Tmux) error { return tm.KillSessionWithProcesses("gt-missing") }); err != nil {
		t.Fatalf("KillSessionWithProcesses(missing) = %v, want nil", err)
	}
	if len(f.kills()) != 0 {
		t.Errorf("signaled %v for a missing session", f.kills())
	}
}

func TestKillSessionWithProcessesExcluding(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, reparented, _, _ := processTree()
	if _, err := killWith(t, f, func(tm *Tmux) error { return tm.KillSessionWithProcessesExcluding("gt-x", nil) }); err != nil {
		t.Fatalf("KillSessionWithProcessesExcluding: %v", err)
	}
	if f.has("gt-x") {
		t.Error("session survived")
	}
	got := map[string]bool{}
	for _, p := range signaled(f, "KILL") {
		got[p] = true
	}
	for _, want := range []string{pane, node, claude, reparented} {
		if !got[want] {
			t.Errorf("KILL set %v missing %s", signaled(f, "KILL"), want)
		}
	}
}

// TestKillSessionWithProcessesExcluding_WithExcludePID: the caller (gt done
// running inside the session) is never signaled, but the session still goes.
func TestKillSessionWithProcessesExcluding_WithExcludePID(t *testing.T) {
	t.Parallel()
	f, pane, _, claude, _, _, _ := processTree()
	if _, err := killWith(t, f, func(tm *Tmux) error {
		return tm.KillSessionWithProcessesExcluding("gt-x", []string{pane, claude})
	}); err != nil {
		t.Fatalf("KillSessionWithProcessesExcluding: %v", err)
	}
	for _, k := range f.kills() {
		if strings.HasSuffix(k, " "+pane) || strings.HasSuffix(k, " "+claude) {
			t.Errorf("signaled an excluded pid: %s", k)
		}
	}
	if f.has("gt-x") {
		t.Error("expected session to not exist after KillSessionWithProcessesExcluding")
	}
}

func TestKillSessionWithProcessesExcluding_NonexistentSession(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	if _, err := killWith(t, f, func(tm *Tmux) error {
		return tm.KillSessionWithProcessesExcluding("gt-missing", []string{"12345"})
	}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestKillPaneProcesses(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, reparented, _, _ := processTree()
	if _, err := killWith(t, f, func(tm *Tmux) error { return tm.KillPaneProcesses("gt-x") }); err != nil {
		t.Fatalf("KillPaneProcesses: %v", err)
	}
	if want := []string{claude, node, reparented, pane}; !reflect.DeepEqual(signaled(f, "TERM"), want) {
		t.Errorf("TERM order = %v, want %v", signaled(f, "TERM"), want)
	}
	if !f.has("gt-x") {
		t.Error("KillPaneProcesses must leave the session for the respawn")
	}
}

func TestKillPaneProcessesExcluding(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, _, _, _ := processTree()
	paneID := f.session("gt-x").panes[0].id
	if _, err := killWith(t, f, func(tm *Tmux) error { return tm.KillPaneProcessesExcluding(paneID, nil) }); err != nil {
		t.Fatalf("KillPaneProcessesExcluding: %v", err)
	}
	if want := []string{claude, node, pane}; !reflect.DeepEqual(signaled(f, "TERM"), want) {
		t.Errorf("TERM order = %v, want %v", signaled(f, "TERM"), want)
	}
}

// TestKillPaneProcessesExcluding_WithExcludePID is the self-handoff case: the
// excluded pane process survives to call RespawnPane.
func TestKillPaneProcessesExcluding_WithExcludePID(t *testing.T) {
	t.Parallel()
	f, pane, node, claude, _, _, _ := processTree()
	paneID := f.session("gt-x").panes[0].id
	if _, err := killWith(t, f, func(tm *Tmux) error { return tm.KillPaneProcessesExcluding(paneID, []string{pane}) }); err != nil {
		t.Fatalf("KillPaneProcessesExcluding: %v", err)
	}
	if want := []string{claude, node}; !reflect.DeepEqual(signaled(f, "TERM"), want) {
		t.Errorf("TERM = %v, want %v with the pane pid excluded", signaled(f, "TERM"), want)
	}
}

func TestKillPaneProcessesExcluding_NonexistentPane(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	if _, err := killWith(t, f, func(tm *Tmux) error { return tm.KillPaneProcessesExcluding("%99999", []string{"12345"}) }); err == nil {
		t.Error("expected error for nonexistent pane")
	}
}

func TestSessionSet(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "")
	tm, _ := f.tmux(nil)
	set, err := tm.GetSessionSet()
	if err != nil {
		t.Fatalf("GetSessionSet: %v", err)
	}
	if !set.Has("gt-x") {
		t.Error("SessionSet.Has(gt-x) = false, want true")
	}
	if set.Has("nonexistent-session-xyz-12345") {
		t.Error("SessionSet.Has(nonexistent) = true, want false")
	}
	var nilSet *SessionSet
	if nilSet.Has("anything") {
		t.Error("nil SessionSet.Has() = true, want false")
	}
	if names := set.Names(); !reflect.DeepEqual(names, []string{"gt-x"}) {
		t.Errorf("SessionSet.Names() = %v", names)
	}
	f.with(func() { f.noServer = true })
	if set, err := tm.GetSessionSet(); err != nil || set.Has("gt-x") {
		t.Errorf("no server: set = %v, err = %v; want empty, nil", set, err)
	}
}

func TestCleanupOrphanedSessions(t *testing.T) {
	t.Parallel()
	isGT := func(s string) bool { return strings.HasPrefix(s, "gt-") || strings.HasPrefix(s, "hq-") }
	f := newFakeServer()
	f.addSession("gt-test-cleanup-rig", "sleep") // zombie: no agent
	f.addSession("hq-test-cleanup", "sleep")     // zombie: no agent
	f.addSession("other-test-session", "sleep")  // not ours
	live := f.addSession("gt-live", "bash")      // agent running under a shell
	f.spawn(live.panes[0].pid, "claude")

	clk := newFixedClock()
	tm, _ := f.tmux(clk)
	var cleaned int
	err := driven(t, clk, processKillGracePeriod, func() error {
		var err error
		cleaned, err = tm.CleanupOrphanedSessions(isGT)
		return err
	})
	if err != nil {
		t.Fatalf("CleanupOrphanedSessions: %v", err)
	}
	if cleaned != 2 {
		t.Errorf("cleaned %d sessions, want 2", cleaned)
	}
	for name, want := range map[string]bool{
		"gt-test-cleanup-rig": false, "hq-test-cleanup": false,
		"other-test-session": true, "gt-live": true,
	} {
		if f.has(name) != want {
			t.Errorf("session %s exists = %v, want %v", name, f.has(name), want)
		}
	}
}

func TestCleanupOrphanedSessions_NoSessions(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.with(func() { f.noServer = true })
	tm, _ := f.tmux(nil)
	cleaned, err := tm.CleanupOrphanedSessions(func(string) bool { return true })
	if err != nil || cleaned != 0 {
		t.Fatalf("CleanupOrphanedSessions = %d, %v; want 0, nil", cleaned, err)
	}
}
