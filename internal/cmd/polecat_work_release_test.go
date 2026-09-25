package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// fakeWorkReleaser is an in-memory polecatWorkReleaser: bead -> (status, assignee).
//
// It models bd 1.2.2's write fences: a plain (unguarded) assignee write on an
// in_progress bead someone holds fails (AssigneeNotStolen), and an
// --if-assignee write whose precondition no longer holds writes nothing
// (exit 13 -> released=false, nil). raceTo re-assigns the bead to another
// agent right after the first read, to model a concurrent re-sling.
type fakeWorkReleaser struct {
	beads     map[string][2]string
	readErr   error
	raceTo    string
	released  []string
	resets    []string
	annotated map[string]string
}

func (f *fakeWorkReleaser) HookState(beadID string) (string, string, error) {
	if f.readErr != nil {
		return "", "", f.readErr
	}
	b, ok := f.beads[beadID]
	if !ok {
		return "", "", errors.New("not found")
	}
	if f.raceTo != "" {
		f.beads[beadID] = [2]string{b[0], f.raceTo}
	}
	return b[0], b[1], nil
}

func (f *fakeWorkReleaser) ReleaseBead(beadID, expectedAssignee string) (bool, error) {
	cur := f.beads[beadID]
	if expectedAssignee == "" {
		if cur[0] == "in_progress" && cur[1] != "" {
			return false, errors.New("AssigneeNotStolen: in_progress bead is held by " + cur[1])
		}
	} else if cur[1] != expectedAssignee {
		return false, nil // exit 13: guard no longer held, nothing written
	}
	f.released = append(f.released, beadID)
	f.beads[beadID] = [2]string{"open", ""}
	return true, nil
}

func (f *fakeWorkReleaser) ResetSlot(agentID string) error {
	f.resets = append(f.resets, agentID)
	return nil
}

func (f *fakeWorkReleaser) Annotate(beadID, text string) error {
	if f.annotated == nil {
		f.annotated = map[string]string{}
	}
	f.annotated[beadID] = text
	return nil
}

// --- releasePolecatWork: the shared compare-and-release helper --------------

func TestReleasePolecatWorkComparesAssigneeBeforeRelease(t *testing.T) {
	const me = "gastown/polecats/basalt"
	cases := []struct {
		name         string
		status, who  string
		readErr      error
		raceTo       string
		wantReleased bool
	}{
		{name: "hooked to this polecat", status: "hooked", who: me, wantReleased: true},
		// The common nuke case: the polecat was working. bd fences a plain
		// write here; the guarded write is the sanctioned transfer.
		{name: "in_progress on this polecat", status: "in_progress", who: me, wantReleased: true},
		{name: "re-slung to another polecat", status: "hooked", who: "gastown/polecats/granite"},
		{name: "re-slung between read and write", status: "in_progress", who: me, raceTo: "gastown/polecats/granite"},
		{name: "closed", status: "closed", who: me},
		{name: "already open", status: "open", who: ""},
		{name: "unreadable", readErr: errors.New("dolt down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {tc.status, tc.who}}, readErr: tc.readErr, raceTo: tc.raceTo}
			out := releasePolecatWork(rel, me, "gt-elvf4", false)
			if out.Released != tc.wantReleased || (len(rel.released) == 1) != tc.wantReleased {
				t.Fatalf("released = %v (%v), want %v; note %q", out.Released, rel.released, tc.wantReleased, out.SkipNote)
			}
			if !tc.wantReleased && out.SkipNote == "" {
				t.Fatal("a skipped release must say why")
			}
			if strings.Contains(out.SkipNote, "release failed") {
				t.Fatalf("a lost race must read as a skip, not a failure: %q", out.SkipNote)
			}
			if tc.raceTo != "" && rel.beads["gt-elvf4"][1] != tc.raceTo {
				t.Fatalf("the winner's assignment was overwritten: %v", rel.beads["gt-elvf4"])
			}
			if len(rel.resets) != 0 {
				t.Fatalf("resetSlot=false must not reset the slot, got %v", rel.resets)
			}
		})
	}
}

func TestReleasePolecatWorkResetsSlotOnlyWhenAsked(t *testing.T) {
	rel := &fakeWorkReleaser{beads: map[string][2]string{}}
	out := releasePolecatWork(rel, "gastown/polecats/basalt", "", true)
	if !out.SlotReset || len(rel.resets) != 1 || rel.resets[0] != "gastown/polecats/basalt" {
		t.Fatalf("slot reset = %v %v", out.SlotReset, rel.resets)
	}
	if len(rel.released) != 0 {
		t.Fatalf("no bead named, nothing to release: %v", rel.released)
	}
}

