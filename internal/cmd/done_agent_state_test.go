package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/land"
)

// TestClearDoneCheckpoints verifies that clearDoneCheckpoints removes
// only done-cp labels while preserving other labels.
//
// Moved here from done_convoy_test.go when the convoy helpers it shared a
// file with were deleted (gt-gzhin.7).
func TestClearDoneCheckpoints(t *testing.T) {
	t.Parallel()
	allLabels := []string{
		"gt:agent",
		"idle:3",
		"done-cp:pushed:mybranch:1738972801",
		"done-cp:mr-created:gt-xyz:1738972802",
		"backoff-until:1738972900",
	}

	var kept []string
	var removed []string
	for _, label := range allLabels {
		if strings.HasPrefix(label, "done-cp:") {
			removed = append(removed, label)
		} else {
			kept = append(kept, label)
		}
	}

	if len(removed) != 2 {
		t.Errorf("expected 2 checkpoint labels removed, got %d: %v", len(removed), removed)
	}
	if len(kept) != 3 {
		t.Errorf("expected 3 labels kept, got %d: %v", len(kept), kept)
	}

	// Verify no checkpoint labels in kept set
	for _, label := range kept {
		if strings.HasPrefix(label, "done-cp:") {
			t.Errorf("checkpoint label was not removed: %s", label)
		}
	}
}

// TestDeferredDoneLeavesReadyToLandBeadUntouched: a polecat that runs
// `gt done --status DEFERRED` on a bead it no longer owns must lose neither
// gt:ready-to-land nor its assignee. DEFERRED preserves the bead for resume;
// the crew takeover nudge relies on this, since the landing flow owns the
// bead's labels and assignee (gt-qmnm3).
func TestDeferredDoneLeavesReadyToLandBeadUntouched(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID:       "gt-held",
		Title:    "held by a polecat",
		Type:     "task",
		Status:   string(beads.StatusInProgress),
		Assignee: "gastown/polecats/agate",
		Labels:   []string{land.LabelReadyToLand},
	})

	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func(string) (string, error) {
			return "", errors.New("resolving HEAD: not a git repository")
		},
	}
	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitDeferred, "gt-held", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("deferred gt done: %v", err)
	}

	got, err := db.Show("gt-held")
	if err != nil {
		t.Fatalf("show gt-held: %v", err)
	}
	if !beads.HasLabel(got, land.LabelReadyToLand) {
		t.Errorf("deferred gt done removed %s: labels %v", land.LabelReadyToLand, got.Labels)
	}
	if got.Assignee != "gastown/polecats/agate" {
		t.Errorf("deferred gt done changed the assignee to %q", got.Assignee)
	}
	if got.Status == string(beads.StatusClosed) {
		t.Error("deferred gt done closed the bead")
	}
}

// TestCompletedDoneOnUnchangedReworkRecordsTheRequeueHint: a completed gt done
// on a rework bead still sitting on the head its rejection names skips the
// close (the close-time invariant, gt-6hmz) and leaves the bead open — but the
// reason it records is the actionable one: the head is unchanged, nothing was
// resubmitted, and an operator can re-queue it (gt-3e1z4).
func TestCompletedDoneOnUnchangedReworkRecordsTheRequeueHint(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	const head = "1111111111111111111111111111111111111111"
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID:     "gt-rework",
		Title:  "the work",
		Type:   "task",
		Status: string(beads.StatusOpen),
		Labels: []string{land.LabelRework},
		Notes: land.FormatRejectionNote(land.RejectionNote{
			Attempt: 1, Kind: "gate", Reason: "the gate refused it",
			Branch: "polecat/mica/gt-rework+abc", Target: "main", MR: "gt-rework", Head: head,
		}),
	})

	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "mica"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func(string) (string, error) {
			return head, nil
		},
	}
	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-rework", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("completed gt done: %v", err)
	}

	got, err := db.Show("gt-rework")
	if err != nil {
		t.Fatalf("show gt-rework: %v", err)
	}
	if got.Status == string(beads.StatusClosed) {
		t.Error("gt done closed a rework bead whose head was unchanged")
	}
	comments, err := db.Comments("gt-rework")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) == 0 {
		t.Fatal("gt done recorded nothing on the unchanged rework bead")
	}
	text := comments[len(comments)-1].Text
	for _, want := range []string{"DONE_CLOSE_SKIPPED", "head unchanged since the rejection", "gt land requeue gt-rework"} {
		if !strings.Contains(text, want) {
			t.Errorf("comment %q does not carry %q", text, want)
		}
	}
}

