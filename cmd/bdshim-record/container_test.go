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
// with the container's, answers a bd where with whereOut and whereExit, leaves the workspace a bd init
// makes in the directory the command mounts, and exits 0 for every other call.
func dockerAnswer(argvLog *[][]string, whereOut []byte, whereExit int) dockerRunner {
	return func(_ []string, argv []string) ([]byte, int, error) {
		*argvLog = append(*argvLog, argv)
		switch {
		case slices.Contains(argv, "version"):
			return []byte(forkContainerVersion), 0, nil
		case slices.Contains(argv, "where"):
			return whereOut, whereExit, nil
		case slices.Contains(argv, "init"):
			if dir := mountOf(argv); dir != "" {
				if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
					return nil, 0, err
				}
			}
		}
		return nil, 0, nil
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
// beyond the BEADS_DOLT_ names, the recorder's pinned identity and bd's test-server opt-in, so the
// home directory, the town and every other inherited value stay outside the container. Each -e names
// a variable and never its value, so a secret cannot be read out of the command line
// (gt-p85pw.117, gt-czb3c).
func TestContainerArgsMountOnlyTheDirectoryAndPassOnlyTheDoltNames(t *testing.T) {
	t.Parallel()
	c := container{
		image:    "bd-image:test",
		bin:      "/usr/local/bin/bd",
		doltHost: testDoltHost,
		base: []string{
			"BEADS_DOLT_SERVER_PORT=" + testScratchPort,
			"BEADS_DOLT_SERVER_DATABASE=town_db",
			"BEADS_DIR=/home/operator/gt/.beads",
			"BEADS_ACTOR=operator@example.com",
			"GT_DOLT_PORT=3307",
		},
	}
	env := containerEnv(c.env("/scratch/copy"), c.doltHost)
	got := c.args("/scratch/copy", env, "bdshim-record-test", []string{"show", "fx-1"})
	want := []string{
		"run", "--rm", "--name", "bdshim-record-test",
		"-v", "/scratch/copy:" + workDir,
		"-w", workDir,
		"-e", "BEADS_DOLT_SERVER_PORT",
	}
	for _, pinned := range pinnedIdentity {
		want = append(want, "-e", pinned[0])
	}
	want = append(want, "-e", doltHostVar, "-e", testServerOptIn[0], "bd-image:test", "/usr/local/bin/bd", "show", "fx-1")
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
		name := got[i+1]
		if strings.Contains(name, "=") {
			t.Errorf("-e %s carries a value on the command line, want only the name", name)
		}
		if !strings.HasPrefix(name, doltEnvPrefix) && !pinnedName(name) && name != testServerOptIn[0] {
			t.Errorf("-e names %s, want a %s name, a pinned identity name or %s", name, doltEnvPrefix, testServerOptIn[0])
		}
	}
	if value, ok := envValue(env, "BEADS_DOLT_SERVER_PORT"); !ok || value != testScratchPort {
		t.Errorf("the client environment names the port %q, found=%v, want it to carry %s", value, ok, testScratchPort)
	}
}

// pinnedName reports whether name is one of the identity variables the recorder pins.
func pinnedName(name string) bool {
	for _, pinned := range pinnedIdentity {
		if pinned[0] == name {
			return true
		}
	}
	return false
}

// TestContainerEnvPinsTheIdentity: the actor and the git identity bd records are the recorder's own
// rather than the operator's, so a golden's author reads the same on every machine (gt-p85pw.103).
func TestContainerEnvPinsTheIdentity(t *testing.T) {
	t.Parallel()
	c := container{base: []string{
		"BEADS_ACTOR=operator@example.com",
		"BD_ACTOR=operator",
		"GIT_AUTHOR_NAME=Operator",
		"BEADS_DOLT_SERVER_PORT=" + testScratchPort,
	}}
	env := c.env("")
	for _, pinned := range pinnedIdentity {
		if got, ok := envValue(env, pinned[0]); !ok || got != pinned[1] {
			t.Errorf("%s = %q, found=%v, want the recorder's %q", pinned[0], got, ok, pinned[1])
		}
	}
	if got, _ := envValue(env, "GIT_AUTHOR_NAME"); got == "Operator" {
		t.Errorf("GIT_AUTHOR_NAME = %q, want the recorder's identity, not the operator's", got)
	}
	if _, ok := envValue(env, "BEADS_DOLT_SERVER_PORT"); !ok {
		t.Errorf("the endpoint name is missing from %v", env)
	}
}

