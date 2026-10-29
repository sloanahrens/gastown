package templates_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/templates"
)

// composeFile is the part of the rendered file these tests read. A key the
// renderer does not write is nil here, and the acceptance boxes that name a key
// the file must not contain read the raw text instead.
type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name"`
	Restart       string            `yaml:"restart"`
	StopGrace     string            `yaml:"stop_grace_period"`
	WorkingDir    string            `yaml:"working_dir"`
	Environment   map[string]string `yaml:"environment"`
	Volumes       []composeVolume   `yaml:"volumes"`
}

type composeVolume struct {
	Type   string `yaml:"type"`
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// liveSpec is the live town's daemon, the fixture the golden file was rendered
// from. The name refusals below are what keep a scratch town from rendering
// these strings instead of its own.
func liveSpec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project:       "gt-town",
		ContainerName: "gt-town-daemon-1",
		Image:         "gt-daemon:4f2a1c9",
		TownRoot:      "/towns/live",
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      "host.docker.internal",
	}
}

// render renders s and parses the result, so a file that is not valid YAML
// fails the test that asked for it rather than the one that reads a key.
func render(t *testing.T, s templates.ComposeSpec) (string, composeFile) {
	t.Helper()
	out, err := templates.RenderDaemonCompose(s)
	if err != nil {
		t.Fatalf("RenderDaemonCompose(%+v) = %v", s, err)
	}
	var f composeFile
	if err := yaml.Unmarshal(out, &f); err != nil {
		t.Fatalf("rendered file is not valid YAML: %v\n%s", err, out)
	}
	return string(out), f
}

