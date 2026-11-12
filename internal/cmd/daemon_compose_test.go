package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"

	"gopkg.in/yaml.v3"
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
	town   string
	flags  daemonComposeFlags
	deps   daemonComposeDeps
	out    *bytes.Buffer
	errOut *bytes.Buffer

	disk     map[string][]byte
	made     []string
	mkdirErr error
	writeErr error
	wrote    []string
	// privateWrote is each path the owner-only seam was given; the derived hm
	// config is the only file that goes through it.
	privateWrote []string
}

// hostHMConfig is the Mac's hm config the rig derives the container's from. Its
// database URL is loopback, as the live one's is.
const hostHMConfig = `{
  "database_url": "postgres://hm:test-password@localhost:5433/hm?sslmode=disable",
  "embed": {"provider": "ollama", "url": "http://localhost:11434"},
  "rerank": {"provider": "llamacpp", "url": "http://localhost:8012"}
}`

// newComposeRig is a town that names both container fields and reaches its Dolt
// through the host the container resolves.
func newComposeRig(t *testing.T) *composeRig {
	t.Helper()
	r := &composeRig{town: composeTestTown, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, disk: map[string][]byte{}}
	r.flags = daemonComposeFlags{image: daemonimage.ImageTag(composeTestRev)}
	r.deps = daemonComposeDeps{
		findTown: func() (string, error) { return r.town, nil },
		daemonConfig: func(string) (*config.DaemonPatrolConfig, error) {
			return &config.DaemonPatrolConfig{
				ContainerNameSetting:  composeTestContainer,
				ComposeProjectSetting: composeTestProject,
			}, nil
		},
		doltHost:     func(string) (string, bool) { return composeTestDoltHost, true },
		containerEnv: func(string) (map[string]string, error) { return map[string]string{}, nil },
		readFile:     func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		evalSymlinks: func(p string) (string, error) { return p, nil },
		isDir:        func(string) (bool, error) { return true, nil },
		townZone:     func(string) (string, error) { return "", nil },
		omConfig: func() ([]byte, error) {
			return templates.RenderOMConfig(templates.DefaultOMConfigSettings())
		},
		hmConfig: func(string) ([]byte, error) { return []byte(hostHMConfig), nil },
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
			writePrivate: func(path string, content []byte) error {
				r.privateWrote = append(r.privateWrote, path)
				if r.writeErr != nil {
					return r.writeErr
				}
				r.disk[path] = content
				return nil
			},
			// No file is at the compose path unless a test puts one there, and
			// then a test replaces this seam with one that reads it.
			read: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		},
		out:    r.out,
		errOut: r.errOut,
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
		HMConfigFile:  containerHMConfigPath(r.town),
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
	// The derived hm config is written first, then the compose file, each after
	// its own directory is made.
	for _, want := range []string{filepath.Dir(containerHMConfigPath(r.town)), filepath.Dir(r.composePath())} {
		if !slices.Contains(r.made, want) {
			t.Errorf("made directories %v, want %s", r.made, want)
		}
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

// --om-token-file puts the host's token file in the render: a read-only mount at
// the one fixed path, and that path as the entrypoint's flag. The command never
// reads the file, so its contents are in neither the compose file nor a line of
// output; a relative path is refused and writes nothing (gt-yaqct).
func TestDaemonComposeWriteOMToken(t *testing.T) {
	t.Parallel()
	secret := "a-marker-that-is-not-a-token"
	tokenFile := filepath.Join(t.TempDir(), "om-token")
	if err := os.WriteFile(tokenFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}

	r := newComposeRig(t)
	r.flags.omTokenFile = tokenFile
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	got, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	for _, want := range []string{
		fmt.Sprintf("      - type: bind\n        source: %q\n        target: %q\n        read_only: true\n",
			tokenFile, templates.LiveOMTokenPath),
		fmt.Sprintf("    command: [%q, %q]", "--"+config.OMTokenFileFlag, templates.LiveOMTokenPath),
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("compose file does not carry %q:\n%s", want, got)
		}
	}
	if bytes.Contains(got, []byte(secret)) {
		t.Errorf("the compose file carries the token file's contents:\n%s", got)
	}
	if strings.Contains(r.out.String(), secret) {
		t.Errorf("output carries the token file's contents: %q", r.out.String())
	}

	refused := newComposeRig(t)
	refused.flags.omTokenFile = "secrets/om-token"
	err := runDaemonCompose(refused.flags, refused.deps)
	if err == nil || !strings.Contains(err.Error(), "is not absolute") {
		t.Fatalf("error = %v, want a relative path refused as not absolute", err)
	}
	if len(refused.wrote) != 0 {
		t.Errorf("a relative token path wrote %v", refused.wrote)
	}
}

// --om-token-file also writes the om config the container reads, beside the
// derived kubeconfig, and mounts it read-only at the XDG path om reads. The
// config names the token's container path, never the host's (gt-gg9hv.13).
func TestComposeWriteWithOMTokenWritesTheDerivedOMConfig(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "om-token")
	if err := os.WriteFile(tokenFile, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}

	r := newComposeRig(t)
	r.flags.omTokenFile = tokenFile
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}

	omPath := containerOMConfigPath(r.town)
	content, ok := r.disk[omPath]
	if !ok {
		t.Fatalf("no om config written at %s; wrote %v", omPath, r.wrote)
	}
	if !strings.Contains(string(content), `"`+templates.LiveOMTokenPath+`"`) {
		t.Errorf("om config = %s, want the backend to name the token at %s", content, templates.LiveOMTokenPath)
	}
	if strings.Contains(string(content), tokenFile) {
		t.Errorf("om config names the host's token path %s:\n%s", tokenFile, content)
	}
	mount := fmt.Sprintf("      - type: bind\n        source: %q\n        target: %q\n        read_only: true\n",
		omPath, templates.LiveOMConfigPath)
	if got := string(r.disk[r.composePath()]); !strings.Contains(got, mount) {
		t.Errorf("compose file does not carry %q:\n%s", mount, got)
	}
}