// TestEndpointEnvDropsTheStoreSelector: a database named in the operator's shell would aim a run at
// a store the copy's own metadata does not name, so the selector is dropped rather than passed in
// (gt-p85pw.92).
func TestEndpointEnvDropsTheStoreSelector(t *testing.T) {
	t.Parallel()
	got := endpointEnv([]string{
		"BEADS_DOLT_SERVER_DATABASE=town_db",
		"BEADS_DOLT_SERVER_PORT=" + testScratchPort,
	})
	want := []string{"BEADS_DOLT_SERVER_PORT=" + testScratchPort}
	if !slices.Equal(got, want) {
		t.Errorf("endpointEnv = %v, want the selector dropped, %v", got, want)
	}
}

// TestContainerArgsWithoutADirectoryMountNothing: the help walk names no directory, so it mounts
// none and keeps the image's own working directory, since there is no scratch to isolate.
func TestContainerArgsWithoutADirectoryMountNothing(t *testing.T) {
	t.Parallel()
	c := container{image: "bd-image:test", bin: "bd", doltHost: testDoltHost}
	got := c.args("", nil, "bdshim-record-test", []string{"--help"})
	if mounts := countFlag(got, "-v"); mounts != 0 {
		t.Errorf("the command mounts %d directories, want none for a call with no directory", mounts)
	}
	if slices.Contains(got, "-w") {
		t.Errorf("docker arguments = %v, want no working directory named for a call with none", got)
	}
}

// TestContainerEnvMapsTheHostAndKeepsTheOtherNames: only the BEADS_DOLT_ names and the recorder's
// own opt-in cross into the container, and the host the recording names for this machine becomes the
// one the container reaches it on, since 127.0.0.1 inside the container is the container itself
// (gt-p85pw.117, gt-czb3c).
func TestContainerEnvMapsTheHostAndKeepsTheOtherNames(t *testing.T) {
	t.Parallel()
	optIn := testServerOptIn[0] + "=" + testServerOptIn[1]
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			"a loopback host becomes the container's",
			[]string{"BEADS_DOLT_SERVER_HOST=127.0.0.1", "BEADS_DOLT_SERVER_PORT=13307"},
			[]string{doltHostVar + "=" + testDoltHost, "BEADS_DOLT_SERVER_PORT=13307", optIn},
		},
		{
			"no host named is the container's",
			[]string{"BEADS_DOLT_SERVER_PORT=13307"},
			[]string{"BEADS_DOLT_SERVER_PORT=13307", doltHostVar + "=" + testDoltHost, optIn},
		},
		{
			"another host is left as it is",
			[]string{"BEADS_DOLT_SERVER_HOST=dolt.example"},
			[]string{"BEADS_DOLT_SERVER_HOST=dolt.example", optIn},
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

// TestContainerPinsTheTestServerOptIn: bd refuses to make a database on a server it cannot vouch
// for unless BEADS_TEST_SERVER=1 is set, and a recording's scratch database is always made on a
// throwaway server, so every container call carries the recorder's own opt-in. An operator's value
// never decides: the pin is the recorder's constant, whether the operator's environment sets the
// name to 0, to 1 or not at all (gt-czb3c).
func TestContainerPinsTheTestServerOptIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base []string
	}{
		{"the operator sets nothing", nil},
		{"the operator sets 0", []string{"BEADS_TEST_SERVER=0"}},
		{"the operator sets 1", []string{"BEADS_TEST_SERVER=1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var envs, argvs [][]string
			c := container{
				image:    "bd-image:test",
				bin:      "bd",
				doltHost: testDoltHost,
				base:     append([]string{"BEADS_DOLT_SERVER_PORT=" + testScratchPort}, tc.base...),
				docker: func(env []string, argv []string) ([]byte, int, error) {
					envs = append(envs, env)
					argvs = append(argvs, argv)
					if slices.Contains(argv, "where") {
						return []byte(`{"data":{"path":"/work/.beads"}}`), 0, nil
					}
					return []byte(forkContainerVersion), 0, nil
				},
			}
			child := containerChild(c)
			if _, _, err := child.Run("", []string{"version"}); err != nil {
				t.Fatalf("the container's version call = %v, want it run", err)
			}
			if _, err := child.Where("", child.Env("")); err != nil {
				t.Fatalf("the container's where call = %v, want it answered", err)
			}
			if len(envs) != 2 {
				t.Fatalf("docker ran %d calls, want the version and the where", len(envs))
			}
			for i, env := range envs {
				if got, ok := envValue(env, testServerOptIn[0]); !ok || got != testServerOptIn[1] {
					t.Errorf("call %d carries %s=%q, found=%v, want the recorder's %s", i, testServerOptIn[0], got, ok, testServerOptIn[1])
				}
				if !slices.Contains(argvs[i], testServerOptIn[0]) {
					t.Errorf("call %d docker arguments = %v, want -e %s", i, argvs[i], testServerOptIn[0])
				}
			}
		})
	}
}

