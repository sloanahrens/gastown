package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// containerChild returns the child bd a container test drives: every call runs through c, and the
// host bd is a build whose line the fake container answers.
func containerChild(c container) childBD {
	return childBD{
		Run:         c.run,
		Env:         c.env,
		Where:       c.where,
		HostVersion: func() ([]byte, error) { return []byte(forkHostVersion), nil },
	}
}

// dockerAnswer returns a dockerRunner that records each command it is given, answers a bd version
// with the container's, leaves the workspace a bd init makes in the directory the command mounts,
// and answers every other call with whereOut and whereExit.
func dockerAnswer(argvLog *[][]string, whereOut []byte, whereExit int) dockerRunner {
	return func(argv []string) ([]byte, int, error) {
		*argvLog = append(*argvLog, argv)
		if slices.Contains(argv, "version") {
			return []byte(forkContainerVersion), 0, nil
		}
		if slices.Contains(argv, "init") {
			if dir := mountOf(argv); dir != "" {
				if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
					return nil, 0, err
				}
			}
		}
		return whereOut, whereExit, nil
	}
}

// mountOf returns the directory a docker command mounts: the host side of its -v pair.
func mountOf(argv []string) string {
	for i, arg := range argv {
		if arg != "-v" || i+1 >= len(argv) {
			continue
		}
		host, _, _ := strings.Cut(argv[i+1], ":")
		return host
	}
	return ""
}

// TestContainerArgsMountOnlyTheDirectoryAndPassOnlyTheDoltNames: one child's docker command mounts
// the directory it works in at /work and nothing else, runs there, and carries no environment
// beyond the BEADS_DOLT_ names, so the home directory, the town and every other inherited value
// stay outside the container (gt-p85pw.117).
func TestContainerArgsMountOnlyTheDirectoryAndPassOnlyTheDoltNames(t *testing.T) {
	t.Parallel()
	c := container{image: "bd-image:test", bin: "/usr/local/bin/bd", doltHost: testDoltHost}
	env := []string{
		"BEADS_DOLT_SERVER_PORT=" + testScratchPort,
		"BEADS_DIR=/home/operator/gt/.beads",
		"BEADS_ACTOR=operator@example.com",
		"GT_DOLT_PORT=3307",
	}
	got := c.args("/scratch/copy", env, []string{"show", "fx-1"})
	want := []string{
		"run", "--rm",
		"-v", "/scratch/copy:" + workDir,
		"-w", workDir,
		"-e", "BEADS_DOLT_SERVER_PORT=" + testScratchPort,
		"-e", doltHostVar + "=" + testDoltHost,
		"bd-image:test", "/usr/local/bin/bd", "show", "fx-1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("docker arguments = %v, want %v", got, want)
	}
	if mounts := countFlag(got, "-v"); mounts != 1 {
		t.Errorf("the command mounts %d directories, want only the one it works in", mounts)
	}
	for i, arg := range got {
		if arg != "-e" {
			continue
		}
		if key, _, _ := strings.Cut(got[i+1], "="); !strings.HasPrefix(key, doltEnvPrefix) {
			t.Errorf("-e names %s, want only the %s names", key, doltEnvPrefix)
		}
	}
}

// TestContainerArgsWithoutADirectoryMountNothing: the help walk names no directory, so it mounts
// none and keeps the image's own working directory, since there is no scratch to isolate.
func TestContainerArgsWithoutADirectoryMountNothing(t *testing.T) {
	t.Parallel()
	c := container{image: "bd-image:test", bin: "bd", doltHost: testDoltHost}
	got := c.args("", nil, []string{"--help"})
	if mounts := countFlag(got, "-v"); mounts != 0 {
		t.Errorf("the command mounts %d directories, want none for a call with no directory", mounts)
	}
	if slices.Contains(got, "-w") {
		t.Errorf("docker arguments = %v, want no working directory named for a call with none", got)
	}
}