// Without --om-token-file the command writes no om config and mounts none: the
// compose file is the only file written, as it was before the flag had one.
func TestComposeWriteWithoutOMTokenWritesNoOMConfig(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	if want := []string{r.composePath()}; !slices.Equal(r.wrote, want) {
		t.Errorf("wrote %v, want only the compose file %v", r.wrote, want)
	}
	if got := string(r.disk[r.composePath()]); strings.Contains(got, templates.LiveOMConfigPath) {
		t.Errorf("compose file mounts an om config without a token file:\n%s", got)
	}
}

// A derivation that fails stops the command before any file is written, and
// names the step, so the town keeps the compose file it had (gt-gg9hv.13).
func TestComposeWriteRefusesWhenTheOMConfigCannotBeDerived(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "om-token")
	if err := os.WriteFile(tokenFile, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}

	r := newComposeRig(t)
	r.flags.omTokenFile = tokenFile
	r.deps.omConfig = func() ([]byte, error) { return nil, errors.New("bad settings") }
	err := runDaemonCompose(r.flags, r.deps)
	if err == nil || !strings.Contains(err.Error(), "deriving the om config") {
		t.Fatalf("error = %v, want the derivation named", err)
	}
	if len(r.wrote) != 0 {
		t.Errorf("a failed derivation wrote %v", r.wrote)
	}
}