// The production releaser writes with --if-assignee and reads bd's exit 13 as
// "guard no longer held" (a skip), any other failure as an error.
func TestBdPolecatWorkReleaserUsesAssigneeGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	for _, tc := range []struct {
		name         string
		exit         int
		wantReleased bool
		wantErr      bool
	}{
		{name: "guard held", exit: 0, wantReleased: true},
		{name: "guard no longer held", exit: 13},
		{name: "other failure", exit: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			binDir := filepath.Join(townRoot, "bin")
			if err := os.MkdirAll(binDir, 0755); err != nil {
				t.Fatal(err)
			}
			argsLog := filepath.Join(townRoot, "args.log")
			script := "#!/bin/sh\necho \"$@\" >> '" + argsLog + "'\nexit " + map[int]string{0: "0", 1: "1", 13: "13"}[tc.exit] + "\n"
			_ = writeBDStub(t, binDir, script, "")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			released, err := bdPolecatWorkReleaser{townRoot: townRoot}.ReleaseBead("gt-elvf4", "gastown/polecats/basalt")
			if released != tc.wantReleased || (err != nil) != tc.wantErr {
				t.Fatalf("released=%v err=%v, want released=%v wantErr=%v", released, err, tc.wantReleased, tc.wantErr)
			}
			args, _ := os.ReadFile(argsLog)
			for _, want := range []string{"update gt-elvf4", "--status=open", "--assignee=", "--if-assignee=gastown/polecats/basalt"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("bd args %q missing %q", args, want)
				}
			}
		})
	}
}

// --- gt polecat nuke (gt-vm5g4) ---------------------------------------------

func stubSurvivingBranch(t *testing.T, branch string) {
	t.Helper()
	prev := survivingBranchForBeadFn
	survivingBranchForBeadFn = func(string, string) (string, bool) { return branch, branch != "" }
	t.Cleanup(func() { survivingBranchForBeadFn = prev })
}

func TestNukeReleaseHookedWorkOnlyWhileAssigneeMatches(t *testing.T) {
	p := &polecat.Polecat{Name: "basalt", Rig: "gastown", Issue: "gt-elvf4"}

	t.Run("in_progress on the nuked polecat, no preserved work", func(t *testing.T) {
		stubSurvivingBranch(t, "")
		rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {"in_progress", "gastown/polecats/basalt"}}}
		out := nukeReleaseHookedWork(rel, "/town", "gastown", p, "")
		if !out.Released || len(rel.released) != 1 {
			t.Fatalf("nuke must release its hooked bead: %+v %v", out, rel.released)
		}
		if len(rel.resets) != 0 {
			t.Fatal("nuke leaves the agent-bead reset to sandbox removal")
		}
	})
	t.Run("already re-slung elsewhere", func(t *testing.T) {
		stubSurvivingBranch(t, "polecat/granite/gt-elvf4+1")
		rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {"hooked", "gastown/polecats/granite"}}}
		out := nukeReleaseHookedWork(rel, "/town", "gastown", p, "")
		if out.Released || len(rel.released) != 0 || len(rel.annotated) != 0 {
			t.Fatalf("nuke must not touch another polecat's bead: %+v %v", out, rel.annotated)
		}
	})
	t.Run("no hooked work", func(t *testing.T) {
		rel := &fakeWorkReleaser{readErr: errors.New("must not be read")}
		if out := nukeReleaseHookedWork(rel, "/town", "gastown", &polecat.Polecat{Name: "basalt", Rig: "gastown"}, ""); out != (workReleaseOutcome{}) {
			t.Fatalf("no issue, no work: %+v", out)
		}
	})
}

// Work the nuke preserved (or found) on origin keeps the hook, so sling's
// surviving-branch guard still stops a fresh re-sling from main (gt-ibt8), and
// the bead says how to resume it.
func TestNukeKeepsHookWhenWorkIsPreserved(t *testing.T) {
	p := &polecat.Polecat{Name: "basalt", Rig: "gastown", Issue: "gt-elvf4"}
	for _, tc := range []struct {
		name, pushedRef, surviving, wantBranch string
	}{
		{name: "this nuke pushed the branch", pushedRef: "origin/polecat/basalt/gt-elvf4+123", wantBranch: "polecat/basalt/gt-elvf4+123"},
		{name: "branch already on origin", surviving: "polecat/basalt/gt-elvf4+99", wantBranch: "polecat/basalt/gt-elvf4+99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSurvivingBranch(t, tc.surviving)
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {"in_progress", "gastown/polecats/basalt"}}}
			out := nukeReleaseHookedWork(rel, "/town", "gastown", p, tc.pushedRef)
			if out.Released || len(rel.released) != 0 {
				t.Fatalf("preserved work must keep the hook: %+v", out)
			}
			note := rel.annotated["gt-elvf4"]
			if !strings.Contains(note, "gt sling gt-elvf4 gastown --branch "+tc.wantBranch) {
				t.Fatalf("bead comment lacks the resume hint for %s:\n%s", tc.wantBranch, note)
			}
		})
	}
}
