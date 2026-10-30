package templates_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// forgeSpec is the scratch project with its throwaway Forgejo, on host ports
// that are neither of the live Forgejo's.
func forgeSpec() templates.ComposeSpec {
	s := scratchSpec()
	s.ScratchForge = true
	s.ScratchForgeHTTPPort = 3100
	s.ScratchForgeSSHPort = 2322
	return s
}

// The live Forgejo compose project (~/forgejo/compose.yml) declares these
// names. They are copied here, not read from the operator's home, so the gate
// needs nothing outside the repository; the project's own file is the source and
// a name it gains belongs in this list.
var (
	liveForgeServices = []string{"db", "forgejo", "docker-in-docker", "runner-register", "runner-daemon"}
	liveForgeVolumes  = []string{"pgdata", "forgejo_data", "runner_data", "docker_certs", "dind_data"}
	liveForgeNetworks = []string{"default"}
	liveForgeProject  = "forgejo"
)

func TestRenderDaemonCompose_ScratchForgeGolden(t *testing.T) {
	t.Parallel()
	out, _ := render(t, forgeSpec())

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose-scratch-forge.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("rendered file differs from testdata/daemon-compose-scratch-forge.golden.yml\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

// Each row is a forge spec the renderer must refuse. Removing the port check a
// row names turns that row red.
func TestRenderDaemonCompose_ScratchForgeRefusesPortsItMustNotTake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*templates.ComposeSpec)
	}{
		{"the live Forgejo's http port as the http port", func(s *templates.ComposeSpec) { s.ScratchForgeHTTPPort = 3000 }},
		{"the live Forgejo's ssh port as the ssh port", func(s *templates.ComposeSpec) { s.ScratchForgeSSHPort = 2222 }},
		{"the live Forgejo's ssh port as the http port", func(s *templates.ComposeSpec) { s.ScratchForgeHTTPPort = 2222 }},
		{"the live Forgejo's http port as the ssh port", func(s *templates.ComposeSpec) { s.ScratchForgeSSHPort = 3000 }},
		{"no http port", func(s *templates.ComposeSpec) { s.ScratchForgeHTTPPort = 0 }},
		{"no ssh port", func(s *templates.ComposeSpec) { s.ScratchForgeSSHPort = 0 }},
		{"a negative port", func(s *templates.ComposeSpec) { s.ScratchForgeHTTPPort = -1 }},
		{"a port above 65535", func(s *templates.ComposeSpec) { s.ScratchForgeSSHPort = 65536 }},
		{"one port for both services", func(s *templates.ComposeSpec) { s.ScratchForgeSSHPort = s.ScratchForgeHTTPPort }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := forgeSpec()
			tt.mutate(&spec)
			if _, err := templates.RenderDaemonCompose(spec); err == nil {
				t.Errorf("RenderDaemonCompose(%+v) = nil error, want a refusal", spec)
			}
		})
	}
}

// Ports without the flag, and the flag on a live spec, are refused: a render
// that ignored them would let the caller think a forge was running.
func TestRenderDaemonCompose_ScratchForgeIsRefusedWhereItDoesNotBelong(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec func() templates.ComposeSpec
	}{
		{"ports set on a scratch spec without the flag", func() templates.ComposeSpec {
			s := forgeSpec()
			s.ScratchForge = false
			return s
		}},
		{"the flag on a live spec", func() templates.ComposeSpec {
			s := liveSpec()
			s.ScratchForge = true
			s.ScratchForgeHTTPPort = 3100
			s.ScratchForgeSSHPort = 2322
			return s
		}},
		{"ports on a live spec", func() templates.ComposeSpec {
			s := liveSpec()
			s.ScratchForgeHTTPPort = 3100
			return s
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := tt.spec()
			if _, err := templates.RenderDaemonCompose(spec); err == nil {
				t.Errorf("RenderDaemonCompose(%+v) = nil error, want a refusal", spec)
			}
		})
	}
}

