package imagectx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fileAt makes a regular file at rel under the output, where a later step makes a
// directory, so that step fails.
func (w *world) fileAt(rel string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.out, rel), []byte("not a directory\n"), 0o644); err != nil {
		w.t.Fatalf("fileAt %s: %v", rel, err)
	}
}

// These cases drive each failure path of the build, one step per row, through the
// fault knobs of the world. Each row names the step it breaks: a row that breaks
// a step after the output was written to proves the context is still refused, and
// a row that breaks a write proves the write's error reaches the caller.

// TestContextRefusesAnUnusableInput checks each input the build refuses before it
// reads any source: a missing source, an output directory that does not exist, a
// bd pin that is not bd or not a full sha, and a seed rig named twice.
func TestContextRefusesAnUnusableInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(w *world, in *Inputs)
		want   string
	}{
		{"no source checkout", func(_ *world, in *Inputs) { in.Src = "" }, "no source checkout named"},
		{"an output directory that does not exist", func(_ *world, in *Inputs) {
			in.Out = in.Out + "/missing"
		}, "cannot read the output directory"},
		{"a bd pin not named bd", func(_ *world, in *Inputs) { in.BD.Name = "om" }, "the bd pin is named \"om\""},
		{"a bd pin commit that is not a full sha", func(_ *world, in *Inputs) {
			in.BD.Commit = "abc123"
		}, "the bd pin commit \"abc123\" is not a full 40-character sha"},
		{"a seed rig named twice", func(_ *world, in *Inputs) {
			in.Seeds = append(in.Seeds, in.Seeds[1])
		}, "seed-pins names rig devops twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			in := w.inputs(t.TempDir())
			tc.mutate(w, &in)
			_, err := Build(context.Background(), in, w.seams())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want it to contain %q", err, tc.want)
			}
			if len(w.goCalls) != 0 {
				t.Errorf("a refused input still ran a build: %v", w.goCalls)
			}
		})
	}
}