// TestTownServerRefusalPrecedesThePin: the pin is added when a call's environment is built, after
// the guard has read the environment it refuses the town's server from, so a run aimed at the
// town's Dolt server is still refused before any database is made and no database-making call ever
// carries the pin. The version call the build proof runs first is not database-making and does carry
// it (gt-czb3c).
func TestTownServerRefusalPrecedesThePin(t *testing.T) {
	t.Parallel()
	const townPort = "13399"
	town := tempDir(t)
	mustWrite(t, filepath.Join(town, "mayor", "town.json"), `{"name":"temp","dolt":{"port":`+townPort+`}}`)
	var envs, argvs [][]string
	c := container{
		image:    "bd-image:test",
		bin:      "bd",
		doltHost: testDoltHost,
		base:     []string{"PATH=/usr/bin", "BEADS_DOLT_SERVER_PORT=" + townPort},
		docker: func(env []string, argv []string) ([]byte, int, error) {
			envs = append(envs, env)
			argvs = append(argvs, argv)
			return []byte(forkContainerVersion), 0, nil
		},
	}
	child := containerChild(c)
	scratch := filepath.Join(town, "scratch")
	err := guardRun(cwdAt(town), scratch, child)
	if err == nil || !strings.Contains(err.Error(), "port "+townPort) {
		t.Fatalf("guardRun in a town that serves Dolt on %s = %v, want its port refused", townPort, err)
	}
	if _, ok := envValue(child.Env(scratch), testServerOptIn[0]); ok {
		t.Errorf("the environment the town-server refusal reads names %s, want the pin added only after the refusal", testServerOptIn[0])
	}
	if _, statErr := os.Stat(scratch); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("scratch %s stat = %v, want no directory made for a refused server", scratch, statErr)
	}
	if len(envs) == 0 {
		t.Fatalf("docker ran no call, want the build proof's version call before the refusal")
	}
	for i, argv := range argvs {
		if slices.Contains(argv, "init") {
			t.Errorf("docker ran the database-making call %v, want the town's server refused before it", argv)
		}
		if slices.Contains(argv, "version") {
			if got, ok := envValue(envs[i], testServerOptIn[0]); !ok || got != testServerOptIn[1] {
				t.Errorf("the version call carries %s=%q, found=%v, want the recorder's %s", testServerOptIn[0], got, ok, testServerOptIn[1])
			}
		}
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
		runErr    error
		runExit   int
		want      string // empty: the proof passes
	}{
		{"one build, the host's branch label", forkHostVersion, forkContainerVersion, nil, nil, 0, ""},
		{"one build, no branch label on either", forkContainerVersion, forkContainerVersion, nil, nil, 0, ""},
		{"another commit", forkHostVersion, "bd version 1.2.2 (688ff7d2: 1111111111) schema<=66\n", nil, nil, 0, "the container's bd is"},
		{"another release", "bd version 1.2.3 (688ff7d2: master@688ff7d249e9) schema<=66\n", forkContainerVersion, nil, nil, 0, "the container's bd is"},
		{"another build id", "bd version 1.2.2 (aaaaaaa: master@688ff7d249e9) schema<=66\n", forkContainerVersion, nil, nil, 0, "the container's bd is"},
		{"another schema level", "bd version 1.2.2 (688ff7d2: master@688ff7d249e9) schema<=67\n", forkContainerVersion, nil, nil, 0, "the container's bd is"},
		{"a line the recorder cannot read", forkHostVersion, "bd: no version here\n", nil, nil, 0, "the container's bd"},
		{"a schema level that is not a number", forkHostVersion, "bd version 1.2.2 (688ff7d2: 688ff7d249e9) schema<=99999999999999999999\n", nil, nil, 0, "schema level"},
		{"a host line the recorder cannot read", "bd: no version here\n", forkContainerVersion, nil, nil, 0, "the host bd"},
		{"a host bd it cannot read", "", "", errors.New("bd version: not found"), nil, 0, "read the version of the host bd"},
		{"a container run that never started", forkHostVersion, "", nil, errors.New("exec: docker: not found"), 0, "run bd version in the container"},
		{"a container version that exited non-zero", forkHostVersion, forkContainerVersion, nil, nil, 2, "exited 2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := testChild(okRun, atWork)
			child.HostVersion = func() ([]byte, error) { return []byte(tc.host), tc.hostErr }
			child.Run = func(string, []string) ([]byte, int, error) {
				return []byte(tc.container), tc.runExit, tc.runErr
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

// TestHostResultKeepsAStdoutAndRefusesARunThatNeverFinished: the host bd's answer is the build the
// container's is compared with, so a run that timed out or never started is an error rather than an
// empty version (gt-p85pw.117).
func TestHostResultKeepsAStdoutAndRefusesARunThatNeverFinished(t *testing.T) {
	t.Parallel()
	line := []byte(forkHostVersion)
	tests := []struct {
		name     string
		runErr   error
		timedOut bool
		wantErr  string
	}{
		{"ran to exit 0", nil, false, ""},
		{"ran past the limit", nil, true, "ran past"},
		{"never started", errors.New("exec: bd: not found"), false, "the host bd:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := hostResult(line, tc.runErr, tc.timedOut)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("hostResult(%s) = %v, want an error naming %q", tc.name, err, tc.wantErr)
				}
				return
			}
			if err != nil || string(got) != string(line) {
				t.Errorf("hostResult(%s) = %q, %v, want bd's line", tc.name, got, err)
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
		exit  int
	}{
		// bd where exits 1 with no_beads_directory when it resolves no workspace (bd 1.2.2).
		{"no workspace yet", []byte(`{"data":{"error":"no_beads_directory","message":"No active beads workspace found."},"schema_version":1}`), 1},
		{"a workspace under the mount", []byte(`{"data":{"path":"/work/.beads"}}`), 0},
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
				docker:   dockerAnswer(&ran, tc.where, tc.exit),
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
// environment that pins it is decided by the workspace bd resolves, never by the pin. The resolver
// is handed the pin itself, since the container filter would drop it, and the answer still decides
// (gt-p85pw.117).
func TestGuardRunRefusesAPinnedWorkspaceOutsideWork(t *testing.T) {
	t.Parallel()
	const pinnedDir = "/work/.beads"
	pinned := []string{"BEADS_DIR=" + pinnedDir, "BEADS_DOLT_SERVER_PORT=" + testScratchPort}
	tests := []struct {
		name  string
		where string
		want  string // empty: the guard accepts
	}{
		{"bd resolves the town's workspace anyway", "/town/.beads", "outside /work"},
		{"bd resolves the pinned workspace", filepath.Join(workDir, ".beads"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen []string
			bd := &guardBD{}
			scratch := filepath.Join(tempDir(t), "scratch")
			child := childWithRawEnv(bd.Run, pinned, func(string, []string) (string, error) {
				return tc.where, nil
			})
			child.Where = func(_ string, env []string) (string, error) {
				seen = env
				return tc.where, nil
			}
			err := guardRun(safeCwd(t), scratch, child)
			if got, ok := envValue(seen, "BEADS_DIR"); !ok || got != pinnedDir {
				t.Fatalf("the resolver was given BEADS_DIR=%q, found=%v, want the pin %s", got, ok, pinnedDir)
			}
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

// nameOf returns the container name a docker run argv gives its --name flag.
func nameOf(argv []string) string {
	for i, arg := range argv {
		if arg == "--name" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// TestContainerCallRemovesATimedOutContainer: a docker client killed at runLimit leaves the container
// it started running against the scratch, so a call that times out removes that container by the
// name it gave it, exactly once. A removal that fails is joined to the timeout, so a container that
// may still be writing is never reported as cleaned up (gt-p85pw.117).
func TestContainerCallRemovesATimedOutContainer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		runErr  error
		rmExit  int
		rmErr   error
		wantRm  bool
		wantErr string // empty: the call returns no error
	}{
		{"a run that finishes", nil, 0, nil, false, ""},
		{"a run that times out and is removed", errDockerTimedOut, 0, nil, true, "ran past"},
		{"a run that times out and the removal exits non-zero", errDockerTimedOut, 1, nil, true, "docker rm exited 1"},
		{"a run that times out and the removal never runs", errDockerTimedOut, 0, errors.New("exec: docker: not found"), true, "remove container"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			c := container{
				image:    "bd-image:test",
				bin:      "bd",
				doltHost: testDoltHost,
				docker: func(_ []string, argv []string) ([]byte, int, error) {
					calls = append(calls, argv)
					if argv[0] == "rm" {
						return nil, tc.rmExit, tc.rmErr
					}
					return nil, 0, tc.runErr
				},
			}
			_, _, err := c.call("/scratch/copy", nil, []string{"show", "fx-1"})
			if !tc.wantRm {
				if len(calls) != 1 {
					t.Fatalf("docker ran %d commands, want the run alone: %v", len(calls), calls)
				}
			} else {
				if len(calls) != 2 {
					t.Fatalf("docker ran %d commands, want the run then one removal: %v", len(calls), calls)
				}
				name := nameOf(calls[0])
				if !strings.HasPrefix(name, "bdshim-record-") {
					t.Fatalf("the run was given the name %q, want a bdshim-record- name", name)
				}
				if want := []string{"rm", "-f", name}; !slices.Equal(calls[1], want) {
					t.Errorf("the removal ran %v, want %v", calls[1], want)
				}
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("call = %v, want no error", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("call = %v, want an error naming %q", err, tc.wantErr)
			}
			if tc.runErr != nil && !errors.Is(err, errDockerTimedOut) {
				t.Errorf("call = %v, want the timeout kept in the error", err)
			}
		})
	}
}

// TestGuardRunRefusesAScratchDockerWouldSplit: a character docker reads as part of a -v mount, in the
// scratch's path, refuses the run before any bd call runs and before the scratch is made, since the
// directory the call mounts would not be the one the recorder named (gt-p85pw.117).
func TestGuardRunRefusesAScratchDockerWouldSplit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		char string
	}{
		{"a colon", ":"},
		{"a comma", ","},
		{"a line break", "\n"},
		{"a NUL", "\x00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &guardBD{}
			scratch := filepath.Join(tempDir(t), "a"+tc.char+"b")
			err := guardRun(safeCwd(t), scratch, testChild(bd.Run, atWork))
			if err == nil || !strings.Contains(err.Error(), "docker would read") {
				t.Fatalf("guardRun over a scratch with %s = %v, want it refused", tc.name, err)
			}
			if len(bd.args) != 0 {
				t.Errorf("bd ran %v, want nothing run for a refused scratch", bd.args)
			}
			if _, statErr := os.Stat(scratch); statErr == nil {
				t.Errorf("scratch %q was made, want nothing written for a refused scratch", scratch)
			}
		})
	}
}
