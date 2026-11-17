package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gatewayVerbs is the policy file gatewayBuilder's checkout holds. BuildGateway
// copies it as bytes: this test never parses it.
const gatewayVerbs = `{"verbs": [{"path": "show", "class": "read"}]}` + "\n"

// gatewayBuilder is builderFor with a gt checkout whose rev holds the
// gateway's policy file at deploy/bd/verbs.json, as the commit bdgw is built
// from does.
func gatewayBuilder(t *testing.T) (Builder, *fakeRunner) {
	t.Helper()
	b, run := builderFor(t)
	b.GTSrc = t.TempDir()
	run.files = map[string][]byte{testRev + ":" + verbsRelPath: []byte(gatewayVerbs)}
	return b, run
}

// TestBuildsBdgw: the gateway image's binary is cross-compiled for linux/arm64
// from the gt checkout at rev, in a clean worktree of it, and lands in the
// output directory under the name the image copies. It is the only binary the
// call writes: the image runs the real bd, not a shim (gt-gi5lhe). The policy
// file is staged beside it, and is no binary.
func TestBuildsBdgw(t *testing.T) {
	t.Parallel()
	b, run := gatewayBuilder(t)
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildGateway: %v", err)
	}
	want := []string{filepath.Join(b.Out, "bdgw")}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildGateway paths = %q, want %q", paths, want)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("BuildGateway returned %s, which is not written: %v", p, err)
		}
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got, want := strings.Join(names, " "), "bdgw verbs.json"; got != want {
		t.Errorf("the output directory holds %q after the build, want %q", got, want)
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d builds, want the one bdgw build", len(run.calls))
	}
	call := run.calls[0]
	for _, want := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOENV=off"} {
		if !contains(call.env, want) {
			t.Errorf("bdgw build env = %q, want %s among them", call.env, want)
		}
	}
	wantArgv := []string{"go", "build", "-trimpath", "-o", "OUT", BDGWPackage}
	if got := withOut(call.argv); !reflect.DeepEqual(got, wantArgv) {
		t.Errorf("bdgw build argv = %q, want %q", call.argv, wantArgv)
	}
	wantAdds := []worktreeAdd{{src: b.GTSrc, commit: testRev}}
	if got := addsOf(run); !reflect.DeepEqual(got, wantAdds) {
		t.Errorf("worktrees added = %+v, want %+v", got, wantAdds)
	}
}

// addsOf is the source and commit of every worktree the fake git added.
func addsOf(run *fakeRunner) []worktreeAdd {
	var added []worktreeAdd
	for _, a := range run.adds {
		added = append(added, worktreeAdd{src: a.src, commit: a.commit})
	}
	return added
}