// TestContainerEnvMapsTheHostAndKeepsTheOtherNames: only the BEADS_DOLT_ names cross into the
// container, and the host the recording names for this machine becomes the one the container
// reaches it on, since 127.0.0.1 inside the container is the container itself (gt-p85pw.117).
func TestContainerEnvMapsTheHostAndKeepsTheOtherNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			"a loopback host becomes the container's",
			[]string{"BEADS_DOLT_SERVER_HOST=127.0.0.1", "BEADS_DOLT_SERVER_PORT=13307"},
			[]string{doltHostVar + "=" + testDoltHost, "BEADS_DOLT_SERVER_PORT=13307"},
		},
		{
			"no host named is the container's",
			[]string{"BEADS_DOLT_SERVER_PORT=13307"},
			[]string{"BEADS_DOLT_SERVER_PORT=13307", doltHostVar + "=" + testDoltHost},
		},
		{
			"another host is left as it is",
			[]string{"BEADS_DOLT_SERVER_HOST=dolt.example"},
			[]string{"BEADS_DOLT_SERVER_HOST=dolt.example"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := containerEnv(endpointEnv(tc.env), testDoltHost); !slices.Equal(got, tc.want) {
				t.Errorf("containerEnv(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestEndpointEnvKeepsOnlyTheDoltNames: the variables a run inherits for the workspace, the actor or
// the fork's own Dolt names never reach the container, so none of them can steer a run (gt-p85pw.117).
func TestEndpointEnvKeepsOnlyTheDoltNames(t *testing.T) {
	t.Parallel()
	got := endpointEnv([]string{
		"PATH=/usr/bin",
		"BEADS_DIR=/home/operator/gt/.beads",
		"BEADS_ACTOR=operator@example.com",
		"GT_DOLT_PORT=3307",
		"BEADS_DOLT_SERVER_PORT=13307",
	})
	want := []string{"BEADS_DOLT_SERVER_PORT=13307"}
	if !slices.Equal(got, want) {
		t.Errorf("endpointEnv = %v, want %v", got, want)
	}
}

// TestProveBDBuildComparesTheBuild: the container's bd must be the build the host's bd is on the
// release, the build id, the commit and the schema level, while the branch label a checkout's build
// stamps before the commit is not compared, so both spellings of one build pass (gt-p85pw.117).
func TestProveBDBuildComparesTheBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		host      string
		container string
		hostErr   error
		want      string // empty: the proof passes
	}{
		{"one build, the host's branch label", forkHostVersion, forkContainerVersion, nil, ""},
		{"one build, no branch label on either", forkContainerVersion, forkContainerVersion, nil, ""},
		{"another commit", forkHostVersion, "bd version 1.2.2 (688ff7d2: 1111111111) schema<=66\n", nil, "the container's bd is"},
		{"another release", "bd version 1.2.3 (688ff7d2: master@688ff7d249e9) schema<=66\n", forkContainerVersion, nil, "the container's bd is"},
		{"another build id", "bd version 1.2.2 (aaaaaaa: master@688ff7d249e9) schema<=66\n", forkContainerVersion, nil, "the container's bd is"},
		{"another schema level", "bd version 1.2.2 (688ff7d2: master@688ff7d249e9) schema<=67\n", forkContainerVersion, nil, "the container's bd is"},
		{"a line the recorder cannot read", forkHostVersion, "bd: no version here\n", nil, "the container's bd"},
		{"a host bd it cannot read", "", "", errors.New("bd version: not found"), "read the version of the host bd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := testChild(okRun, atWork)
			child.HostVersion = func() ([]byte, error) { return []byte(tc.host), tc.hostErr }
			child.Run = func(string, []string) ([]byte, int, error) {
				return []byte(tc.container), 0, nil
			}
			err := proveBDBuild(child)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("proveBDBuild over %s = %v, want it accepted", tc.name, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("proveBDBuild over %s = %v, want an error naming %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestGuardRunRefusesAVersionMismatchBeforeAnythingIsWritten: a container whose bd is another build
// stops the run before the scratch directory is even made, so nothing is written (gt-p85pw.117).
func TestGuardRunRefusesAVersionMismatchBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	bd := &guardBD{}
	child := testChild(bd.Run, atWork)
	child.Run = func(string, []string) ([]byte, int, error) {
		return []byte("bd version 1.2.2 (688ff7d2: 1111111111) schema<=66\n"), 0, nil
	}
	scratch := filepath.Join(tempDir(t), "scratch")
	err := guardRun(safeCwd(t), scratch, child)
	if err == nil || !strings.Contains(err.Error(), "the container's bd is") {
		t.Fatalf("guardRun over a container of another build = %v, want it refused", err)
	}
	if _, statErr := os.Stat(scratch); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("scratch %s stat = %v, want no directory made for a refused build", scratch, statErr)
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want nothing run for a refused build", bd.args)
	}
}

// TestGuardRunRefusesAnEmptyAnswerFromTheContainer: bd where printing nothing is not bd answering
// that there is no workspace, so the run stops before the init, with nothing written (gt-p85pw.117).
func TestGuardRunRefusesAnEmptyAnswerFromTheContainer(t *testing.T) {
	t.Parallel()
	var ran [][]string
	c := container{
		image:    "bd-image:test",
		bin:      "bd",
		doltHost: testDoltHost,
		base:     scratchServerEnv(),
		docker:   dockerAnswer(&ran, nil, 0),
	}
	scratch := filepath.Join(tempDir(t), "scratch")
	err := guardRun(safeCwd(t), scratch, containerChild(c))
	if err == nil || !strings.Contains(err.Error(), "did not answer with JSON") {
		t.Fatalf("guardRun over a container that answered nothing = %v, want the empty answer refused", err)
	}
	for _, argv := range ran {
		if slices.Contains(argv, "init") {
			t.Errorf("docker ran %v, want no bd init after a refused answer", argv)
		}
	}
	if _, statErr := os.Stat(beadsDirIn(scratch)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the scratch workspace stat = %v, want nothing written", statErr)
	}
}

// TestGuardRunReachesTheInitWhenTheContainerResolvesUnderWork: bd answering with no workspace, or
// one under the mounted directory, is the proof passing, and the scratch's init is the first write
// (gt-p85pw.117).
func TestGuardRunReachesTheInitWhenTheContainerResolvesUnderWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		where []byte
	}{
		{"no workspace yet", []byte(`{"data":{"prefix":"fx"}}`)},
		{"a workspace under the mount", []byte(`{"data":{"path":"/work/.beads"}}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran [][]string
			c := container{
				image:    "bd-image:test",
				bin:      "bd",
				doltHost: testDoltHost,
				base:     scratchServerEnv(),
				docker:   dockerAnswer(&ran, tc.where, 0),
			}
			scratch := filepath.Join(tempDir(t), "scratch")
			if err := guardRun(safeCwd(t), scratch, containerChild(c)); err != nil {
				t.Fatalf("guardRun over %s = %v, want it accepted", tc.name, err)
			}
			inits := 0
			for _, argv := range ran {
				if slices.Contains(argv, "init") {
					inits++
				}
			}
			if inits != 1 {
				t.Errorf("bd ran %d inits, want the scratch's one", inits)
			}
		})
	}
}

// TestGuardRunRefusesAPinnedWorkspaceOutsideWork: BEADS_DIR is not an isolation boundary, so an
// environment that pins it is decided by the workspace bd resolves, never by the pin (gt-p85pw.117).
func TestGuardRunRefusesAPinnedWorkspaceOutsideWork(t *testing.T) {
	t.Parallel()
	pinned := []string{"BEADS_DIR=/work/.beads", "BEADS_DOLT_SERVER_PORT=" + testScratchPort}
	tests := []struct {
		name  string
		where Resolver
		want  string // empty: the guard accepts
	}{
		{"bd resolves the town's workspace anyway", func(string, []string) (string, error) {
			return "/town/.beads", nil
		}, "outside /work"},
		{"bd resolves the pinned workspace", atWork, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &guardBD{}
			scratch := filepath.Join(tempDir(t), "scratch")
			err := guardRun(safeCwd(t), scratch, childWith(bd.Run, pinned, tc.where))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("guardRun over %s = %v, want it accepted", tc.name, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guardRun over %s = %v, want an error naming %q", tc.name, err, tc.want)
			}
			if len(bd.args) != 0 {
				t.Errorf("bd ran %v, want no bd init after a refused answer", bd.args)
			}
		})
	}
}

// countFlag returns how many times flag appears in argv.
func countFlag(argv []string, flag string) int {
	count := 0
	for _, arg := range argv {
		if arg == flag {
			count++
		}
	}
	return count
}