// The rendered file is the whole contract with the deployer, so it is compared
// byte for byte: a stray line here is a compose key nobody reviewed.
func TestRenderDaemonCompose_Golden(t *testing.T) {
	t.Parallel()
	out, _ := render(t, liveSpec())

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("rendered file differs from testdata/daemon-compose.golden.yml\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

func TestRenderDaemonCompose_ServiceShape(t *testing.T) {
	t.Parallel()
	_, f := render(t, liveSpec())

	svc, ok := f.Services["daemon"]
	if !ok {
		t.Fatalf("services = %v, want a daemon service", f.Services)
	}
	if svc.Restart != "unless-stopped" {
		t.Errorf("restart = %q, want unless-stopped: a stopped container stays stopped after `docker compose stop`", svc.Restart)
	}
	if svc.StopGrace != templates.MinimumStopGrace.String() {
		t.Errorf("stop_grace_period = %q, want %s (MinimumStopGrace)", svc.StopGrace, templates.MinimumStopGrace)
	}
	if got := len(f.Services); got != 1 {
		t.Errorf("services = %d, want the daemon alone (sweep-dind is C2's)", got)
	}
}

// A published port and a Docker socket are the two ways this container could
// reach the host, and the design rules both out: the daemon reaches an engine
// only through the sweep's engine on the compose network.
func TestRenderDaemonCompose_NoSocketNoPorts(t *testing.T) {
	t.Parallel()
	out, _ := render(t, liveSpec())

	if strings.Contains(out, "docker.sock") {
		t.Errorf("rendered file mounts a Docker socket:\n%s", out)
	}
	if strings.Contains(out, "ports:") {
		t.Errorf("rendered file publishes a port:\n%s", out)
	}
}

func TestRenderDaemonCompose_BindMountsTheTownRootAtItsOwnPath(t *testing.T) {
	t.Parallel()
	spec := liveSpec()
	_, f := render(t, spec)

	vols := f.Services["daemon"].Volumes
	if len(vols) != 1 {
		t.Fatalf("volumes = %+v, want the town root alone", vols)
	}
	if vols[0].Type != "bind" {
		t.Errorf("volume type = %q, want bind", vols[0].Type)
	}
	if vols[0].Source != spec.TownRoot || vols[0].Target != spec.TownRoot {
		t.Errorf("volume = %s:%s, want %s at the same absolute path", vols[0].Source, vols[0].Target, spec.TownRoot)
	}
	if f.Services["daemon"].WorkingDir != spec.TownRoot {
		t.Errorf("working_dir = %q, want the town root %s", f.Services["daemon"].WorkingDir, spec.TownRoot)
	}
}

func TestRenderDaemonCompose_ContainerNameIsTheOwnerID(t *testing.T) {
	t.Parallel()
	spec := liveSpec()
	_, f := render(t, spec)
	svc := f.Services["daemon"]

	if svc.ContainerName != spec.ContainerName {
		t.Errorf("container_name = %q, want %q", svc.ContainerName, spec.ContainerName)
	}
	if got := svc.Environment["GT_DAEMON_CONTAINER_NAME"]; got != spec.ContainerName {
		t.Errorf("GT_DAEMON_CONTAINER_NAME = %q, want the container name %q, the owner id the daemon records", got, spec.ContainerName)
	}
	if got := svc.Environment[config.ContainerModeEnv]; got != config.SupervisorContainer {
		t.Errorf("%s = %q, want %q: the daemon writes no pid file in container mode", config.ContainerModeEnv, got, config.SupervisorContainer)
	}
	if got, ok := svc.Environment["GT_DAEMON_TAKEOVER"]; !ok || got != "${GT_DAEMON_TAKEOVER:-}" {
		t.Errorf("GT_DAEMON_TAKEOVER = %q (present %t), want the pass-through ${GT_DAEMON_TAKEOVER:-}, empty unless an operator sets it for one start", got, ok)
	}
	if got := svc.Environment["DOCKER_HOST"]; got != "tcp://sweep-dind:2375" {
		t.Errorf("DOCKER_HOST = %q, want tcp://sweep-dind:2375", got)
	}
	if got := svc.Environment["GT_TOWN_ROOT"]; got != spec.TownRoot {
		t.Errorf("GT_TOWN_ROOT = %q, want the town root %s", got, spec.TownRoot)
	}
}

// A render that fell back to a default name would be a scratch town writing the
// live daemon's container name (gt-ix1kz), so every empty or unusable field is
// a refusal, never a guess.
func TestRenderDaemonCompose_RefusesASpecItCannotRenderSafely(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*templates.ComposeSpec)
		want string
	}{
		{"no project", func(s *templates.ComposeSpec) { s.Project = "" }, "Project"},
		{"no container name", func(s *templates.ComposeSpec) { s.ContainerName = "" }, "ContainerName"},
		{"no image", func(s *templates.ComposeSpec) { s.Image = "" }, "Image"},
		{"no town root", func(s *templates.ComposeSpec) { s.TownRoot = "" }, "TownRoot"},
		{"no stop grace", func(s *templates.ComposeSpec) { s.StopGrace = 0 }, "MinimumStopGrace"},
		{"stop grace below the budget", func(s *templates.ComposeSpec) { s.StopGrace = templates.MinimumStopGrace - time.Second }, "MinimumStopGrace"},
		{"loopback dolt host", func(s *templates.ComposeSpec) { s.DoltHost = "127.0.0.1" }, "loopback"},
		{"localhost dolt host", func(s *templates.ComposeSpec) { s.DoltHost = "localhost" }, "loopback"},
		{"ipv6 loopback dolt host", func(s *templates.ComposeSpec) { s.DoltHost = "::1" }, "loopback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := liveSpec()
			tc.edit(&spec)

			out, err := templates.RenderDaemonCompose(spec)
			if err == nil {
				t.Fatalf("RenderDaemonCompose rendered a file for %s:\n%s", tc.name, out)
			}
			if out != nil {
				t.Errorf("RenderDaemonCompose returned %q with its error, want no file", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestRenderDaemonCompose_AcceptsANonLoopbackDoltHost(t *testing.T) {
	t.Parallel()
	// The probe (gt-qpeei.4) answered the path to the Mac's Dolt at the VM's
	// gateway address, so a literal non-loopback address is a host the
	// container can use and the renderer passes it through untouched.
	spec := liveSpec()
	spec.DoltHost = "192.168.65.254"
	if _, err := templates.RenderDaemonCompose(spec); err != nil {
		t.Errorf("RenderDaemonCompose with a gateway address = %v, want no error", err)
	}
}

// The spec's strings reach the file unquoted only if they cannot change its
// shape, so one with a space and a colon still parses as the same five keys,
// and the mount keeps its whole path on both sides.
func TestRenderDaemonCompose_QuotesAValueThatWouldChangeTheYAML(t *testing.T) {
	t.Parallel()
	spec := liveSpec()
	spec.TownRoot = "/towns/live town: one"
	_, f := render(t, spec)

	vols := f.Services["daemon"].Volumes
	if len(vols) != 1 || vols[0].Source != spec.TownRoot || vols[0].Target != spec.TownRoot {
		t.Fatalf("volumes = %+v, want %s mounted at its own path", vols, spec.TownRoot)
	}
	if got := f.Services["daemon"].WorkingDir; got != spec.TownRoot {
		t.Errorf("working_dir = %q, want %q", got, spec.TownRoot)
	}
}

// pinnedImages are the references images.go declares, as a service may name
// them by tag. A task that adds a service adds its constant here, so an image
// string hard-coded in the renderer fails this test.
var pinnedImages = []string{
	templates.DaemonBaseImage,
}

// daemonImagePrefix is the local tag the deployer builds on this host; it is
// allowed by name because nothing pulls it.
const daemonImagePrefix = "gt-daemon:"

func TestRenderDaemonCompose_EveryImageIsPinned(t *testing.T) {
	t.Parallel()
	spec := liveSpec()
	_, f := render(t, spec)
	if len(f.Services) == 0 {
		t.Fatal("rendered file has no services")
	}

	for name, svc := range f.Services {
		switch {
		case svc.Image == "":
			t.Errorf("service %q renders no image", name)
		case strings.HasPrefix(svc.Image, daemonImagePrefix) && svc.Image != daemonImagePrefix:
			// The daemon's own tag, built from this host's binaries.
		case slices.Contains(pinnedImages, svc.Image):
			// A reference images.go carries.
		default:
			t.Errorf("service %q image %q: neither the local %s<rev> tag nor a reference in images.go", name, svc.Image, daemonImagePrefix)
		}
		if strings.HasSuffix(svc.Image, ":latest") {
			t.Errorf("service %q image %q: a moving tag makes a restart run an image the ladder never proved", name, svc.Image)
		}
		if !strings.Contains(svc.Image, ":") && !strings.Contains(svc.Image, "@") {
			t.Errorf("service %q image %q names no version", name, svc.Image)
		}
	}
}

var doltVersionShape = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func TestImages_NameAVersion(t *testing.T) {
	t.Parallel()
	if strings.HasSuffix(templates.DaemonBaseImage, ":latest") || !strings.Contains(templates.DaemonBaseImage, ":") {
		t.Errorf("DaemonBaseImage = %q, want a tagged base image", templates.DaemonBaseImage)
	}
	if !doltVersionShape.MatchString(templates.DoltVersion) {
		t.Errorf("DoltVersion = %q, want X.Y.Z", templates.DoltVersion)
	}
	if deps.CompareVersions(templates.DoltVersion, deps.MinDoltVersion) < 0 {
		t.Errorf("DoltVersion = %q is below the binary's own MinDoltVersion %q", templates.DoltVersion, deps.MinDoltVersion)
	}
}

// MinimumStopGrace duplicates daemon.ShutdownBudget because internal/daemon
// imports this package, so the renderer cannot read it here. This is the pin
// that keeps the copy honest.
func TestMinimumStopGrace_CoversShutdownBudget(t *testing.T) {
	t.Parallel()
	if templates.MinimumStopGrace < daemon.ShutdownBudget {
		t.Fatalf("MinimumStopGrace = %v, want at least daemon.ShutdownBudget %v", templates.MinimumStopGrace, daemon.ShutdownBudget)
	}
}
