package cmd

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
)

const (
	composeTestRev       = "0123456"
	composeTestContainer = "gt-compose-daemon-1"
	composeTestProject   = "gt-compose"
	composeTestDoltHost  = "host.docker.internal"
	composeTestTown      = "/fake/town"
)

// composeRig is a town root and the deps the command reads it through. The
// filesystem is in memory: disk holds each file the write seam was given, made
// holds each directory the mkdir seam was asked for, so a test reads what landed
// and a refusal is proven to have written nothing. There is no command seam at
// all: the command has no way to start or run anything, which is what "starts
// nothing" means here.
type composeRig struct {
	town  string
	flags daemonComposeFlags
	deps  daemonComposeDeps
	out   *bytes.Buffer

	disk     map[string][]byte
	made     []string
	mkdirErr error
	writeErr error
	wrote    []string
}

// newComposeRig is a town that names both container fields and reaches its Dolt
// through the host the container resolves.
func newComposeRig(t *testing.T) *composeRig {
	t.Helper()
	r := &composeRig{town: composeTestTown, out: &bytes.Buffer{}, disk: map[string][]byte{}}
	r.flags = daemonComposeFlags{image: daemonimage.ImageTag(composeTestRev)}
	r.deps = daemonComposeDeps{
		findTown: func() (string, error) { return r.town, nil },
		daemonConfig: func(string) (*config.DaemonPatrolConfig, error) {
			return &config.DaemonPatrolConfig{
				ContainerNameSetting:  composeTestContainer,
				ComposeProjectSetting: composeTestProject,
			}, nil
		},
		doltHost: func(string) (string, bool) { return composeTestDoltHost, true },
		files: composeFiles{
			mkdirAll: func(dir string) error {
				if r.mkdirErr != nil {
					return r.mkdirErr
				}
				r.made = append(r.made, dir)
				return nil
			},
			write: func(path string, content []byte) error {
				r.wrote = append(r.wrote, path)
				if r.writeErr != nil {
					return r.writeErr
				}
				r.disk[path] = content
				return nil
			},
		},
		out: r.out,
	}
	return r
}

// composePath is where the town's live compose file lives.
func (r *composeRig) composePath() string { return config.ContainerComposeFile(r.town) }

// render is the file the rig's settings render to.
func (r *composeRig) render(t *testing.T) []byte {
	t.Helper()
	content, err := templates.RenderDaemonCompose(templates.ComposeSpec{
		Project:       composeTestProject,
		ContainerName: composeTestContainer,
		Image:         r.flags.image,
		TownRoot:      r.town,
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      composeTestDoltHost,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return content
}

// The command writes the render for the town's own names to the live compose
// path, making the directory it does not have, and prints where it went.
func TestDaemonComposeWriteWritesTheRender(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	if len(r.wrote) != 1 || r.wrote[0] != r.composePath() {
		t.Fatalf("wrote %v, want the live compose file %s once", r.wrote, r.composePath())
	}
	if want := filepath.Dir(r.composePath()); len(r.made) != 1 || r.made[0] != want {
		t.Errorf("made directories %v, want %s once", r.made, want)
	}
	got, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	if !bytes.Equal(got, r.render(t)) {
		t.Errorf("compose file =\n%s\nwant the render of the town's settings:\n%s", got, r.render(t))
	}
	for _, want := range []string{
		`container_name: "` + composeTestContainer + `"`,
		`image: "` + daemonimage.ImageTag(composeTestRev) + `"`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("compose file does not carry %q:\n%s", want, got)
		}
	}
	if out := r.out.String(); !strings.Contains(out, r.composePath()) || !strings.Contains(out, composeTestProject) {
		t.Errorf("output %q does not name the file %s and the project %s", out, r.composePath(), composeTestProject)
	}
}

// --image names the tag itself: space around it is not part of the tag, and the
// file carries the tag, never the flag's padding.
func TestDaemonComposeWriteTrimsTheImage(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	tag := daemonimage.ImageTag(composeTestRev)
	r.flags.image = "  " + tag + "\n"
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	got := r.disk[r.composePath()]
	if want := []byte(`image: "` + tag + `"`); !bytes.Contains(got, want) {
		t.Errorf("compose file does not carry %q:\n%s", want, got)
	}
}

// Every refusal leaves the compose file the town already had as it was, and
// writes nothing at all. Each row is red if its check is removed: the command
// would go on to write, and the write count and the file would both differ.
func TestDaemonComposeWriteRefusesAndLeavesTheFileAlone(t *testing.T) {
	t.Parallel()
	old := []byte("services: {}\n")
	cases := []struct {
		name   string
		mutate func(r *composeRig)
		want   string
	}{
		{
			name: "a town that sets neither container field",
			mutate: func(r *composeRig) {
				r.deps.daemonConfig = func(string) (*config.DaemonPatrolConfig, error) {
					return &config.DaemonPatrolConfig{}, nil
				}
			},
			want: "does not set both daemon.container_name and daemon.compose_project",
		},
		{
			name: "a town that sets only the container name",
			mutate: func(r *composeRig) {
				r.deps.daemonConfig = func(string) (*config.DaemonPatrolConfig, error) {
					return &config.DaemonPatrolConfig{ContainerNameSetting: composeTestContainer}, nil
				}
			},
			want: "does not set both daemon.container_name and daemon.compose_project",
		},
		{
			name: "a town that sets only the compose project",
			mutate: func(r *composeRig) {
				r.deps.daemonConfig = func(string) (*config.DaemonPatrolConfig, error) {
					return &config.DaemonPatrolConfig{ComposeProjectSetting: composeTestProject}, nil
				}
			},
			want: "set both or neither",
		},
		{
			name:   "a loopback Dolt host",
			mutate: func(r *composeRig) { r.deps.doltHost = func(string) (string, bool) { return "127.0.0.1", true } },
			want:   "is a loopback address",
		},
		{
			name:   "an empty Dolt host",
			mutate: func(r *composeRig) { r.deps.doltHost = func(string) (string, bool) { return "", true } },
			want:   "DoltHost is empty",
		},
		{
			name:   "a town with no Dolt endpoint",
			mutate: func(r *composeRig) { r.deps.doltHost = func(string) (string, bool) { return "", false } },
			want:   "Dolt endpoint cannot be resolved",
		},
		{
			name:   "an image that is not gt-daemon:<rev>",
			mutate: func(r *composeRig) { r.flags.image = "daemon:latest" },
			want:   "is not gt-daemon:<7 or more hex digits>",
		},
		{
			name:   "an image whose rev is not hex",
			mutate: func(r *composeRig) { r.flags.image = "gt-daemon:nothex1" },
			want:   "is not gt-daemon:<7 or more hex digits>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.disk[r.composePath()] = old
			tc.mutate(r)
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
			got, ok := r.disk[r.composePath()]
			if !ok || !bytes.Equal(got, old) {
				t.Errorf("compose file = %q, want the old render %q kept", got, old)
			}
		})
	}
}

