package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// fakeGitState is a worktree as gitStateOf reads it.
type fakeGitState struct {
	work        git.UncommittedWorkStatus
	branch      string
	preserved   git.BranchPreservationStatus
	preserveErr error
	stashes     int
	targets     []string
}

func (f *fakeGitState) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	w := f.work
	return &w, nil
}
func (f *fakeGitState) CurrentBranch() (string, error) { return f.branch, nil }
func (f *fakeGitState) BranchPreservationStatus(branch, remote string, targets []string) (git.BranchPreservationStatus, error) {
	f.targets = targets
	return f.preserved, f.preserveErr
}
func (f *fakeGitState) StashCountAll() (int, error) { return f.stashes, nil }

// TestGitStateVerdicts pins what makes a worktree dirty for the nuke and
// recovery gates: real uncommitted files (runtime artifacts do not count),
// the branch's own stashes (other branches' stashes are only reported),
// unpreserved commits, and a preservation check that could not run, which
// fails closed (gt-14a).
func TestGitStateVerdicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fake  fakeGitState
		clean bool
		check func(t *testing.T, s *GitState)
	}{
		{name: "clean", fake: fakeGitState{branch: "polecat/a"}, clean: true},
		{
			name:  "runtime artifacts only",
			fake:  fakeGitState{work: git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".opencode/plugins/gastown.js", ".beads/issues.jsonl"}}},
			clean: true,
			check: func(t *testing.T, s *GitState) {
				if len(s.UncommittedFiles) != 0 {
					t.Errorf("UncommittedFiles = %v, want none", s.UncommittedFiles)
				}
			},
		},
		{
			name: "real file beside runtime artifacts",
			fake: fakeGitState{work: git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".opencode/plugins/gastown.js", "real.go"}}},
			check: func(t *testing.T, s *GitState) {
				if strings.Join(s.UncommittedFiles, ",") != "real.go" {
					t.Errorf("UncommittedFiles = %v, want [real.go]", s.UncommittedFiles)
				}
			},
		},
		{
			name: "staged-only content stays dirty (no index-skew relaxation here)",
			fake: fakeGitState{work: git.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{"README.md"}, StagedOnly: []string{"README.md"}}},
			check: func(t *testing.T, s *GitState) {
				if strings.Join(s.UncommittedFiles, ",") != "README.md" {
					t.Errorf("UncommittedFiles = %v, want [README.md]", s.UncommittedFiles)
				}
			},
		},
		{
			name: "own stash dirty, shared stashes reported",
			fake: fakeGitState{work: git.UncommittedWorkStatus{StashCount: 1}, stashes: 3},
			check: func(t *testing.T, s *GitState) {
				if s.StashCount != 1 || s.SharedStashCount != 2 {
					t.Errorf("stashes own %d shared %d, want 1 and 2", s.StashCount, s.SharedStashCount)
				}
			},
		},
		{
			name:  "only other branches' stashes",
			fake:  fakeGitState{stashes: 2},
			clean: true,
			check: func(t *testing.T, s *GitState) {
				if s.StashCount != 0 || s.SharedStashCount != 2 {
					t.Errorf("stashes own %d shared %d, want 0 and 2", s.StashCount, s.SharedStashCount)
				}
			},
		},
		{
			name: "unpreserved commits",
			fake: fakeGitState{preserved: git.BranchPreservationStatus{ComparisonBase: "upstream/main", UnpreservedPatchCount: 2}},
			check: func(t *testing.T, s *GitState) {
				if s.UnpushedCommits != 2 || s.ComparisonBase != "upstream/main" {
					t.Errorf("unpushed %d base %q, want 2 against upstream/main", s.UnpushedCommits, s.ComparisonBase)
				}
			},
		},
		{
			name: "preservation check fails closed",
			fake: fakeGitState{preserveErr: errors.New("no comparison ref")},
			check: func(t *testing.T, s *GitState) {
				if !s.PreservationCheckFailed || !strings.Contains(s.PreservationCheckFailure, "no comparison ref") {
					t.Errorf("state %+v does not record the failed check", s)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := tc.fake
			s, err := gitStateOf(&fake, []string{"release"})
			if err != nil {
				t.Fatalf("gitStateOf: %v", err)
			}
			if s.Clean != tc.clean {
				t.Errorf("Clean = %v, want %v (%+v)", s.Clean, tc.clean, s)
			}
			if strings.Join(fake.targets, ",") != "release" {
				t.Errorf("preservation judged against %v, want the given targets", fake.targets)
			}
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// gitStateSeatFixtures is a polecat's agent bead and the reads the command
// builds from it, before a test replaces the sandbox probe and the git reader.
type gitStateSeatFixtures struct {
	reads       polecatGitStateReads
	agentBeadID string
}

// newGitStateSeatFixtures stores an agent bead recording agentName (none when
// empty) and returns the production reads over it, so a test drives the real
// agent-bead read and only the gateway and git seams are faked.
func newGitStateSeatFixtures(t *testing.T, agentName string) gitStateSeatFixtures {
	t.Helper()
	bd := beadsfake.New()
	beadID := "gt-gastown-polecat-nux"
	description := ""
	if agentName != "" {
		description = "agent: " + agentName + "\n"
	}
	bd.Seed(beads.Issue{ID: beadID, Type: "agent", Description: description})
	return gitStateSeatFixtures{reads: polecatGitStateReadsFor(bd, t.TempDir(), beadID), agentBeadID: beadID}
}

// TestPolecatGitStateSandboxSeatUnknown is gt-ue5ady: a sandbox seat edits and
// commits inside its sandbox, so its pod-side worktree is a stale leftover
// whose clean reading says nothing. The command must answer that the state
// lives in the sandbox, name it, and never read the pod-side worktree.
func TestPolecatGitStateSandboxSeatUnknown(t *testing.T) {
	t.Parallel()
	const (
		agentName   = "codex"
		sessionName = "gt-nux"
	)
	for _, tc := range []struct {
		name   string
		exists bool
		want   string
	}{
		{
			name:   "running sandbox",
			exists: true,
			want:   "unknown: git state lives in the running sandbox " + sessionName + "; the pod-side worktree is not the seat's work",
		},
		{
			name:   "gone sandbox names no stale reading",
			exists: false,
			want:   "unknown: git state of sandbox " + sessionName + " is unknown; the pod-side worktree is not the seat's work",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newGitStateSeatFixtures(t, agentName)
			gitCalled := false
			f.reads.gitState = func(string) (*GitState, error) {
				gitCalled = true
				return &GitState{Clean: true, UncommittedFiles: []string{}}, nil
			}
			f.reads.probe = func(agent, session string) (polecat.LiveGitState, bool) {
				if agent != agentName || session != sessionName {
					t.Errorf("probe asked about agent %q session %q, want %q and %q", agent, session, agentName, sessionName)
				}
				return polecat.SandboxSeatGitState("", tc.exists, nil, session), true
			}

			var out bytes.Buffer
			if err := reportPolecatGitState(&out, "gastown", "nux", "/stale/host/clone", sessionName, f.reads, false); err != nil {
				t.Fatalf("reportPolecatGitState: %v", err)
			}
			if gitCalled {
				t.Error("the pod-side git reader ran for a sandbox seat")
			}
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("output does not carry %q:\n%s", tc.want, got)
			}
			if strings.Contains(strings.ToLower(got), "clean") {
				t.Errorf("output claims a clean verdict for a sandbox seat:\n%s", got)
			}
		})
	}
}

// TestPolecatGitStateSandboxSeatJSON pins the --json answer for a sandbox seat:
// a source of unknown and no clean field, because clean is not knowable from
// the pod-side worktree.
func TestPolecatGitStateSandboxSeatJSON(t *testing.T) {
	t.Parallel()
	const (
		agentName   = "codex"
		sessionName = "gt-nux"
	)
	f := newGitStateSeatFixtures(t, agentName)
	f.reads.gitState = func(string) (*GitState, error) {
		t.Error("the pod-side git reader ran for a sandbox seat")
		return nil, errors.New("unreachable")
	}
	f.reads.probe = func(_, session string) (polecat.LiveGitState, bool) {
		return polecat.SandboxSeatGitState("", true, nil, session), true
	}

	var out bytes.Buffer
	if err := reportPolecatGitState(&out, "gastown", "nux", "/stale/host/clone", sessionName, f.reads, true); err != nil {
		t.Fatalf("reportPolecatGitState: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if got["source"] != polecat.GitStateSourceUnknown {
		t.Errorf("source = %v, want %q", got["source"], polecat.GitStateSourceUnknown)
	}
	if _, ok := got["clean"]; ok {
		t.Errorf("sandbox-seat JSON carries a clean field:\n%s", out.String())
	}
	reason, _ := got["reason"].(string)
	if !strings.Contains(reason, sessionName) {
		t.Errorf("reason %q does not name the sandbox %s", reason, sessionName)
	}
}

// TestPolecatGitStateHostSeatUnchanged pins a host seat's output byte for byte
// against what the command printed before the sandbox probe ran, so the probe
// never changes a host seat's answer.
func TestPolecatGitStateHostSeatUnchanged(t *testing.T) {
	t.Parallel()
	const (
		agentName   = "codex"
		sessionName = "gt-toast"
		clonePath   = "/town/gastown/polecats/toast/gastown"
	)
	f := newGitStateSeatFixtures(t, agentName)
	f.reads.probe = func(agent, session string) (polecat.LiveGitState, bool) {
		if agent != agentName || session != sessionName {
			t.Errorf("probe asked about agent %q session %q, want %q and %q", agent, session, agentName, sessionName)
		}
		return polecat.LiveGitState{}, false
	}
	f.reads.gitState = func(path string) (*GitState, error) {
		if path != clonePath {
			t.Errorf("git reader asked about %q, want %q", path, clonePath)
		}
		return gitStateOf(&fakeGitState{branch: "polecat/toast"}, nil)
	}

	var text bytes.Buffer
	if err := reportPolecatGitState(&text, "gastown", "toast", clonePath, sessionName, f.reads, false); err != nil {
		t.Fatalf("reportPolecatGitState: %v", err)
	}
	wantText := "Git State: gastown/toast\n\n" +
		"  Working Tree:  clean\n" +
		"  Unpushed:      0 commits\n" +
		"  Branch Stashes: 0\n" +
		"\n" +
		"  Verdict:       CLEAN (safe to kill)\n"
	if text.String() != wantText {
		t.Errorf("host-seat text changed:\ngot:\n%q\nwant:\n%q", text.String(), wantText)
	}

	var encoded bytes.Buffer
	if err := reportPolecatGitState(&encoded, "gastown", "toast", clonePath, sessionName, f.reads, true); err != nil {
		t.Fatalf("reportPolecatGitState --json: %v", err)
	}
	wantJSON := "{\n" +
		"  \"clean\": true,\n" +
		"  \"uncommitted_files\": [],\n" +
		"  \"unpushed_commits\": 0,\n" +
		"  \"unpreserved_patch_count\": 0,\n" +
		"  \"stash_count\": 0\n" +
		"}\n"
	if encoded.String() != wantJSON {
		t.Errorf("host-seat JSON changed:\ngot:\n%q\nwant:\n%q", encoded.String(), wantJSON)
	}
}

// TestPolecatGitStateUnreadableAgentBeadIsUnknown refuses a clean verdict when
// the agent bead cannot be read: without the recorded agent the seat could be a
// sandbox seat, so the pod-side worktree is not evidence either way.
func TestPolecatGitStateUnreadableAgentBeadIsUnknown(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	bd.FailWith("show", errors.New("dolt unreachable"))
	f := gitStateSeatFixtures{reads: polecatGitStateReadsFor(bd, t.TempDir(), "gt-gastown-polecat-nux")}
	f.reads.gitState = func(string) (*GitState, error) {
		t.Error("the pod-side git reader ran with no readable agent bead")
		return nil, errors.New("unreachable")
	}
	f.reads.probe = func(string, string) (polecat.LiveGitState, bool) {
		t.Error("the sandbox probe ran with no recorded agent name")
		return polecat.LiveGitState{}, false
	}

	var out bytes.Buffer
	if err := reportPolecatGitState(&out, "gastown", "nux", "/stale/host/clone", "gt-nux", f.reads, false); err != nil {
		t.Fatalf("reportPolecatGitState: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "unknown: could not read the polecat's agent bead: dolt unreachable") {
		t.Errorf("output does not report the unread bead as unknown:\n%s", got)
	}
	if strings.Contains(strings.ToLower(got), "clean") {
		t.Errorf("output claims a clean verdict with no readable agent bead:\n%s", got)
	}
}

// TestPolecatGitStateWiredProbeReadsSandboxSeat drives the probe that
// polecatGitStateReadsFor builds, not a stub: a sandbox seat whose registry
// record says it runs reads unknown, and the pod-side git reader never runs.
// Only the git reader is replaced. The record answers before the sandbox CLI
// is asked, so no gateway is reached.
func TestPolecatGitStateWiredProbeReadsSandboxSeat(t *testing.T) {
	t.Parallel()
	const sessionName = "gt-nux"
	town := sandboxSeatTown(t)
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sessionName, Rig: "gastown", Polecat: "nux"}); err != nil {
		t.Fatalf("WriteSandboxRecord: %v", err)
	}
	bd := beadsfake.New()
	const beadID = "gt-gastown-polecat-nux"
	bd.Seed(beads.Issue{ID: beadID, Type: "agent", Description: "agent: " + sandboxSeatAgent + "\n"})
	reads := polecatGitStateReadsFor(bd, town, beadID)
	gitCalled := false
	reads.gitState = func(string) (*GitState, error) {
		gitCalled = true
		return nil, errors.New("pod-side git reader ran for a sandbox seat")
	}

	var out bytes.Buffer
	if err := reportPolecatGitState(&out, "gastown", "nux", "/stale/host/clone", sessionName, reads, false); err != nil {
		t.Fatalf("reportPolecatGitState: %v", err)
	}
	if gitCalled {
		t.Error("the pod-side git reader ran for a sandbox seat")
	}
	want := "unknown: git state lives in the running sandbox " + sessionName + "; the pod-side worktree is not the seat's work"
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("output does not carry %q:\n%s", want, got)
	}
}