// TestContextStopsOnAFailedStep breaks one step of the build per row and checks
// the failure reaches the caller naming that step. A row that breaks the output
// directory after a step wrote into it proves the later steps refuse rather than
// write into a broken context.
func TestContextStopsOnAFailedStep(t *testing.T) {
	t.Parallel()
	gtBuild := func(w *world) func(args []string) {
		return func(args []string) {
			if contains(args, "./cmd/gt") {
				w.makeDirAt("go-floor.sh")
			}
		}
	}
	cases := []struct {
		name   string
		mutate func(w *world, in *Inputs)
		want   string
	}{
		{"the tree status cannot be read", func(w *world, _ *Inputs) {
			w.failGit = []string{"status"}
		}, "cannot read the status of"},
		{"HEAD cannot be read", func(w *world, _ *Inputs) {
			w.failGit = []string{"rev-parse --verify HEAD^{commit}"}
		}, "cannot read HEAD of"},
		{"the bin directory cannot be made once the status is read", func(w *world, _ *Inputs) {
			w.onStatus = func() { w.fileAt("bin") }
		}, "cannot make the bin directory"},
		{"the build rig's tree cannot be archived", func(w *world, _ *Inputs) {
			w.failGit = []string{"archive --format=tar " + gastownRev}
		}, "cannot archive"},
		{"a seeded rig's pin cannot be fetched", func(w *world, _ *Inputs) {
			w.failRun = []string{"fetch --depth 1 " + devopsURL}
		}, "fetching devops from its pin"},
		{"a seeded rig's main cannot be read", func(w *world, _ *Inputs) {
			w.failGit = []string{"ls-remote"}
		}, "cannot read the main of rig devops"},
		{"a seeded rig's main answers two refs", func(w *world, _ *Inputs) {
			w.lsRemote[devopsURL] = devopsPin + "\trefs/heads/main\n" + devopsPin + "\trefs/heads/main\n"
		}, "the source answered 2 refs for main, want one"},
		{"a seeded rig's main is not a sha", func(w *world, _ *Inputs) {
			w.lsRemote[devopsURL] = "zz\trefs/heads/main\n"
		}, "rig devops: the source's main ref is not a full sha"},
		{"a seeded rig's main cannot be fetched", func(w *world, _ *Inputs) {
			w.failRun = []string{"--unshallow"}
		}, "cannot fetch the main of rig beads"},
		{"a seeded rig's fetched main cannot be read", func(w *world, _ *Inputs) {
			w.failGit = []string{"FETCH_HEAD"}
		}, "cannot read the main fetched for rig beads"},
		{"a seeded rig's pin cannot be archived", func(w *world, _ *Inputs) {
			w.failGit = []string{"archive --format=tar " + devopsPin}
		}, "cannot archive"},
		{"a later rig's fetch directory cannot be made once the output is broken", func(w *world, _ *Inputs) {
			w.onArchive[devopsPin] = w.breakOut
		}, "cannot make a fetch directory for rig beads"},
		{"bd's fetch directory cannot be made once the output is broken", func(w *world, _ *Inputs) {
			w.onArchive[beadsPin] = w.breakOut
		}, "cannot make a fetch directory for bd"},
		{"bd cannot be fetched at its pin", func(w *world, _ *Inputs) {
			w.failRun = []string{"fetch --depth 1 " + beadsURL + " " + bdPin}
		}, "fetching bd from its pin"},
		{"bd does not compile", func(w *world, _ *Inputs) {
			w.failGo = []string{"./cmd/bd"}
		}, "cannot build bd at"},
		{"gt does not build", func(w *world, _ *Inputs) {
			w.failGo = []string{"./cmd/gt"}
		}, "cannot build gt at"},
		{"the output is broken before the seeds are unpacked", func(w *world, _ *Inputs) {
			w.onGo = func(args []string) {
				if contains(args, "./cmd/sweep-run") {
					w.breakOut()
				}
			}
		}, "cannot unpack the gastown tree"},
		{"the commit's go-floor.sh cannot be read", func(w *world, _ *Inputs) {
			w.failGit = []string{"show " + gastownRev + ":deploy/polecat/go-floor.sh"}
		}, "cannot read deploy/polecat/go-floor.sh"},
		{"go-floor.sh cannot be written", func(w *world, _ *Inputs) {
			w.onGo = gtBuild(w)
		}, "cannot write go-floor.sh"},
		{"bd.commit cannot be written", func(w *world, _ *Inputs) {
			w.onGo = func(args []string) {
				if contains(args, "./cmd/gt") {
					w.makeDirAt("bd.commit")
				}
			}
		}, "cannot write bd.commit"},
		{"the build rig's go.mod is a directory", func(w *world, _ *Inputs) {
			delete(w.commits[gastownRev], "go.mod")
			w.commits[gastownRev]["go.mod/x"] = "x\n"
		}, "cannot read the go.mod of rig gastown"},
		{"a go.mod does not parse", func(w *world, _ *Inputs) {
			w.commits[gastownRev]["go.mod"] = "module x\n\ngo 1.27\nrequire (\n"
		}, "cannot parse the go.mod of rig gastown"},
		{"no rig go.mod has a go line", func(w *world, _ *Inputs) {
			w.commits[gastownRev]["go.mod"] = "module x\n"
			w.commits[devopsPin]["go.mod"] = "module d\n"
			w.commits[beadsPin]["go.mod"] = "module b\n"
			w.commits[beadsMain]["go.mod"] = "module b\n"
		}, "no rig go.mod has a go line"},
		{"a go line is not a version", func(w *world, _ *Inputs) {
			w.commits[gastownRev]["go.mod"] = "module x\n\ngo 1.27rc1\n"
		}, "go line \"1.27rc1\" is not a version"},
		{"a go line number is out of range", func(w *world, _ *Inputs) {
			w.commits[gastownRev]["go.mod"] = "module x\n\ngo 1.99999999999999999999\n"
		}, "is not a version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			in := w.inputs(t.TempDir())
			tc.mutate(w, &in)
			_, err := Build(context.Background(), in, w.seams())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want it to contain %q\n%s", err, tc.want, w.dump())
			}
			assertNoEngine(t, w.calls, w.goCalls)
		})
	}
}

// TestContextRefusesAMissingSeam checks Build refuses to run with any of its four
// seams unset, before it reads an input.
func TestContextRefusesAMissingSeam(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	full := w.seams()
	for name, s := range map[string]Seams{
		"no run":     {Git: full.Git, GitEnv: full.GitEnv, Go: full.Go},
		"no git":     {Run: full.Run, GitEnv: full.GitEnv, Go: full.Go},
		"no git env": {Run: full.Run, Git: full.Git, Go: full.Go},
		"no go":      {Run: full.Run, Git: full.Git, GitEnv: full.GitEnv},
	} {
		_, err := Build(context.Background(), w.inputs(t.TempDir()), s)
		if err == nil || !strings.Contains(err.Error(), "seam") {
			t.Errorf("%s: Build error = %v, want the seam refusal", name, err)
		}
	}
	if len(w.calls) != 0 || len(w.goCalls) != 0 {
		t.Errorf("a build without its seams still ran a command: %v %v", w.calls, w.goCalls)
	}
}

// TestRunArgvNamesWhyItStopped checks a command that cannot start, or whose
// context ends first, fails naming why. No process is started: an expired or
// cancelled context stops the start before it forks, and a missing binary has
// nothing to run.
func TestRunArgvNamesWhyItStopped(t *testing.T) {
	t.Parallel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		argv []string
		want string
	}{
		{"the deadline has passed", expired, []string{"git", "version"}, "git timed out"},
		{"the context is cancelled", cancelled, []string{"git", "version"}, "git was canceled"},
		{"the command does not exist", context.Background(), []string{"/nonexistent/polecat-imagectx-probe"}, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runArgv(tc.ctx, t.TempDir(), nil, tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runArgv error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