// Ports the live forge does not use are accepted, so the refusals above are the
// check and not a blanket refusal.
func TestRenderDaemonCompose_ScratchForgeAcceptsPortsBesideTheLiveOnes(t *testing.T) {
	t.Parallel()
	for _, ports := range [][2]int{{3001, 2223}, {2223, 3001}, {1, 65535}} {
		spec := forgeSpec()
		spec.ScratchForgeHTTPPort, spec.ScratchForgeSSHPort = ports[0], ports[1]
		render(t, spec)
	}
}

// No service, volume or network is one the live Forgejo project declares, and the
// stack is not the live project's name either.
func TestRenderDaemonCompose_ScratchForgeSharesNoNameWithTheLiveForgejo(t *testing.T) {
	t.Parallel()
	spec := forgeSpec()
	_, f := render(t, spec)

	if len(f.Services) < 5 {
		t.Fatalf("services = %v, want the forge stack beside the daemon, the engine and the Dolt server", f.Services)
	}
	for _, name := range liveForgeServices {
		if _, ok := f.Services[name]; ok {
			t.Errorf("scratch render declares service %q, a name the live Forgejo project declares", name)
		}
	}
	for _, name := range liveForgeVolumes {
		if _, ok := f.Volumes[name]; ok {
			t.Errorf("scratch render declares volume %q, a name the live Forgejo project declares", name)
		}
	}
	for _, name := range liveForgeNetworks {
		if _, ok := f.Networks[name]; ok {
			t.Errorf("scratch render declares network %q, a name the live Forgejo project declares", name)
		}
	}
	for name, svc := range f.Services {
		for _, n := range svc.Networks {
			if slices.Contains(liveForgeNetworks, n) {
				t.Errorf("service %q joins network %q, one the live Forgejo project declares", name, n)
			}
		}
		for _, v := range svc.Volumes {
			if slices.Contains(liveForgeVolumes, v.Source) {
				t.Errorf("service %q mounts volume %q, one the live Forgejo project declares", name, v.Source)
			}
		}
		for _, p := range svc.Ports {
			if strings.Contains(p, ":3000:") || strings.Contains(p, ":2222:") {
				t.Errorf("service %q publishes %q, a host port the live Forgejo takes", name, p)
			}
		}
	}
	if spec.Project == liveForgeProject {
		t.Errorf("fixture project %q is the live Forgejo project; the test would prove nothing", spec.Project)
	}
}

// The stack's own services all appear, and each volume a service mounts is
// declared, so a compose file that names a volume it never declares fails here
// and not at the L2 window.
func TestRenderDaemonCompose_ScratchForgeServicesMountDeclaredVolumes(t *testing.T) {
	t.Parallel()
	_, f := render(t, forgeSpec())
	for _, name := range []string{"scratch-forgejo", "scratch-forgejo-db", "scratch-forge-dind", "scratch-forge-runner"} {
		svc, ok := f.Services[name]
		if !ok {
			t.Fatalf("services = %v, want %q", f.Services, name)
		}
		for _, v := range svc.Volumes {
			if _, ok := f.Volumes[v.Source]; !ok {
				t.Errorf("service %q mounts %q, which the file does not declare", name, v.Source)
			}
		}
		for _, n := range svc.Networks {
			if _, ok := f.Networks[n]; !ok && n != "scratch-egress" {
				t.Errorf("service %q joins %q, which the file does not declare", name, n)
			}
		}
	}
}

// The scratch Forgejo publishes exactly its two ports, on loopback and on the
// ports the spec names; nothing else in the stack publishes anything.
func TestRenderDaemonCompose_ScratchForgePublishesOnlyItsTwoPortsOnLoopback(t *testing.T) {
	t.Parallel()
	_, f := render(t, forgeSpec())
	want := []string{"127.0.0.1:3100:3000", "127.0.0.1:2322:22"}
	if got := f.Services["scratch-forgejo"].Ports; !slices.Equal(got, want) {
		t.Errorf("scratch-forgejo ports = %q, want %q", got, want)
	}
	for name, svc := range f.Services {
		if name != "scratch-forgejo" && len(svc.Ports) != 0 {
			t.Errorf("service %q publishes %q, want only scratch-forgejo to publish", name, svc.Ports)
		}
	}
}

