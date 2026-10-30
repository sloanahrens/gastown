package templates_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates"
)

// scratchSpec is the scratch project's daemon. Its image and DoltHost are the
// two fields the renderer refuses when empty or loopback, so the golden built
// from it is a render a real scratch start could apply.
func scratchSpec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project:          "gt-scratch",
		ContainerName:    "gt-scratch-daemon-1",
		Image:            "gt-daemon:4f2a1c9",
		TownRoot:         "/towns/scratch",
		StopGrace:        templates.MinimumStopGrace,
		DoltHost:         "host.docker.internal",
		Scratch:          true,
		ScratchDoltImage: templates.DoltServerImage,
		LiveTownRoot:     liveSpec().TownRoot,
	}
}

func TestRenderDaemonCompose_ScratchGolden(t *testing.T) {
	t.Parallel()
	out, _ := render(t, scratchSpec())

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose-scratch.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("rendered file differs from testdata/daemon-compose-scratch.golden.yml\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

// Each row is one of the three live-identity checks, plus the two fields a
// scratch render cannot do without. Removing any check turns its row red.
func TestRenderDaemonCompose_ScratchRefusesTheLiveIdentity(t *testing.T) {
	t.Parallel()
	live := liveSpec()
	tests := []struct {
		name   string
		mutate func(*templates.ComposeSpec)
	}{
		{"live town root", func(s *templates.ComposeSpec) { s.TownRoot = live.TownRoot }},
		{"live town root with a trailing slash", func(s *templates.ComposeSpec) { s.TownRoot = live.TownRoot + "/" }},
		// macOS volumes are case-insensitive by default, so the same directory
		// can be spelled in another case.
		{"live town root in other letter case", func(s *templates.ComposeSpec) { s.TownRoot = strings.ToUpper(live.TownRoot) }},
		{"live container name", func(s *templates.ComposeSpec) { s.ContainerName = config.DefaultContainerName }},
		{"live compose project", func(s *templates.ComposeSpec) { s.Project = config.DefaultComposeProject }},
		{"no live town root to check against", func(s *templates.ComposeSpec) { s.LiveTownRoot = "" }},
		{"no scratch Dolt image", func(s *templates.ComposeSpec) { s.ScratchDoltImage = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := scratchSpec()
			tt.mutate(&spec)
			if _, err := templates.RenderDaemonCompose(spec); err == nil {
				t.Errorf("RenderDaemonCompose(%+v) = nil error, want a refusal", spec)
			}
		})
	}
}

// liveIdentifiers are the names the live render declares or joins, which a
// scratch render must not carry anywhere in its text: its container name, its
// project, its town root and every volume and network it names.
func liveIdentifiers(t *testing.T) []string {
	t.Helper()
	live := liveSpec()
	_, f := render(t, live)

	names := []string{live.ContainerName, live.Project, live.TownRoot}
	for v := range f.Volumes {
		names = append(names, v)
	}
	for n := range f.Networks {
		names = append(names, n)
	}
	for _, svc := range f.Services {
		names = append(names, svc.Networks...)
	}
	return names
}

func TestRenderDaemonCompose_ScratchSharesNoNameWithTheLiveRender(t *testing.T) {
	t.Parallel()
	names := liveIdentifiers(t)
	// The list is only worth checking if it read the live file's names, which
	// the engine's volume proves it did.
	if !slices.Contains(names, "sweep_dind_data") {
		t.Fatalf("live identifiers = %q, want the engine's volume among them", names)
	}

	out, _ := render(t, scratchSpec())
	for _, name := range names {
		if strings.Contains(out, name) {
			t.Errorf("scratch render contains %q, a name the live render carries:\n%s", name, out)
		}
	}
}

// The scratch daemon takes the --scratch-town flag the container entrypoint
// reads. The live daemon passes nothing, so a live start never asks for an
// install. The operator note about a failed install is in the scratch file.
func TestRenderDaemonCompose_ScratchPassesTheScratchTownFlagAndTheLiveOneDoesNot(t *testing.T) {
	t.Parallel()
	scratchOut, scratch := render(t, scratchSpec())
	if got := scratch.Services["daemon"].Command; !slices.Equal(got, []string{"--scratch-town"}) {
		t.Errorf("scratch daemon command = %q, want [--scratch-town]", got)
	}
	if !strings.Contains(scratchOut, ".gt-install-incomplete") {
		t.Errorf("scratch render does not tell the operator how to clear a failed install (.gt-install-incomplete)")
	}

	liveOut, live := render(t, liveSpec())
	if got := live.Services["daemon"].Command; len(got) != 0 {
		t.Errorf("live daemon command = %q, want none", got)
	}
	if strings.Contains(liveOut, "--scratch-town") {
		t.Errorf("live render asks for a scratch install:\n%s", liveOut)
	}
}

func TestRenderDaemonCompose_ScratchDeclaresItsOwnVolumes(t *testing.T) {
	t.Parallel()
	_, f := render(t, scratchSpec())

	want := []string{"scratch_dind_data", "scratch_dolt_data"}
	if len(f.Volumes) != len(want) {
		t.Errorf("volumes = %v, want exactly %q", f.Volumes, want)
	}
	for _, v := range want {
		if _, ok := f.Volumes[v]; !ok {
			t.Errorf("volumes = %v, want %q declared", f.Volumes, v)
		}
	}
}

// The scratch Dolt server is reachable on the internal network alone and
// publishes no port: the daemon reaches it there, and the Mac cannot reach it
// at all.
func TestRenderDaemonCompose_ScratchDoltPublishesNoPortAndJoinsTheInternalNetworkAlone(t *testing.T) {
	t.Parallel()
	out, f := render(t, scratchSpec())

	dolt, ok := f.Services["scratch-dolt"]
	if !ok {
		t.Fatalf("services = %v, want a scratch-dolt service", f.Services)
	}
	if len(dolt.Ports) != 0 || strings.Contains(out, "ports:") {
		t.Errorf("scratch-dolt publishes a port: ports = %q", dolt.Ports)
	}
	if len(dolt.Networks) != 1 {
		t.Fatalf("scratch-dolt networks = %q, want the internal network alone", dolt.Networks)
	}
	net, ok := f.Networks[dolt.Networks[0]]
	if !ok || !net.Internal {
		t.Errorf("scratch-dolt network %q is not declared internal: true", dolt.Networks[0])
	}
	if !slices.Contains(f.Services["daemon"].Networks, dolt.Networks[0]) {
		t.Errorf("daemon networks = %q, want the scratch-dolt network %q so it can reach the Dolt server", f.Services["daemon"].Networks, dolt.Networks[0])
	}
}

// The scratch Dolt image is the one the daemon image's client version names, so
// the town's server and its client cannot drift apart.
func TestRenderDaemonCompose_ScratchDoltImageTagIsDoltVersion(t *testing.T) {
	t.Parallel()
	_, f := render(t, scratchSpec())

	image := f.Services["scratch-dolt"].Image
	repo, tag, found := strings.Cut(image, ":")
	if !found || repo != "dolthub/dolt-sql-server" {
		t.Fatalf("scratch-dolt image = %q, want dolthub/dolt-sql-server:<tag>", image)
	}
	if tag != templates.DoltVersion {
		t.Errorf("scratch-dolt image tag = %q, want DoltVersion %q", tag, templates.DoltVersion)
	}
	if image != templates.DoltServerImage {
		t.Errorf("scratch-dolt image = %q, want DoltServerImage %q", image, templates.DoltServerImage)
	}
}