// A failed om config write stops the command before the compose file is
// written, so the old compose file stays as it was.
func TestComposeWriteStopsWhenTheOMConfigCannotBeWritten(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "om-token")
	if err := os.WriteFile(tokenFile, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}

	r := newComposeRig(t)
	r.flags.omTokenFile = tokenFile
	r.writeErr = errors.New("disk full")
	err := runDaemonCompose(r.flags, r.deps)
	if err == nil || !strings.Contains(err.Error(), "cannot write the om config") {
		t.Fatalf("error = %v, want the om config write named", err)
	}
	if want := []string{containerOMConfigPath(r.town)}; !slices.Equal(r.wrote, want) {
		t.Errorf("wrote %v, want the om config alone before the compose file was refused", r.wrote)
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
		{
			name: "a token file flag given blank",
			mutate: func(r *composeRig) {
				r.flags.omTokenFile, r.flags.omTokenFileSet = "  ", true
			},
			want: "was given blank",
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
	r.deps.files.writePrivate = func(string, []byte) error {
		t.Error("--print wrote the hm config")
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
	// daemon/ is a regular file, so .runtime/ cannot be made for the hm config
	// the write derives before the compose file.
	r.mkdirErr = errors.New("not a directory")
	err := runDaemonCompose(r.flags, r.deps)
	if want := containerHMConfigPath(r.town); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to name %s", err, want)
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
	r.deps.files.writePrivate = func(string, []byte) error { return nil }
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

// The command is registered as `gt daemon compose write`, parses --image,
// --om-token-file and --print, and runs nothing without a subcommand.
func TestDaemonComposeCommandIsWired(t *testing.T) {
	t.Parallel()
	var got daemonComposeFlags
	var ran bool
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		got, ran = f, true
		return nil
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--print",
		"--" + config.OMTokenFileFlag, "/ops/om/token"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := (daemonComposeFlags{image: daemonimage.ImageTag(composeTestRev), omTokenFile: "/ops/om/token", omTokenFileSet: true, print: true}); !ran || !reflect.DeepEqual(got, want) {
		t.Errorf("ran %v flags %+v, want write with the image, the token file and --print (%+v)", ran, got, want)
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

// A blank --om-token-file is refused when the command parses it, so the
// Changed wiring is what marks the flag as given. Driving the flag, not the
// flags struct, is what proves that wiring is live.
func TestDaemonComposeBlankTokenFlagIsRefusedThroughFlag(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "  "} {
		t.Run(fmt.Sprintf("value %q", value), func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			old := []byte("old render")
			r.disk[r.composePath()] = old
			c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
				return runDaemonCompose(f, r.deps)
			})
			c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--" + config.OMTokenFileFlag, value})
			c.SilenceUsage, c.SilenceErrors = true, true
			err := c.Execute()
			if err == nil || !strings.Contains(err.Error(), "was given blank") {
				t.Fatalf("error = %v, want it to contain %q", err, "was given blank")
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
			if got := r.disk[r.composePath()]; !bytes.Equal(got, old) {
				t.Errorf("compose file = %q, want the old render %q kept", got, old)
			}
		})
	}
}

// --config-dir reaches the spec through the real command and the render writes
// the town's config directory at its host path and at the container's XDG path,
// with XDG_CONFIG_HOME naming the parent (gt-gg9hv.3).
func TestComposeWriteConfigDirFlagReachesSpec(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	configDir := "/host/home/.config/gt"
	var got daemonComposeFlags
	c := newDaemonComposeCmd(func(out io.Writer, f daemonComposeFlags) error {
		got = f
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--config-dir", configDir})
	c.SilenceUsage, c.SilenceErrors = true, true

	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.configDir != configDir {
		t.Errorf("the flag reached the spec as %q, want %q", got.configDir, configDir)
	}
	file, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	// Read the file as compose does: the assertion turns on the mounts and the
	// environment, not on indentation or quoting.
	var composed struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Volumes     []struct {
				Type     string `yaml:"type"`
				Source   string `yaml:"source"`
				Target   string `yaml:"target"`
				ReadOnly bool   `yaml:"read_only"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(file, &composed); err != nil {
		t.Fatalf("the compose file does not parse as YAML: %v\n%s", err, file)
	}
	service, ok := composed.Services["daemon"]
	if !ok {
		t.Fatalf("the compose file has no daemon service:\n%s", file)
	}
	mounted := map[string]bool{}
	for _, mount := range service.Volumes {
		if mount.Type == "bind" && mount.Source == configDir {
			mounted[mount.Target] = mount.ReadOnly
		}
	}
	for _, target := range []string{configDir, templates.LiveConfigDirPath} {
		readOnly, ok := mounted[target]
		if !ok {
			t.Errorf("no bind mount of %s at %s; volumes = %+v", configDir, target, service.Volumes)
			continue
		}
		if !readOnly {
			t.Errorf("the bind mount of %s at %s is not read-only", configDir, target)
		}
	}
	if got := service.Environment["XDG_CONFIG_HOME"]; got != templates.LiveOpenShellConfigHome {
		t.Errorf("XDG_CONFIG_HOME = %q, want %q", got, templates.LiveOpenShellConfigHome)
	}
}

// --extra-host repeats: each flag adds one alias, the flags reach the spec in
// the order they were given, and both entries reach the daemon service's
// extra_hosts. The render sorts them, so the file shows sorted lines while the
// flags keep their order here (gt-gg9hv.2).
func TestComposeWriteExtraHostFlagRepeats(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	var got daemonComposeFlags
	c := newDaemonComposeCmd(func(out io.Writer, f daemonComposeFlags) error {
		got = f
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{
		"write", "--image", daemonimage.ImageTag(composeTestRev),
		"--extra-host", "git.example:host-gateway",
		"--extra-host", "forgejo:10.0.0.2",
	})
	c.SilenceUsage, c.SilenceErrors = true, true

	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := []string{"git.example:host-gateway", "forgejo:10.0.0.2"}; !slices.Equal(got.extraHosts, want) {
		t.Errorf("flags reached the spec as %q, want them in the order given: %q", got.extraHosts, want)
	}

	file, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	if want := "    extra_hosts:\n      - \"forgejo:10.0.0.2\"\n      - \"git.example:host-gateway\"\n"; !strings.Contains(string(file), want) {
		t.Errorf("compose file does not carry %q:\n%s", want, file)
	}
}

// A malformed --extra-host is refused by the render, so the command writes no
// file and keeps the one the town had (gt-jzeg6).
func TestComposeWriteExtraHostMalformedIsRefused(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"git.example", "forgejo:"} {
		t.Run(fmt.Sprintf("entry %q", entry), func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			old := []byte("old render")
			r.disk[r.composePath()] = old
			c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
				return runDaemonCompose(f, r.deps)
			})
			c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--extra-host", entry})
			c.SilenceUsage, c.SilenceErrors = true, true

			err := c.Execute()
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", entry)) {
				t.Fatalf("error = %v, want a refusal naming the malformed entry %q", err, entry)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
			if got := r.disk[r.composePath()]; !bytes.Equal(got, old) {
				t.Errorf("compose file = %q, want the old render %q kept", got, old)
			}
		})
	}
}

// --backup-dir reaches the spec through the real command, and the render mounts
// the directory read-write at its own path and names it in GT_DOLT_BACKUP_ROOT
// (gt-gg9hv.17).
func TestComposeWriteBackupDirFlagReachesSpec(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	backupDir := "/host/home/gt-backups/dolt"
	var got daemonComposeFlags
	c := newDaemonComposeCmd(func(out io.Writer, f daemonComposeFlags) error {
		got = f
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--backup-dir", backupDir})
	c.SilenceUsage, c.SilenceErrors = true, true

	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.backupDir != backupDir {
		t.Errorf("the flag reached the spec as %q, want %q", got.backupDir, backupDir)
	}
	file, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	for _, want := range []string{
		fmt.Sprintf("source: %q\n        target: %q\n    networks:\n", backupDir, backupDir),
		fmt.Sprintf("GT_DOLT_BACKUP_ROOT: %q\n", backupDir),
	} {
		if !strings.Contains(string(file), want) {
			t.Errorf("compose file does not carry %q:\n%s", want, file)
		}
	}
}

// A relative --backup-dir is refused by the render, so the command writes no
// file and the old one is kept.
func TestComposeWriteBackupDirRelativeIsRefused(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	old := []byte("old render")
	r.disk[r.composePath()] = old
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--backup-dir", "gt-backups/dolt"})
	c.SilenceUsage, c.SilenceErrors = true, true

	err := c.Execute()
	if err == nil || !strings.Contains(err.Error(), `"gt-backups/dolt"`) {
		t.Fatalf("error = %v, want a refusal naming the relative backup dir", err)
	}
	if len(r.wrote) != 0 {
		t.Errorf("a refusal wrote %v", r.wrote)
	}
	if got := r.disk[r.composePath()]; !bytes.Equal(got, old) {
		t.Errorf("compose file = %q, want the old render %q kept", got, old)
	}
}

// --claude-beads-dir reaches the spec through the real command, and the render
// mounts the directory read-write at its own path, with no read_only key, so the
// wisp_reaper can write the claude database from the container (gt-he92oj).
func TestDaemonComposeClaudeBeadsFlag(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	dir := "/host/home/.claude/.beads"
	var got daemonComposeFlags
	c := newDaemonComposeCmd(func(out io.Writer, f daemonComposeFlags) error {
		got = f
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--claude-beads-dir", dir})
	c.SilenceUsage, c.SilenceErrors = true, true

	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.claudeBeadsDir != dir {
		t.Errorf("the flag reached the spec as %q, want %q", got.claudeBeadsDir, dir)
	}
	file, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	target := fmt.Sprintf("source: %q\n        target: %q\n", dir, dir)
	i := strings.Index(string(file), target)
	if i < 0 {
		t.Fatalf("compose file does not carry the mount %q:\n%s", target, file)
	}
	if strings.HasPrefix(string(file)[i+len(target):], "        read_only") {
		t.Errorf("the claude beads mount carries read_only, want it read-write:\n%s", file)
	}
}

// A --claude-beads-dir that is blank, relative, or not a directory is refused by
// the command, so no file is written and the old render is kept (gt-he92oj).
func TestDaemonComposeClaudeBeadsFlagRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, value string
		notDir      bool
		unreadable  bool
		want        string
	}{
		{name: "blank", value: " ", want: "was given blank"},
		{name: "relative path", value: "claude/.beads", want: "not absolute"},
		{name: "not a directory", value: "/host/home/.claude/.beads", notDir: true, want: "is not a directory"},
		{name: "cannot be stat'd", value: "/host/home/.claude/.beads", unreadable: true, want: "cannot be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.deps.isDir = func(string) (bool, error) {
				if tc.unreadable {
					return false, fs.ErrNotExist
				}
				return !tc.notDir, nil
			}
			old := []byte("old render")
			r.disk[r.composePath()] = old
			c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
				return runDaemonCompose(f, r.deps)
			})
			c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--claude-beads-dir", tc.value})
			c.SilenceUsage, c.SilenceErrors = true, true

			if err := c.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a refusal that says %q", err, tc.want)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
			if got := r.disk[r.composePath()]; !bytes.Equal(got, old) {
				t.Errorf("compose file = %q, want the old render %q kept", got, old)
			}
		})
	}
}

// --bind-ro reaches the spec through the real command: each directory is mounted
// read-only at its own path, and the mounts come out sorted by path whatever
// order the flags were given in (gt-vqexg).
func TestComposeWriteBindRO(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	const reports, releases = "/Users/Shared/gt-cloud/reports", "/Users/Shared/gt-cloud/releases"
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev),
		"--bind-ro", reports, "--bind-ro", releases})
	c.SilenceUsage, c.SilenceErrors = true, true

	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	file, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	mount := func(dir string) string {
		return fmt.Sprintf("source: %q\n        target: %q\n        read_only: true\n", dir, dir)
	}
	relAt, repAt := strings.Index(string(file), mount(releases)), strings.Index(string(file), mount(reports))
	if relAt < 0 || repAt < 0 {
		t.Fatalf("compose file does not carry both read-only mounts:\n%s", file)
	}
	if relAt > repAt {
		t.Errorf("the read-only mounts are not sorted by path:\n%s", file)
	}
}

// Each --bind-ro the command or the render cannot mount read-only is refused
// with the path named, and the command writes no file and keeps the old one.
func TestComposeWriteBindRORefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		value      string
		duplicate  bool
		notDir     bool
		unreadable bool
		want       string
	}{
		{name: "relative path", value: "gt-cloud/reports", want: "not an absolute path"},
		{name: "dot-dot path", value: "/fake/../etc/reports", want: "under .."},
		{name: "inside the town", value: composeTestTown + "/reports", want: "inside or contains the town root"},
		{name: "containing the town", value: "/fake", want: "inside or contains the town root"},
		{name: "given twice", value: "/host/reports", duplicate: true, want: "more than once"},
		{name: "not a directory", value: "/host/reports.txt", notDir: true, want: "is not a directory"},
		{name: "cannot be stat'd", value: "/host/missing", unreadable: true, want: "cannot be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.deps.isDir = func(string) (bool, error) {
				switch {
				case tc.unreadable:
					return false, fs.ErrNotExist
				case tc.notDir:
					return false, nil
				}
				return true, nil
			}
			old := []byte("old render")
			r.disk[r.composePath()] = old
			c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
				return runDaemonCompose(f, r.deps)
			})
			args := []string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--bind-ro", tc.value}
			if tc.duplicate {
				args = append(args, "--bind-ro", tc.value)
			}
			c.SetArgs(args)
			c.SilenceUsage, c.SilenceErrors = true, true

			err := c.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error = %v, want a refusal that says %q and names %q", err, tc.want, tc.value)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
			if got := r.disk[r.composePath()]; !bytes.Equal(got, old) {
				t.Errorf("compose file = %q, want the old render %q kept", got, old)
			}
		})
	}
}

// A blank --bind-ro names no directory, so the command refuses it rather than
// render as if the flag were unset.
func TestComposeWriteBindROBlankIsRefused(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		return runDaemonCompose(f, r.deps)
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--bind-ro", "  "})
	c.SilenceUsage, c.SilenceErrors = true, true

	err := c.Execute()
	if err == nil || !strings.Contains(err.Error(), "blank") {
		t.Fatalf("error = %v, want a refusal of the blank --bind-ro", err)
	}
	if len(r.wrote) != 0 {
		t.Errorf("a refusal wrote %v", r.wrote)
	}
}

// The --bind-ro paragraph of the write help wraps within 80 columns: no line of
// it runs past 80.
func TestComposeWriteBindROHelpIsWrapped(t *testing.T) {
	t.Parallel()
	write, _, err := newDaemonComposeCmd(func(io.Writer, daemonComposeFlags) error { return nil }).Find([]string{"write"})
	if err != nil {
		t.Fatalf("find write: %v", err)
	}
	lines := strings.Split(write.Long, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "--bind-ro names") })
	if start < 0 {
		t.Fatal("the write help has no --bind-ro paragraph")
	}
	for _, l := range lines[start:] {
		if l == "" {
			break
		}
		if len(l) > 80 {
			t.Errorf("help line is %d columns, want at most 80: %q", len(l), l)
		}
	}
}

// The --claude-beads-dir behaviour lives in one place in the write help: the
// flag usage states the mount and its refusals, and the Long paragraph points at
// it while keeping the reaper setting the mount serves (R2; gt-ulsf3k). A second
// copy of the refusals in the paragraph would drift from the one the flag
// enforces.
func TestComposeWriteClaudeBeadsDirHelpStatesTheChecksOnce(t *testing.T) {
	t.Parallel()
	write, _, err := newDaemonComposeCmd(func(io.Writer, daemonComposeFlags) error { return nil }).Find([]string{"write"})
	if err != nil {
		t.Fatalf("find write: %v", err)
	}
	flag := write.Flags().Lookup("claude-beads-dir")
	if flag == nil {
		t.Fatal("the write command has no --claude-beads-dir flag")
	}
	if !strings.Contains(flag.Usage, "is refused") {
		t.Errorf("the flag usage does not state what it refuses: %q", flag.Usage)
	}
	lines := strings.Split(write.Long, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "--claude-beads-dir") })
	if start < 0 {
		t.Fatal("the write help has no --claude-beads-dir paragraph")
	}
	var para []string
	for _, l := range lines[start:] {
		if l == "" {
			break
		}
		para = append(para, l)
	}
	text := strings.Join(para, "\n")
	if strings.Contains(text, "is refused") {
		t.Errorf("the paragraph states the flag's refusals a second time; the flag usage owns them:\n%s", text)
	}
	if !strings.Contains(text, "extra_beads_roots") {
		t.Errorf("the paragraph no longer names the reaper setting the mount serves:\n%s", text)
	}
}

// The host's isDir is the stat the --bind-ro check reads: a directory is true, a
// file is false, and a path that is not there is an error the command refuses
// (gt-vqexg). Every path sits under t.TempDir(), so nothing outside it is read.
func TestRealDaemonComposeIsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "reports.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the fixture file: %v", err)
	}
	isDir := realDaemonComposeDeps(io.Discard).isDir
	cases := []struct {
		name    string
		path    string
		want    bool
		wantErr bool
	}{
		{name: "a directory", path: dir, want: true},
		{name: "a file", path: file, want: false},
		{name: "a missing path", path: filepath.Join(dir, "missing"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := isDir(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("isDir(%q) error = %v, want an error %v", tc.path, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("isDir(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// The write derives the container's hm config from the Mac's, writes it under
// the town through the owner-only seam, and the render mounts it where HM_CONFIG
// names it. The database password is in neither the compose file nor the output.
func TestDaemonComposeWriteDerivesTheHMConfig(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	path := filepath.Join(r.town, ".runtime", "container", "hm-config.json")
	if !slices.Equal(r.privateWrote, []string{path}) {
		t.Fatalf("owner-only writes %v, want the hm config at %s", r.privateWrote, path)
	}
	var derived struct {
		DatabaseURL string `json:"database_url"`
	}
	if err := json.Unmarshal(r.disk[path], &derived); err != nil {
		t.Fatalf("the derived hm config does not parse: %v", err)
	}
	if want := fmt.Sprintf("postgres://hm:test-password@%s:5433/hm?sslmode=disable", composeTestDoltHost); derived.DatabaseURL != want {
		t.Errorf("database_url = %q, want the loopback host rewritten: %q", derived.DatabaseURL, want)
	}
	// The embedder and the reranker are read from the blocks hm reads, so the
	// container reaches them at the host, not at its own loopback.
	var derivedBlocks struct {
		Embed struct {
			URL string `json:"url"`
		} `json:"embed"`
		Rerank struct {
			URL string `json:"url"`
		} `json:"rerank"`
	}
	if err := json.Unmarshal(r.disk[path], &derivedBlocks); err != nil {
		t.Fatalf("the derived hm config's service blocks do not parse: %v", err)
	}
	if want := "http://host.docker.internal:11434"; derivedBlocks.Embed.URL != want {
		t.Errorf("embed.url = %q, want the loopback host rewritten: %q", derivedBlocks.Embed.URL, want)
	}
	if want := "http://host.docker.internal:8012"; derivedBlocks.Rerank.URL != want {
		t.Errorf("rerank.url = %q, want the loopback host rewritten: %q", derivedBlocks.Rerank.URL, want)
	}
	compose := string(r.disk[r.composePath()])
	for _, want := range []string{
		`source: "` + path + `"`,
		`target: "` + templates.LiveHMConfigPath + `"`,
		"read_only: true",
		`HM_CONFIG: "` + templates.LiveHMConfigPath + `"`,
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("the compose file lacks %q:\n%s", want, compose)
		}
	}
	for _, secret := range []string{"test-password", "localhost:5433"} {
		if strings.Contains(compose, secret) {
			t.Errorf("the compose file carries %q:\n%s", secret, compose)
		}
		for _, out := range []*bytes.Buffer{r.out, r.errOut} {
			if strings.Contains(out.String(), secret) {
				t.Errorf("the output carries %q: %q", secret, out.String())
			}
		}
	}
}

// An hm config the write cannot produce refuses before the compose file, and the
// message never repeats the database URL, whose password it holds.
func TestDaemonComposeWriteRefusesAnUnusableHMConfig(t *testing.T) {
	t.Parallel()
	boom := errors.New("no such file")
	cases := []struct {
		name   string
		config func(string) ([]byte, error)
		want   string
		secret string
	}{
		{
			name:   "unreadable",
			config: func(string) ([]byte, error) { return nil, boom },
			want:   "reading the Mac's hm config",
		},
		{
			name:   "unparsable",
			config: func(string) ([]byte, error) { return []byte(`{"database_url":`), nil },
			want:   "deriving the hm config",
		},
		{
			name:   "no database_url",
			config: func(string) ([]byte, error) { return []byte(`{"embed_url": "http://host.docker.internal/x"}`), nil },
			want:   "database_url is missing",
		},
		{
			name:   "database_url is not a string",
			config: func(string) ([]byte, error) { return []byte(`{"database_url": 5433}`), nil },
			want:   "database_url is not a string",
		},
		{
			name: "database_url (URL form) does not parse",
			config: func(string) ([]byte, error) {
				return []byte(`{"database_url": "postgres://hm:s3 cret@localhost:5433/hm"}`), nil
			},
			want:   "database_url (URL form) does not parse",
			secret: "s3 cret",
		},
		{
			// A key=value string with a loopback host is rewritten (the render
			// case below); one this cannot parse is refused, naming the file it
			// came from and the form, never copied and never the password.
			name: "a key=value database_url that does not parse names the file and the form",
			config: func(string) ([]byte, error) {
				return []byte(`{"database_url": "host=localhost password='s3cret"}`), nil
			},
			want:   "hm-config.json",
			secret: "s3cret",
		},
		{
			name: "a key=value database_url that cannot be rewritten names the form",
			config: func(string) ([]byte, error) {
				return []byte(`{"database_url": "host=/var/run/postgresql password=s3cret"}`), nil
			},
			want:   "(key=value form) names a unix-socket host",
			secret: "s3cret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.deps.hmConfig = tc.config
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Errorf("the error repeats the database password: %v", err)
			}
			if len(r.wrote) != 0 || len(r.privateWrote) != 0 {
				t.Errorf("a refused hm config wrote %v and %v", r.wrote, r.privateWrote)
			}
		})
	}
}

// A failed owner-only write is the command's error, names the file, and leaves
// the compose file unwritten.
func TestDaemonComposeWriteReportsAnHMConfigWriteFailure(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	failure := errors.New("disk full")
	r.deps.files.writePrivate = func(string, []byte) error { return failure }
	err := runDaemonCompose(r.flags, r.deps)
	path := containerHMConfigPath(r.town)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to wrap %v and name %s", err, failure, path)
	}
	if len(r.wrote) != 0 {
		t.Errorf("a failed hm config write still wrote %v", r.wrote)
	}
}

// The real deps' hm reader follows the host's path rule: a config directory it
// cannot turn into an absolute path is refused before any file is opened.
func TestRealDaemonComposeHMConfigRefusesARelativeConfigDir(t *testing.T) {
	t.Parallel()
	if _, err := realDaemonComposeDeps(io.Discard).hmConfig("config/gt"); err == nil {
		t.Fatal("realDaemonComposeDeps().hmConfig accepted a relative config directory")
	}
}

// The Mac's hm config is found in the directory --config-dir names, else the
// XDG config directory, else the home's config directory; with none of them
// there is no path to read.
func TestHostHMConfigPath(t *testing.T) {
	t.Parallel()
	boom := errors.New("no home")
	cases := []struct {
		name      string
		configDir string
		xdg       string
		home      func() (string, error)
		want      string
		wantErr   bool
	}{
		{name: "the config directory the flag names", configDir: "/host/.config/gt", xdg: "/xdg",
			home: func() (string, error) { return "/home/operator", nil }, want: "/host/.config/gt/hm-config.json"},
		{name: "the XDG directory", xdg: "/host/xdg",
			home: func() (string, error) { return "/home/operator", nil }, want: "/host/xdg/gt/hm-config.json"},
		{name: "the home directory", home: func() (string, error) { return "/home/operator", nil },
			want: "/home/operator/.config/gt/hm-config.json"},
		{name: "no home", home: func() (string, error) { return "", boom }, wantErr: true},
		{name: "a relative config directory", configDir: "config/gt",
			home: func() (string, error) { return "/home/operator", nil }, wantErr: true},
		{name: "a relative XDG directory", xdg: "xdg",
			home: func() (string, error) { return "/home/operator", nil }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(name string) string {
				if name == "XDG_CONFIG_HOME" {
					return tc.xdg
				}
				return ""
			}
			got, err := hostHMConfigPath(tc.configDir, getenv, tc.home)
			if (err != nil) != tc.wantErr {
				t.Fatalf("hostHMConfigPath error = %v, want an error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("hostHMConfigPath = %q, want %q", got, tc.want)
			}
		})
	}
}