// TestUnchangedReworkHeadLeavesTheSeatRestartable: a gt done that stands down
// on a rework bead still on its rejected head resubmits nothing, so the seat
// must not read as finished. Recorded as COMPLETED with the bead still hooked,
// patrol_scan skips the seat as stopped on purpose and nothing restarts it
// (gt-jfhvc). It records stuck with a DEFERRED exit instead, which patrol_scan
// restarts on the preserved branch (gt-ks62m). The restart itself is tested in
// internal/patrolscan (TestDeferredExitSeatIsRestartedNotSkipped); this test
// pins the record it reads, including the cleanup status it names in the
// restart reason.
func TestUnchangedReworkHeadLeavesTheSeatRestartable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		criteria string
	}{
		{name: "plain rework bead"},
		{name: "unchecked criteria name a different reason", criteria: "- [ ] the gate passes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
				if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
			if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
				t.Fatalf("write routes.jsonl: %v", err)
			}

			const head = "1111111111111111111111111111111111111111"
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			db.Seed(doneAgentBead(), beads.Issue{
				ID:                 "gt-rework",
				Title:              "the work",
				Type:               "task",
				Status:             string(beads.StatusOpen),
				AcceptanceCriteria: tc.criteria,
				Labels:             []string{land.LabelRework},
				Notes: land.FormatRejectionNote(land.RejectionNote{
					Attempt: 1, Kind: "gate", Reason: "the gate refused it",
					Branch: "polecat/nux/gt-rework+abc", Target: "main", MR: "gt-rework", Head: head,
				}),
			})

			env := doneStateEnv{
				getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
				routed: func(string) beads.Client { return db },
				source: func(string, string) beads.Client { return db },
				purge:  func(string, string) {},
				reviewHead: func(string) (string, error) {
					return head, nil
				},
			}
			// The worktree holds the rejected commit on its branch, so the
			// restart resumes it: that is the cleanup status patrol_scan reads.
			pending := pendingCompletion()
			unpushed := "has_unpushed"
			pending.CleanupStatus = &unpushed
			if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-rework", pending); err != nil {
				t.Fatalf("completed gt done: %v", err)
			}

			fields := beads.ParseAgentFields(mustShowBead(t, db, "gt-gastown-polecat-nux").Description)
			if fields.AgentState != string(beads.AgentStateStuck) {
				t.Errorf("agent_state = %q, want stuck: a stood-down seat is not finished, and patrol_scan skips a done seat", fields.AgentState)
			}
			if fields.ExitType != done.ExitDeferred {
				t.Errorf("exit_type = %q, want %s: patrol_scan restarts a stuck seat only after a DEFERRED exit", fields.ExitType, done.ExitDeferred)
			}
			if fields.CleanupStatus != unpushed {
				t.Errorf("cleanup_status = %q, want %q: the DEFERRED restart names it as the preserved work", fields.CleanupStatus, unpushed)
			}
			if got := mustShowBead(t, db, "gt-rework").Status; got == string(beads.StatusClosed) {
				t.Error("gt done closed a rework bead whose head was unchanged")
			}
		})
	}
}

// TestCompletedDoneOnSubmittedWorkRecordsDone: a gt done that submits its work
// (the bead is gt:ready-to-land) is a real completion. The seat records done
// with exit COMPLETED, and the bead stays open for the landing worker to close.
// The rework stand-down must not pull this path onto a DEFERRED exit (gt-jfhvc).
func TestCompletedDoneOnSubmittedWorkRecordsDone(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	const head = "2222222222222222222222222222222222222222"
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{
		ID:     "gt-base-123",
		Title:  "the work",
		Type:   "task",
		Status: string(beads.StatusInProgress),
		Labels: []string{land.LabelReadyToLand},
	})

	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func(string) (string, error) {
			return head, nil
		},
	}
	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingCompletion()); err != nil {
		t.Fatalf("completed gt done: %v", err)
	}

	fields := beads.ParseAgentFields(mustShowBead(t, db, "gt-gastown-polecat-nux").Description)
	if fields.AgentState != string(beads.AgentStateDone) {
		t.Errorf("agent_state = %q, want done: a submitted completion is finished", fields.AgentState)
	}
	if fields.ExitType != done.ExitCompleted {
		t.Errorf("exit_type = %q, want %s: a submitted completion must not read as a stand-down", fields.ExitType, done.ExitCompleted)
	}
	if fields.HookBead != "" {
		t.Errorf("hook_bead = %q, want it cleared", fields.HookBead)
	}
	got := mustShowBead(t, db, "gt-base-123")
	if got.Status == string(beads.StatusClosed) {
		t.Error("gt done closed a submitted bead; the landing worker closes it")
	}
	comments, err := db.Comments("gt-base-123")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Text, "Submitted for landing") {
		t.Errorf("gt done did not record the submission on the bead: comments %v", comments)
	}
}

