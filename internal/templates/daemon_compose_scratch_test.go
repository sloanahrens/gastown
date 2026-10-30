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

// scratchSpec is the scratch project's daemon. Its image is one of the fields
// the renderer refuses when empty, so the golden built from it is a render a
// real scratch start could apply. It names no DoltHost: the scratch daemon's
// Dolt host comes from the scratch town's own settings.
func scratchSpec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project:          "gt-scratch",
		ContainerName:    "gt-scratch-daemon-1",
		Image:            "gt-daemon:4f2a1c9",
		TownRoot:         "/towns/scratch",
		StopGrace:        templates.MinimumStopGrace,
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

// Each row is one of the live-identity checks or a field a scratch render cannot
// do without. Removing any check turns its row red.
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
		{"directory under the live town root", func(s *templates.ComposeSpec) { s.TownRoot = live.TownRoot + "/scratch" }},
		{"live container name", func(s *templates.ComposeSpec) { s.ContainerName = config.DefaultContainerName }},
		{"live compose project", func(s *templates.ComposeSpec) { s.Project = config.DefaultComposeProject }},
		{"no live town root to check against", func(s *templates.ComposeSpec) { s.LiveTownRoot = "" }},
		{"live town root not absolute", func(s *templates.ComposeSpec) { s.LiveTownRoot = "live" }},
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

// A scratch root reached through a symlink to the live root, or under one, names
// the live town under another path, so it is refused too. The directory under
// the link does not exist yet, which is the case a scratch install starts from.
// A dangling link is refused as well: it exists, and its target may be the live
// root, so it cannot be taken as a plain name. An ancestor of the live root is
// refused, because mounting it exposes the live town. Removing the resolved-path
// comparison turns the link rows red; resolving a dangling link by its name
// turns the dangling rows red; dropping the ancestor check turns that row red.
func TestRenderDaemonCompose_ScratchRefusesTheLiveRootThroughALink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	live := filepath.Join(base, "live")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(live, link); err != nil {
		t.Fatal(err)
	}
	danglingIntoLive := filepath.Join(base, "dangling-live")
	if err := os.Symlink(filepath.Join(live, "missing"), danglingIntoLive); err != nil {
		t.Fatal(err)
	}
	danglingElsewhere := filepath.Join(base, "dangling-elsewhere")
	if err := os.Symlink(filepath.Join(base, "nowhere"), danglingElsewhere); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		liveRoot string
		scratch  string
	}{
		{"a symlink to the live root", live, link},
		{"a directory under a symlink to the live root", live, filepath.Join(link, "scratch")},
		{"the live root reached through a symlink", link, live},
		{"a directory under the live root", live, filepath.Join(live, "scratch")},
		{"a dangling symlink into the live root", live, danglingIntoLive},
		{"a directory under a dangling symlink into the live root", live, filepath.Join(danglingIntoLive, "scratch")},
		{"a dangling symlink elsewhere", live, danglingElsewhere},
		{"an ancestor of the live root", live, base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := scratchSpec()
			spec.LiveTownRoot = tt.liveRoot
			spec.TownRoot = tt.scratch
			if _, err := templates.RenderDaemonCompose(spec); err == nil {
				t.Errorf("scratch TownRoot %q with live root %q rendered, want a refusal", tt.scratch, tt.liveRoot)
			}
		})
	}
}

// A root that cannot be resolved, because a path component is a file, is refused
// rather than taken as clear, whichever root it is: the check cannot tell where
// it would mount.
func TestRenderDaemonCompose_ScratchRefusesARootThatCannotBeResolved(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		liveRoot string
		scratch  string
	}{
		{"scratch root under a file", filepath.Join(base, "live"), filepath.Join(file, "scratch")},
		{"live root under a file", filepath.Join(file, "live"), filepath.Join(base, "scratch")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := scratchSpec()
			spec.LiveTownRoot = tt.liveRoot
			spec.TownRoot = tt.scratch
			if _, err := templates.RenderDaemonCompose(spec); err == nil {
				t.Errorf("scratch TownRoot %q with live root %q rendered, want a refusal", tt.scratch, tt.liveRoot)
			}
		})
	}
}