// --print writes nothing and puts the render on stdout.
func TestDaemonComposeWritePrint(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	r.flags.print = true
	r.deps.files.write = func(string, []byte) error {
		t.Error("--print wrote a file")
		return nil
	}
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	if got, want := r.out.Bytes(), r.render(t); !bytes.Equal(got, want) {
		t.Errorf("--print output =\n%s\nwant the render:\n%s", got, want)
	}
	if _, ok := r.disk[r.composePath()]; ok {
		t.Errorf("--print left a file at %s", r.composePath())
	}
}

// The compose file's directory is made before the file is written, and a
// directory the town cannot grow is the write's error, naming the file.
func TestDaemonComposeWriteRefusesADirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	// daemon/ is a regular file, so daemon/container/ cannot be made.
	r.mkdirErr = errors.New("not a directory")
	err := runDaemonCompose(r.flags, r.deps)
	if err == nil || !strings.Contains(err.Error(), r.composePath()) {
		t.Fatalf("error = %v, want it to name %s", err, r.composePath())
	}
	if len(r.wrote) != 0 {
		t.Errorf("a directory it cannot make still wrote %v", r.wrote)
	}
}

// A failed write is the command's error, and it names the file and the state
// the old one is left in.
func TestDaemonComposeWriteReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	failure := errors.New("disk full")
	r.writeErr = failure
	err := runDaemonCompose(r.flags, r.deps)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), r.composePath()) {
		t.Errorf("error = %v, want it to wrap %v and name %s", err, failure, r.composePath())
	}
}

// The town and its settings are read through their seams, and a read that
// fails is the command's error.
func TestDaemonComposeWriteReportsReadFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *composeRig)
		want   string
	}{
		{
			name: "not in a town",
			mutate: func(r *composeRig) {
				r.deps.findTown = func() (string, error) { return "", errors.New("no workspace") }
			},
			want: "not in a Gas Town workspace",
		},
		{
			name: "unreadable daemon settings",
			mutate: func(r *composeRig) {
				r.deps.daemonConfig = func(string) (*config.DaemonPatrolConfig, error) { return nil, errors.New("cannot parse") }
			},
			want: "reading the town's daemon settings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			tc.mutate(r)
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refused read wrote %v", r.wrote)
			}
		})
	}
}

// The Dolt host is the town's endpoint resolved in container mode, whichever
// process asks: the resolver is asked for the town in container mode, and its
// host and whether it names an endpoint pass through.
func TestContainerDoltHostResolvesInContainerMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ep       config.DoltEndpoint
		ok       bool
		wantHost string
		wantOK   bool
	}{
		{
			name:     "a town with an endpoint",
			ep:       config.DoltEndpoint{Host: composeTestDoltHost, Port: 3307},
			ok:       true,
			wantHost: composeTestDoltHost,
			wantOK:   true,
		},
		{
			name: "a town with no endpoint",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotRoot string
			var gotContainer bool
			resolve := func(root string, inContainer bool) (config.DoltEndpoint, bool) {
				gotRoot, gotContainer = root, inContainer
				return tc.ep, tc.ok
			}
			host, ok := containerDoltHost(resolve)(composeTestTown)
			if host != tc.wantHost || ok != tc.wantOK {
				t.Errorf("host, ok = %q, %v; want %q, %v", host, ok, tc.wantHost, tc.wantOK)
			}
			if gotRoot != composeTestTown || !gotContainer {
				t.Errorf("resolved %q in container mode %v; want %q in container mode", gotRoot, gotContainer, composeTestTown)
			}
		})
	}
}

// The command is registered as `gt daemon compose write`, parses --image and
// --print, and runs nothing without a subcommand.
func TestDaemonComposeCommandIsWired(t *testing.T) {
	t.Parallel()
	var got daemonComposeFlags
	var ran bool
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		got, ran = f, true
		return nil
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--print"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !ran || got != (daemonComposeFlags{image: daemonimage.ImageTag(composeTestRev), print: true}) {
		t.Errorf("ran %v flags %+v, want write with the image and --print", ran, got)
	}

	bare := newDaemonComposeCmd(func(_ io.Writer, _ daemonComposeFlags) error { return nil })
	bare.SilenceUsage, bare.SilenceErrors = true, true
	if err := bare.Execute(); err == nil {
		t.Error("`gt daemon compose` with no subcommand ran")
	}

	if sub, _, err := daemonCmd.Find([]string{"compose"}); err != nil || sub.Name() != "compose" {
		t.Errorf("compose is not registered under gt daemon (%v)", err)
	}
}