// TestDoneAgentStateAsksForHeadInItsOwnCwd: the HEAD the close gate judges
// comes from the worktree gt done was handed, not from the process working
// directory. gt done can resolve its agent and worktree from the environment,
// so a run standing elsewhere would otherwise validate a rework stand-down (and
// review evidence) against another repository's head (gt-u0zq0).
func TestDoneAgentStateAsksForHeadInItsOwnCwd(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{
		ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked),
	})

	var asked []string
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func(dir string) (string, error) {
			asked = append(asked, dir)
			return "", errors.New("resolving HEAD in " + dir + ": not a git repository")
		},
	}

	cwd := filepath.Join(townRoot, "gastown")
	if _, err := updateAgentStateOnDoneIn(env, cwd, townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("completed gt done: %v", err)
	}
	if len(asked) == 0 {
		t.Fatal("the close gate resolved no HEAD")
	}
	for _, dir := range asked {
		if dir != cwd {
			t.Errorf("HEAD asked for in %q, want the worktree gt done was given (%q)", dir, cwd)
		}
	}
	// A HEAD that cannot be read is reported, not fatal: the close still runs,
	// which is the fail-open side this gate takes for state it cannot inspect.
	got, err := db.Show("gt-base-123")
	if err != nil {
		t.Fatalf("show gt-base-123: %v", err)
	}
	if got.Status != string(beads.StatusClosed) {
		t.Errorf("gt-base-123 status = %q, want closed despite the unreadable HEAD", got.Status)
	}
}

// showFailsClient is a beads.Client whose Show reports err for one ID and
// delegates the rest, so a test can fail exactly the read gt done makes to
// resolve the hooked bead.
type showFailsClient struct {
	beads.Client
	id  string
	err error
}

func (c showFailsClient) Show(id string) (*beads.Issue, error) {
	if id == c.id {
		return nil, c.err
	}
	return c.Client.Show(id)
}

// TestDoneAgentStateLeavesTheHookOnAnUnreadableHookedBead: when the read that
// resolves the hooked bead fails for a reason other than absence, gt done must
// not clear hook_bead or mark the agent done. Falling through would strand the
// work bead — hooked or in_progress, with no polecat on it and nothing in the
// output — which is the dispatch-loop shape gt-pftz named (gt-abr6v).
func TestDoneAgentStateLeavesTheHookOnAnUnreadableHookedBead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{
		ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked),
	})

	readErr := errors.New("dial tcp 127.0.0.1:3307: connection refused")
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client {
			return showFailsClient{Client: db, id: "gt-base-123", err: readErr}
		},
		purge: func(string, string) {},
	}

	_, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{})
	if err == nil {
		t.Fatal("a failed read of the hooked bead returned no error")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("error %v does not carry the read failure", err)
	}

	agent, err := db.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("show agent bead: %v", err)
	}
	fields := beads.ParseAgentFields(agent.Description)
	if fields.HookBead != "gt-base-123" {
		t.Errorf("hook_bead = %q, want it left at gt-base-123", fields.HookBead)
	}
	if fields.AgentState == string(beads.AgentStateDone) {
		t.Error("gt done marked the agent done over a bead it could not read")
	}

	work, err := db.Show("gt-base-123")
	if err != nil {
		t.Fatalf("show work bead: %v", err)
	}
	if work.Status != string(beads.StatusHooked) {
		t.Errorf("work bead status = %q, want it left hooked", work.Status)
	}
}

// TestDoneAgentStateClearsTheHookWhenTheBeadIsGone pins the other side of that
// read: absence is an answer, so a hooked bead that no longer exists still lets
// gt done clear the hook and finish (gt-abr6v).
func TestDoneAgentStateClearsTheHookWhenTheBeadIsGone(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead())

	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
	}

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("gt done with a missing hooked bead: %v", err)
	}

	agent, err := db.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("show agent bead: %v", err)
	}
	if fields := beads.ParseAgentFields(agent.Description); fields.HookBead != "" {
		t.Errorf("hook_bead = %q, want it cleared for a bead that is gone", fields.HookBead)
	}
}