// The controls for the refusals above: a scratch root beside the live root, and
// one reached through a symlink to a directory elsewhere, are not refused.
func TestRenderDaemonCompose_ScratchAcceptsARootBesideTheLiveRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	live := filepath.Join(base, "live")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		scratch string
	}{
		{"a root beside the live root", filepath.Join(base, "scratch")},
		{"a root under a symlink to elsewhere", filepath.Join(link, "scratch")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := scratchSpec()
			spec.LiveTownRoot = live
			spec.TownRoot = tt.scratch
			if _, err := templates.RenderDaemonCompose(spec); err != nil {
				t.Errorf("scratch TownRoot %q with live root %q = %v, want a render", tt.scratch, live, err)
			}
		})
	}
}

// liveIdentifiers are the names the live render declares or joins, which a
// scratch render must not carry anywhere in its text: its container name, its
// project, its town root, its engine, its socket and Dolt host, and every
// volume and network it names.
func liveIdentifiers(t *testing.T) []string {
	t.Helper()
	live := liveSpec()
	_, f := render(t, live)

	names := []string{live.ContainerName, live.Project, live.TownRoot, live.DoltHost, engineName}
	names = append(names, f.Services["daemon"].Environment["DOCKER_HOST"])
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

// The scratch daemon's command carries the entrypoint's flags, each built from
// the one constant the entrypoint registers, so the two cannot drift apart. It
// names the scratch project's own Dolt service, so the install writes that
// server's endpoint instead of starting one inside the daemon container. The
// live daemon passes nothing, so a live start never asks for an install.
func TestRenderDaemonCompose_ScratchPassesTheEntrypointFlagsAndTheLiveOneDoesNot(t *testing.T) {
	t.Parallel()
	_, scratch := render(t, scratchSpec())
	if got, want := scratch.Services["daemon"].Command,
		[]string{"--" + config.ScratchTownFlag, "--" + config.DoltHostFlag, templates.ScratchDoltService}; !slices.Equal(got, want) {
		t.Errorf("scratch daemon command = %q, want %q", got, want)
	}

	liveOut, live := render(t, liveSpec())
	if got := live.Services["daemon"].Command; len(got) != 0 {
		t.Errorf("live daemon command = %q, want none", got)
	}
	for _, flag := range []string{config.ScratchTownFlag, config.DoltHostFlag} {
		if strings.Contains(liveOut, flag) {
			t.Errorf("live render carries %q, which only the scratch project passes:\n%s", flag, liveOut)
		}
	}
}

// The forge golden is a second scratch spec, and its daemon command carries the
// Dolt host too: the forge adds services and changes nothing about the install.
func TestRenderDaemonCompose_ScratchForgePassesTheDoltHost(t *testing.T) {
	t.Parallel()
	_, f := render(t, forgeSpec())
	if got, want := f.Services["daemon"].Command,
		[]string{"--" + config.ScratchTownFlag, "--" + config.DoltHostFlag, templates.ScratchDoltService}; !slices.Equal(got, want) {
		t.Errorf("scratch forge daemon command = %q, want %q", got, want)
	}
}

// The operator note about a failed install is written once, in Go, and rendered
// as YAML comments. Each line must appear exactly once as a whole line, so a
// second copy or a reworded one fails here.
func TestRenderDaemonCompose_ScratchOperatorNoteAppearsOnceAsWholeLines(t *testing.T) {
	t.Parallel()
	out, _ := render(t, scratchSpec())
	for _, line := range []string{
		"    # After a failed scratch install the entrypoint refuses every start until",
		"    # the operator removes .gt-install-incomplete and what the install left,",
		"    # from the host side of the scratch directory.",
	} {
		if n := strings.Count(out, "\n"+line+"\n"); n != 1 {
			t.Errorf("operator note line %q appears %d times in the scratch render, want once:\n%s", line, n, out)
		}
	}
}

// The scratch daemon starts only once the scratch Dolt server accepts a
// connection, the same wait the engine's health gives it (gt-3bp5q). The live
// render has no scratch service and no such wait.
func TestRenderDaemonCompose_ScratchDaemonWaitsForItsDoltServer(t *testing.T) {
	t.Parallel()
	_, f := render(t, scratchSpec())

	dep, ok := f.Services["daemon"].DependsOn[templates.ScratchDoltService]
	if !ok || dep.Condition != "service_healthy" {
		t.Errorf("daemon depends_on = %v, want %s with condition service_healthy", f.Services["daemon"].DependsOn, templates.ScratchDoltService)
	}
	if hc := f.Services[templates.ScratchDoltService].Healthcheck; hc == nil || len(hc.Test) == 0 {
		t.Errorf("%s has no healthcheck, so service_healthy never holds the daemon", templates.ScratchDoltService)
	}

	_, live := render(t, liveSpec())
	if _, ok := live.Services["daemon"].DependsOn[templates.ScratchDoltService]; ok {
		t.Errorf("live daemon depends_on the scratch Dolt server")
	}
	if _, ok := live.Services[templates.ScratchDoltService]; ok {
		t.Errorf("live render declares the scratch Dolt server")
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
	repo, tagAndDigest, found := strings.Cut(image, ":")
	if !found || repo != "dolthub/dolt-sql-server" {
		t.Fatalf("scratch-dolt image = %q, want dolthub/dolt-sql-server:<tag>@sha256:<digest>", image)
	}
	tag, _, _ := strings.Cut(tagAndDigest, "@")
	if tag != templates.DoltVersion {
		t.Errorf("scratch-dolt image tag = %q, want DoltVersion %q", tag, templates.DoltVersion)
	}
	if image != templates.DoltServerImage {
		t.Errorf("scratch-dolt image = %q, want DoltServerImage %q", image, templates.DoltServerImage)
	}
}

// The scratch render passes the om token file to the entrypoint beside
// --scratch-town, in a golden of its own; without it the command is the flag
// alone (TestRenderDaemonCompose_ScratchGolden).
func TestRenderDaemonCompose_ScratchPassesTheOMTokenFile(t *testing.T) {
	t.Parallel()
	spec := scratchSpec()
	spec.OMTokenFile = "/run/secrets/om-token"
	out, parsed := render(t, spec)

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose-scratch-omtoken.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("rendered file differs from testdata/daemon-compose-scratch-omtoken.golden.yml\ngot:\n%s\nwant:\n%s", out, golden)
	}
	want := []string{
		"--" + config.ScratchTownFlag,
		"--" + config.DoltHostFlag, templates.ScratchDoltService,
		"--" + config.OMTokenFileFlag, "/run/secrets/om-token",
	}
	if got := parsed.Services["daemon"].Command; !slices.Equal(got, want) {
		t.Errorf("scratch daemon command = %q, want %q", got, want)
	}
}

// A dollar sign in the path reaches the container as itself: compose interpolates
// every value it reads, so the render doubles it.
func TestRenderDaemonCompose_ScratchEscapesADollarInTheTokenPath(t *testing.T) {
	t.Parallel()
	spec := scratchSpec()
	spec.OMTokenFile = "/run/$secrets/om-token"
	out, _ := render(t, spec)
	if !strings.Contains(out, `"/run/$$secrets/om-token"`) {
		t.Errorf("the token path is not escaped for compose:\n%s", out)
	}
}

// The field is the scratch render's alone: a live spec that sets it is refused,
// as are a relative path and a control character, and the live render carries
// none of it.
func TestRenderDaemonCompose_OMTokenFileIsRefusedWhereItDoesNotBelong(t *testing.T) {
	t.Parallel()
	live := liveSpec()
	live.OMTokenFile = "/run/secrets/om-token"
	relative := scratchSpec()
	relative.OMTokenFile = "secrets/om-token"
	control := scratchSpec()
	control.OMTokenFile = "/run/secrets/om\ntoken"
	for name, tc := range map[string]struct {
		spec templates.ComposeSpec
		want string
	}{
		"a live spec":     {live, "OMTokenFile is set on a live spec"},
		"a relative path": {relative, "is not absolute"},
		"a control char":  {control, "control character"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := templates.RenderDaemonCompose(tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenderDaemonCompose = %q, %v; want an error naming %q", out, err, tc.want)
			}
		})
	}
	liveOut, _ := render(t, liveSpec())
	if strings.Contains(liveOut, config.OMTokenFileFlag) {
		t.Errorf("the live render carries --%s:\n%s", config.OMTokenFileFlag, liveOut)
	}
}