// TestGatewayBuildContext: BuildGateway assembles the build context the gt-bd
// image is built from, and it holds the two files the image's COPY lines read:
// bdgw and the gateway's policy file, verbs.json, staged with the bytes of the
// rev bdgw is built from (gt-3h694z). A rev without the policy file is refused
// before any build runs, so the image never starts without it.
func TestGatewayBuildContext(t *testing.T) {
	t.Parallel()
	t.Run("the context holds bdgw and verbs.json", func(t *testing.T) {
		t.Parallel()
		b, _ := gatewayBuilder(t)
		if _, err := b.BuildGateway(context.Background(), testRev); err != nil {
			t.Fatalf("BuildGateway: %v", err)
		}
		entries, err := os.ReadDir(b.Out)
		if err != nil {
			t.Fatalf("reading the build context: %v", err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if got, want := strings.Join(names, " "), "bdgw verbs.json"; got != want {
			t.Errorf("the build context holds %q, want %q", got, want)
		}
		got, err := os.ReadFile(filepath.Join(b.Out, verbsName))
		if err != nil {
			t.Fatalf("reading the staged %s: %v", verbsName, err)
		}
		if string(got) != gatewayVerbs {
			t.Errorf("the staged %s = %q, want the built rev's policy %q", verbsName, got, gatewayVerbs)
		}
	})
	t.Run("the policy is read at the built rev, not the working tree", func(t *testing.T) {
		t.Parallel()
		b, _ := gatewayBuilder(t)
		// The working tree holds a different policy from the commit bdgw is
		// built from. The image must carry the commit's bytes, or its policy
		// and its binary can name different revisions.
		if err := os.MkdirAll(filepath.Join(b.GTSrc, "deploy", "bd"), 0o755); err != nil {
			t.Fatalf("making the working tree's deploy/bd: %v", err)
		}
		if err := os.WriteFile(filepath.Join(b.GTSrc, verbsRelPath), []byte("{\"working\": true}\n"), 0o644); err != nil {
			t.Fatalf("writing the working tree's verbs.json: %v", err)
		}
		if _, err := b.BuildGateway(context.Background(), testRev); err != nil {
			t.Fatalf("BuildGateway: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(b.Out, verbsName))
		if err != nil {
			t.Fatalf("reading the staged %s: %v", verbsName, err)
		}
		if string(got) != gatewayVerbs {
			t.Errorf("the staged %s = %q, want the built rev's policy %q, not the working tree's", verbsName, got, gatewayVerbs)
		}
	})
	t.Run("a rev without the policy file is refused", func(t *testing.T) {
		t.Parallel()
		b, run := gatewayBuilder(t)
		delete(run.files, testRev+":"+verbsRelPath)
		paths, err := b.BuildGateway(context.Background(), testRev)
		if err == nil {
			t.Fatalf("BuildGateway built without the policy file (paths %q)", paths)
		}
		if !strings.Contains(err.Error(), "verbs.json") {
			t.Errorf("BuildGateway error = %v, want it to name verbs.json", err)
		}
		if len(paths) != 0 {
			t.Errorf("a refused call listed %q, want no paths", paths)
		}
		if len(run.calls) != 0 {
			t.Errorf("a refused call ran %d builds, want none", len(run.calls))
		}
		entries, err := os.ReadDir(b.Out)
		if err != nil {
			t.Fatalf("reading the build context: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("a refused call left %d entries in the build context, want none", len(entries))
		}
	})
}

// TestBuildGatewayFailsWithoutLeavingPartialOutput: a build that fails returns
// its error, lists no paths and leaves the output directory empty.
func TestBuildGatewayFailsWithoutLeavingPartialOutput(t *testing.T) {
	t.Parallel()
	b, run := gatewayBuilder(t)
	run.failPkg = BDGWPackage
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil {
		t.Fatalf("a failed bdgw build returned no error (paths %q)", paths)
	}
	if len(paths) != 0 {
		t.Errorf("a failed bdgw build listed %q, want no paths", paths)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed bdgw build left %d entries in the output directory, want none", len(entries))
	}
}

// TestBuildGatewayRefusesWhenThePolicyCannotBeStaged: a policy file the call
// cannot stage fails before any build runs, so a build context that would hold
// no verbs.json is never built.
func TestBuildGatewayRefusesWhenThePolicyCannotBeStaged(t *testing.T) {
	t.Parallel()
	b, run := gatewayBuilder(t)
	b.writeFile = func(string, []byte, os.FileMode) error { return errors.New("write refused") }
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil || !strings.Contains(err.Error(), "cannot stage verbs.json") {
		t.Fatalf("BuildGateway = %q, %v; want an error naming the verbs.json staging", paths, err)
	}
	if !strings.Contains(err.Error(), "write refused") {
		t.Errorf("BuildGateway error = %v, want it to carry the seam's refusal", err)
	}
	if len(paths) != 0 {
		t.Errorf("a refused call listed %q, want no paths", paths)
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused call ran %d builds, want none", len(run.calls))
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the build context: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused call left %d entries in the build context, want none", len(entries))
	}
}

// TestBuildGatewayFailsWhenTheOutputCannotBeMoved: an output that cannot be
// moved into Out fails the call, and the staging directory is removed, so Out
// holds nothing but what was already there.
func TestBuildGatewayFailsWhenTheOutputCannotBeMoved(t *testing.T) {
	t.Parallel()
	b, _ := gatewayBuilder(t)
	// rename(2) cannot put a file over a directory, so bdgw cannot be moved in.
	if err := os.Mkdir(filepath.Join(b.Out, "bdgw"), 0o755); err != nil {
		t.Fatalf("making the blocking directory: %v", err)
	}
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil || !strings.Contains(err.Error(), "cannot move bdgw") {
		t.Fatalf("BuildGateway = %q, %v; want an error naming the bdgw move", paths, err)
	}
	if len(paths) != 0 {
		t.Errorf("a failed move listed %q, want no paths", paths)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "bdgw" {
		t.Errorf("the output directory holds %d entries after a failed move, want only the blocking bdgw directory", len(entries))
	}
}

// TestBuildGatewayMoveUsesTheFileOpsSeam: the move into Out goes through the
// same fileOps seam the plugin and mango moves use, so a refused rename fails
// the call the way a real one does (gt-xye8y5).
func TestBuildGatewayMoveUsesTheFileOpsSeam(t *testing.T) {
	t.Parallel()
	b, _ := gatewayBuilder(t)
	b.rename = func(string, string) error { return errors.New("rename refused") }
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil || !strings.Contains(err.Error(), "cannot move bdgw") {
		t.Fatalf("BuildGateway = %q, %v; want an error naming the bdgw move", paths, err)
	}
	if !strings.Contains(err.Error(), "rename refused") {
		t.Errorf("BuildGateway error = %v, want it to carry the seam's refusal", err)
	}
	if len(paths) != 0 {
		t.Errorf("a failed move listed %q, want no paths", paths)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed move left %d entries in the output directory, want none", len(entries))
	}
}

// refuseVerbsMove refuses the move whose destination is verbs.json and runs
// every other rename, so a test can fail the policy move alone.
func refuseVerbsMove(oldpath, newpath string) error {
	if filepath.Base(newpath) == verbsName {
		return errors.New("verbs move refused")
	}
	return os.Rename(oldpath, newpath)
}

// TestBuildGatewayTakesBdgwBackOutWhenThePolicyCannotBeMoved: a policy file the
// build context cannot take fails the call, and the bdgw already moved is taken
// back out, so a failed call leaves the context as it found it. When the bdgw
// cannot be taken back out either, the error reports that too, so a caller
// never reads a clean refusal over a context this call left changed.
func TestBuildGatewayTakesBdgwBackOutWhenThePolicyCannotBeMoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remove func(string) error
		want   []string
	}{
		{
			name: "the bdgw is taken back out",
			want: []string{"cannot move verbs.json", "verbs move refused"},
		},
		{
			name:   "a bdgw that cannot be taken back out is reported",
			remove: func(string) error { return errors.New("remove refused") },
			want:   []string{"cannot move verbs.json", "cannot take bdgw back out of", "remove refused"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, _ := gatewayBuilder(t)
			b.rename = refuseVerbsMove
			b.remove = tc.remove
			paths, err := b.BuildGateway(context.Background(), testRev)
			if err == nil {
				t.Fatalf("BuildGateway = %q, nil; want an error naming the verbs.json move", paths)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("BuildGateway error = %v, want it to contain %q", err, want)
				}
			}
			if len(paths) != 0 {
				t.Errorf("a failed policy move listed %q, want no paths", paths)
			}
			entries, readErr := os.ReadDir(b.Out)
			if readErr != nil {
				t.Fatalf("reading the build context: %v", readErr)
			}
			want := 0
			if tc.remove != nil {
				// The bdgw the undo could not take back out stays, and is named.
				want = 1
			}
			if len(entries) != want {
				t.Errorf("the build context holds %d entries after a failed policy move, want %d", len(entries), want)
			}
			if want == 1 && entries[0].Name() != "bdgw" {
				t.Errorf("the build context holds %q, want the bdgw the undo could not remove", entries[0].Name())
			}
		})
	}
}

// TestBuildGatewayRefusesARevTheSourceLacks: a rev the gt checkout does not hold
// is refused before any build runs, so a stamp never names a commit the build
// did not read.
func TestBuildGatewayRefusesARevTheSourceLacks(t *testing.T) {
	t.Parallel()
	b, run := gatewayBuilder(t)
	if _, err := b.BuildGateway(context.Background(), "1111111111111111111111111111111111111111"); err == nil {
		t.Fatal("BuildGateway built from a rev the gt checkout does not hold")
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused call ran %d builds, want none", len(run.calls))
	}
}

// TestBuildGatewayRefusesBeforeAnyBuild: each input BuildGateway cannot build
// from is refused before the first build runs, so the refusal writes nothing.
func TestBuildGatewayRefusesBeforeAnyBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Builder)
	}{
		{"relative output directory", func(b *Builder) { b.Out = "out" }},
		{"no output directory", func(b *Builder) { b.Out = "" }},
		{"no gt source", func(b *Builder) { b.GTSrc = "" }},
		{"no runner", func(b *Builder) { b.Run = nil }},
		{"no git", func(b *Builder) { b.Git = nil }},
		{"gt source inside a canonical clone", func(b *Builder) { b.GTSrc = filepath.Join("town", "rig", "mayor", "rig") }},
		{"output directory that does not exist", func(b *Builder) { b.Out = filepath.Join(b.Out, "missing") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := gatewayBuilder(t)
			tc.mutate(&b)
			if _, err := b.BuildGateway(context.Background(), testRev); err == nil {
				t.Fatal("BuildGateway accepted an input it cannot build from")
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused call ran %d builds, want none", len(run.calls))
			}
		})
	}
}