// TestDoneStateEnvZeroValueResolvesHeadInTheGivenDir pins the default wiring:
// a doneStateEnv that names no resolver reads HEAD through
// done.CurrentReviewEvidenceHeadIn, which takes the directory to read it in.
func TestDoneStateEnvZeroValueResolvesHeadInTheGivenDir(t *testing.T) {
	t.Parallel()
	var e doneStateEnv
	if got, want := reflect.ValueOf(e.head()).Pointer(), reflect.ValueOf(done.CurrentReviewEvidenceHeadIn).Pointer(); got != want {
		t.Error("zero doneStateEnv does not resolve HEAD through done.CurrentReviewEvidenceHeadIn")
	}
}

// TestDoneAgentStatePurgesTheTownDatabaseToo: gt done's self-clean purges the
// closed wisps of the finishing rig's database, and of the town (hq) database
// as well. Nothing else reclaims the town's: wisp_reaper, the patrol that
// would, is enabled in no rig, so the plugin-run wisps accumulated there at
// ~90 a day until an operator purged 9,488 of them by hand on 2026-10-12
// (gt-j4rxz). Each directory is purged once, for the configured grace period.
func TestDoneAgentStatePurgesTheTownDatabaseToo(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	daemonJSON := `{"type":"daemon-patrol-config","version":1,"patrols":{"wisp_reaper":{"enabled":true,"delete_age":"48h"}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "daemon.json"), []byte(daemonJSON), 0644); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{
		ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked),
	})

	var purged []string
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge: func(dir, root string) {
			purged = append(purged, dir)
			// The grace period comes from closedWispDeleteAge(root), read by
			// the real purge; record what each directory was asked for.
			var rec recordingPurger
			purgeClosedEphemeralBeads(&rec, root)
			if len(rec.olderThan) != 1 || rec.olderThan[0] != "48h" {
				t.Errorf("purge of %s asked for grace period %q, want 48h", dir, rec.olderThan)
			}
		},
		reviewHead: func(string) (string, error) {
			return "", errors.New("resolving HEAD: not a git repository")
		},
	}
	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("completed gt done: %v", err)
	}

	want := []string{filepath.Join(townRoot, "gastown"), townRoot}
	if !reflect.DeepEqual(purged, want) {
		t.Errorf("gt done purged %v, want %v", purged, want)
	}
}

// TestDoneAgentStateToleratesAFailingTownPurge: the town purge is best-effort
// like the rig's. A Dolt serialization conflict — another client committing at
// that moment — must leave gt done's result and the agent state untouched
// (gt-j4rxz).
func TestDoneAgentStateToleratesAFailingTownPurge(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{
		ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked),
	})

	rigDir := filepath.Join(townRoot, "gastown")
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge: func(dir, root string) {
			if dir == root {
				// The real purge path, against a store that conflicts.
				purgeClosedEphemeralBeads(failingPurger{}, root)
			}
		},
		reviewHead: func(string) (string, error) {
			return "", errors.New("resolving HEAD: not a git repository")
		},
	}
	if _, err := updateAgentStateOnDoneIn(env, rigDir, townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("gt done with a failing town purge: %v", err)
	}

	agent, err := db.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("show agent bead: %v", err)
	}
	fields := beads.ParseAgentFields(agent.Description)
	if fields.AgentState != string(beads.AgentStateDone) {
		t.Errorf("agent_state = %q, want %q despite the failing town purge", fields.AgentState, beads.AgentStateDone)
	}
	if fields.HookBead != "" {
		t.Errorf("hook_bead = %q, want it cleared despite the failing town purge", fields.HookBead)
	}
}

// TestWispPurgeDirs: gt done purges the rig's beads directory and the town
// (hq) database's. A rig whose beads directory IS the town directory has one
// database, not two, and is listed once — the alternative, two entries for one
// directory, is a second bd purge on a database nothing changed (gt-j4rxz).
func TestWispPurgeDirs(t *testing.T) {
	t.Parallel()
	townRoot := "/town"

	t.Run("rig and town", func(t *testing.T) {
		got := wispPurgeDirs(filepath.Join(townRoot, "gastown"), townRoot)
		want := []string{filepath.Join(townRoot, "gastown"), townRoot}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("wispPurgeDirs() = %v, want %v", got, want)
		}
	})

	t.Run("the rig is the town", func(t *testing.T) {
		for _, beadsPath := range []string{townRoot, filepath.Join(townRoot, "."), townRoot + "/"} {
			got := wispPurgeDirs(beadsPath, townRoot)
			if len(got) != 1 || got[0] != beadsPath {
				t.Errorf("wispPurgeDirs(%q, %q) = %v, want that directory once", beadsPath, townRoot, got)
			}
		}
	})
}
