package seed

import (
	"strings"
	"testing"
)

// The build rig's seed is its pin when the pin covers the build commit, the
// build commit when the pin lacks a module, and an error only when the pin cannot
// be read at all. The image builds the build commit in every case.

func TestBuildRigSeedUsesThePinThatCoversTheBuildCommit(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{rigDir("gastown") + " cat-file -t " + mainSHA: "commit"}}
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/gastown", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a")},
	})
	commit, notes, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, town, f.run)
	if err != nil {
		t.Fatalf("BuildRigSeed: %v", err)
	}
	if commit != pinSHA {
		t.Errorf("seeded at %s, want the pin %s", commit, pinSHA)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "covers build commit") {
		t.Errorf("notes = %v, want one line saying the pin covers the build commit", notes)
	}
}

func TestBuildRigSeedFallsBackToTheBuildCommitWhenThePinLacksAModule(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{rigDir("gastown") + " cat-file -t " + mainSHA: "commit"}}
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/gastown", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a", "github.com/x/new")},
	})
	commit, notes, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, town, f.run)
	if err != nil {
		t.Fatalf("BuildRigSeed on a pin lacking a module = %v, want the build commit, not an error", err)
	}
	if commit != mainSHA {
		t.Errorf("seeded at %s, want the build commit %s", commit, mainSHA)
	}
	want := "seed: pin " + short(pinSHA) + " lacks github.com/x/new"
	if len(notes) != 1 || !strings.HasPrefix(notes[0], want) || !strings.Contains(notes[0], "seeding at the build commit (bump seed-pins)") {
		t.Errorf("notes = %v, want the fallback line naming the module", notes)
	}
}

func TestBuildRigSeedFailsWhenThePinCannotBeRead(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{rigDir("gastown") + " cat-file -t " + mainSHA: "commit"}}
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a")},
	})
	_, _, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, town, f.run)
	if err == nil || !strings.Contains(err.Error(), "not readable in its canonical checkout") {
		t.Fatalf("BuildRigSeed on an unreadable pin = %v, want an unreadable-pin error", err)
	}
}

func TestBuildRigSeedWithNoPinUsesTheBuildCommit(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{rigDir("gastown") + " cat-file -t " + mainSHA: "commit"}}
	commit, notes, err := BuildRigSeed(nil, "gastown", mainSHA, town, f.run)
	if err != nil || commit != mainSHA || len(notes) != 0 {
		t.Errorf("BuildRigSeed without a pin = %s, %v, %v; want the build commit and no notes", commit, notes, err)
	}
}