// The runner and the stack name only the scratch Forgejo: every host the file
// pairs with the forge's port 3000 is the scratch service, never the live
// "forgejo:3000".
func TestRenderDaemonCompose_ScratchForgeRunnerNamesOnlyTheScratchForgejo(t *testing.T) {
	t.Parallel()
	out, f := render(t, forgeSpec())
	hosts := regexp.MustCompile(`([A-Za-z0-9._-]+):3000\b`).FindAllStringSubmatch(out, -1)
	if len(hosts) == 0 {
		t.Fatal("render names no forge URL, want the scratch Forgejo's")
	}
	for _, m := range hosts {
		// The published mapping 127.0.0.1:3100:3000 puts a port, not a host,
		// before ":3000".
		if m[1] == "3100" {
			continue
		}
		if m[1] != "scratch-forgejo" {
			t.Errorf("render names %q:3000, want only scratch-forgejo", m[1])
		}
	}
	runner := f.Services["scratch-forge-runner"]
	if _, ok := runner.DependsOn["scratch-forgejo"]; !ok {
		t.Errorf("runner depends_on = %v, want the scratch Forgejo", runner.DependsOn)
	}
	if runner.Environment["DOCKER_HOST"] != "tcp://docker:2376" {
		t.Errorf("runner DOCKER_HOST = %q, want its own engine, whose hostname is docker", runner.Environment["DOCKER_HOST"])
	}
	if f.Services["scratch-forge-dind"].Image != templates.DindImage {
		t.Errorf("runner engine image = %q, want DindImage %q", f.Services["scratch-forge-dind"].Image, templates.DindImage)
	}
}

// The stack runs the versions the live Forgejo project runs, each pinned by a
// tag or digest in images.go, and none is a moving tag.
func TestRenderDaemonCompose_ScratchForgeImagesAreThePinnedOnes(t *testing.T) {
	t.Parallel()
	_, f := render(t, forgeSpec())
	want := map[string]string{
		"scratch-forgejo":      "data.forgejo.org/forgejo/forgejo:16.0.5",
		"scratch-forgejo-db":   "postgres:17.11",
		"scratch-forge-runner": "data.forgejo.org/forgejo/runner:13.2.0",
		"scratch-forge-dind":   templates.DindImage,
	}
	pinned := imageTable(t)
	for name, image := range want {
		got := f.Services[name].Image
		if got != image {
			t.Errorf("service %q image = %q, want %q", name, got, image)
		}
		if !slices.Contains(pinned, got) {
			t.Errorf("service %q image %q is not a reference images.go declares", name, got)
		}
	}
}

// The render carries no secret and interpolates nothing but the one takeover
// setting the daemon service already reads: the database trusts its internal
// network instead of holding a password.
func TestRenderDaemonCompose_ScratchForgeCarriesNoSecretAndInterpolatesNothingNew(t *testing.T) {
	t.Parallel()
	out, _ := render(t, forgeSpec())
	if strings.Contains(strings.ToUpper(out), "PASSWD") || strings.Contains(strings.ToUpper(out), "PASSWORD") {
		t.Errorf("render names a password:\n%s", out)
	}
	if got := strings.Count(out, "${"); got != 1 {
		t.Errorf("render holds %d interpolations, want only GT_DAEMON_TAKEOVER's", got)
	}
}

// The forge's database is reachable on the internal network alone: the render
// puts it on no network the daemon or the runner joins.
func TestRenderDaemonCompose_ScratchForgeDatabaseIsOnItsInternalNetworkAlone(t *testing.T) {
	t.Parallel()
	_, f := render(t, forgeSpec())
	db := f.Services["scratch-forgejo-db"]
	if len(db.Networks) != 1 {
		t.Fatalf("database networks = %q, want one", db.Networks)
	}
	if n, ok := f.Networks[db.Networks[0]]; !ok || !n.Internal {
		t.Errorf("database network %q is not declared internal: true", db.Networks[0])
	}
	for _, name := range []string{"daemon", "scratch-forge-runner", "scratch-forge-dind"} {
		if slices.Contains(f.Services[name].Networks, db.Networks[0]) {
			t.Errorf("service %q joins the database's network %q", name, db.Networks[0])
		}
	}
}
