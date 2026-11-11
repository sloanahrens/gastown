package seed

import (
	"strings"
	"testing"
)

// The build rig's seed is its pin when the pin covers the build commit, the
// build commit when the pin lacks a module, and an error only when the pin cannot
// be read at all. The image builds the build commit in every case.

// buildRigFixture is a guard table whose build rig, gastown, has its bare repo
// holding the build commit.
func buildRigFixture(t *testing.T) *guardGit {
	t.Helper()
	f := newGuardGit(t)
	f.answer[f.bare("gastown")+" cat-file -t "+mainSHA] = "commit"
	return f
}

func TestBuildRigSeedUsesThePinThatCoversTheBuildCommit(t *testing.T) {
	t.Parallel()
	f := buildRigFixture(t)
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/gastown", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a")},
	})
	commit, notes, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, f.town, "", f.run)
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
	f := buildRigFixture(t)
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/gastown", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a", "github.com/x/new")},
	})
	commit, notes, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, f.town, "", f.run)
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
	f := buildRigFixture(t)
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a")},
	})
	_, _, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, f.town, "", f.run)
	if err == nil || !strings.Contains(err.Error(), "not readable in its bare repo "+f.bare("gastown")) {
		t.Fatalf("BuildRigSeed on an unreadable pin = %v, want an unreadable-pin error naming the bare repo", err)
	}
}

func TestBuildRigSeedWithNoPinUsesTheBuildCommit(t *testing.T) {
	t.Parallel()
	f := buildRigFixture(t)
	commit, notes, err := BuildRigSeed(nil, "gastown", mainSHA, f.town, "", f.run)
	if err != nil || commit != mainSHA || len(notes) != 0 {
		t.Errorf("BuildRigSeed without a pin = %s, %v, %v; want the build commit and no notes", commit, notes, err)
	}
}

// A build rig with no bare repo under the town is refused before git runs,
// naming the bare path.
func TestBuildRigSeedRefusesABuildRigWithNoBareRepo(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	_, _, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, f.town, "", f.run)
	if err == nil || !strings.Contains(err.Error(), f.town+"/gastown/.repo.git") {
		t.Fatalf("BuildRigSeed with no bare repo = %v, want an error naming the bare path", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("BuildRigSeed ran git against a rig with no bare repo: %v", f.calls)
	}
}

// A build source that does not hold the build commit is named, with advice
// that fits it: fetching the town's copy cannot help.
func TestBuildRigSeedFailsWhenTheBuildSourceLacksTheBuildCommit(t *testing.T) {
	t.Parallel()
	const buildSrc = "/build/checkout"
	f := newGuardGit(t)
	_, _, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, f.town, buildSrc, f.run)
	if err == nil {
		t.Fatal("BuildRigSeed on a build source lacking the build commit = nil, want an error")
	}
	for _, want := range []string{"cannot read the build commit", buildSrc, "build from a checkout that holds the commit being built"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// With a build source, the build rig's commit check and pin comparison are
// asked of that directory, never of the town's bare repo.
func TestBuildRigSeedReadsTheBuildSource(t *testing.T) {
	t.Parallel()
	const buildSrc = "/build/checkout"
	f := newGuardGit(t)
	f.answer[buildSrc+" cat-file -t "+mainSHA] = "commit"
	seedRigAt(f, buildSrc, mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/gastown", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/gastown", "github.com/x/a")},
	})
	commit, _, err := BuildRigSeed([]Source{{Rig: "gastown", Commit: pinSHA}}, "gastown", mainSHA, f.town, buildSrc, f.run)
	if err != nil {
		t.Fatalf("BuildRigSeed with a build source: %v", err)
	}
	if commit != pinSHA {
		t.Errorf("seeded at %s, want the pin %s", commit, pinSHA)
	}
	if len(f.calls) == 0 {
		t.Fatal("BuildRigSeed made no git calls")
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, buildSrc+" ") {
			t.Errorf("read %q, want every build rig read under the build source %s", c, buildSrc)
		}
	}
}
