package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// bootSeams are the fakes a test supplies for newBootProbes. Each records its
// calls, so a test can prove a seam was never reached. container is the
// process's container mode, which the Dolt probe reads.
type bootSeams struct {
	forgejoErr   error
	dialErr      error
	engineErr    error
	container    bool
	forgejoCalls atomic.Int32
	dialCalls    atomic.Int32
	engineCalls  atomic.Int32
}

func (s *bootSeams) deps() bootProbeDeps {
	return bootProbeDeps{
		forgejoHealthy: func(context.Context, string) error {
			s.forgejoCalls.Add(1)
			return s.forgejoErr
		},
		dial: func(context.Context, string) error {
			s.dialCalls.Add(1)
			return s.dialErr
		},
		engineProbe: func(context.Context) ([]string, error) {
			s.engineCalls.Add(1)
			return nil, s.engineErr
		},
		inContainer: func() bool { return s.container },
	}
}

// bootOrigin is a Forgejo origin that a probe is built with.
func bootOrigin() (string, error) { return "https://forgejo.test", nil }

// bootMode returns a getenv seam whose only answer is the container mode.
func bootMode(mode string) func(string) string {
	return func(name string) string {
		if name == config.ContainerModeEnv {
			return mode
		}
		return ""
	}
}

// writeBootTownDolt writes the town's mayor/town.json with a Dolt endpoint on
// 127.0.0.1:port, the file the endpoint resolver reads first.
func writeBootTownDolt(t *testing.T, townRoot string, port int) {
	t.Helper()
	writeBootTownDoltHosts(t, townRoot, "127.0.0.1", "", port)
}

// writeBootTownDoltHosts writes mayor/town.json with a Dolt host, and a
// container_host when containerHost is set, on port.
func writeBootTownDoltHosts(t *testing.T, townRoot, host, containerHost string, port int) {
	t.Helper()
	dir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dolt := fmt.Sprintf(`"host":%q,"port":%d`, host, port)
	if containerHost != "" {
		dolt += fmt.Sprintf(`,"container_host":%q`, containerHost)
	}
	body := fmt.Sprintf(`{"type":"town","name":"boot-test","dolt":{%s}}`, dolt)
	if err := os.WriteFile(filepath.Join(dir, "town.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bootProbeNames is the sorted names of a probe set.
func bootProbeNames(probes []bootProbe) []string {
	names := make([]string, 0, len(probes))
	for _, p := range probes {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return names
}

// bootProbeByName returns the probe named name, and whether the set has one.
func bootProbeByName(probes []bootProbe, name string) (bootProbe, bool) {
	for _, p := range probes {
		if p.Name == name {
			return p, true
		}
	}
	return bootProbe{}, false
}

// bootAnswers reports whether a probe answers right now.
func bootAnswers(p bootProbe) bool {
	return p.Check(context.Background()) == nil
}

func TestBootProbesHostSetHasEngineForgejoAndDolt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		engineErr  error
		forgejoErr error
		dialErr    error
		wantAnswer map[string]bool
	}{
		{
			name:       "all answering",
			wantAnswer: map[string]bool{"docker": true, "forgejo": true, "dolt": true},
		},
		{
			name:       "engine error is not answering",
			engineErr:  errors.New("docker ps: cannot connect"),
			wantAnswer: map[string]bool{"docker": false, "forgejo": true, "dolt": true},
		},
		{
			name:       "forgejo error is not answering",
			forgejoErr: errors.New("forgejo health: 503"),
			wantAnswer: map[string]bool{"docker": true, "forgejo": false, "dolt": true},
		},
		{
			name:       "dial error is not answering",
			dialErr:    errors.New("connection refused"),
			wantAnswer: map[string]bool{"docker": true, "forgejo": true, "dolt": false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeBootTownDolt(t, townRoot, 3307)
			seams := bootSeams{engineErr: tt.engineErr, forgejoErr: tt.forgejoErr, dialErr: tt.dialErr}
			probes := newBootProbes(townRoot, bootMode(""), bootOrigin, seams.deps())
			if got, want := bootProbeNames(probes), []string{"docker", "dolt", "forgejo"}; !slices.Equal(got, want) {
				t.Fatalf("probe names = %v; want %v", got, want)
			}
			for name, want := range tt.wantAnswer {
				p, ok := bootProbeByName(probes, name)
				if !ok {
					t.Fatalf("probe %q missing from the set", name)
				}
				if got := bootAnswers(p); got != want {
					t.Errorf("probe %q answers = %v; want %v", name, got, want)
				}
			}
		})
	}
}

func TestBootProbesContainerAndPodSetsHaveNoEngineProbe(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeBootTownDolt(t, townRoot, 3307)
	seams := bootSeams{container: true}
	probes := newBootProbes(townRoot, bootMode(config.SupervisorContainer), bootOrigin, seams.deps())
	if got, want := bootProbeNames(probes), []string{"dolt", "forgejo"}; !slices.Equal(got, want) {
		t.Fatalf("probe names = %v; want %v", got, want)
	}
	for _, p := range probes {
		if !bootAnswers(p) {
			t.Errorf("probe %q not answering with its seam healthy", p.Name)
		}
	}
	if n := seams.engineCalls.Load(); n != 0 {
		t.Errorf("engine seam called %d times in container mode; want 0", n)
	}
}

func TestBootProbesForgejoNotConfiguredIsLeftOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		origin      func() (string, error)
		wantPresent bool
		wantAnswer  bool
	}{
		{
			name:   "origin reads as not configured",
			origin: func() (string, error) { return "", errForgejoNotConfigured },
		},
		{
			name:   "origin is empty with no error",
			origin: func() (string, error) { return "", nil },
		},
		{
			name:        "origin setting unreadable is not answering",
			origin:      func() (string, error) { return "", errForgejoSettingUnreadable },
			wantPresent: true,
		},
		{
			name:        "origin rig list unknown is not answering",
			origin:      func() (string, error) { return "", errForgejoRigListUnknown },
			wantPresent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeBootTownDolt(t, townRoot, 3307)
			var seams bootSeams
			probes := newBootProbes(townRoot, bootMode(""), tt.origin, seams.deps())
			p, present := bootProbeByName(probes, "forgejo")
			if present != tt.wantPresent {
				t.Fatalf("forgejo present = %v; want %v (set %v)", present, tt.wantPresent, bootProbeNames(probes))
			}
			if !present {
				return
			}
			if got := bootAnswers(p); got != tt.wantAnswer {
				t.Errorf("forgejo answers = %v; want %v", got, tt.wantAnswer)
			}
			if n := seams.forgejoCalls.Load(); n != 0 {
				t.Errorf("health call made %d times for an unreadable origin; want 0", n)
			}
		})
	}
}

func TestBootProbesDoltWithNoEndpointIsNotAnswering(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var seams bootSeams
	probes := newBootProbes(townRoot, bootMode(""), bootOrigin, seams.deps())
	p, ok := bootProbeByName(probes, "dolt")
	if !ok {
		t.Fatal("dolt probe missing from the set")
	}
	if bootAnswers(p) {
		t.Error("dolt answers with no endpoint named; want not answering")
	}
	if n := seams.dialCalls.Load(); n != 0 {
		t.Errorf("dial made %d times with no endpoint; want 0", n)
	}
}

func TestBootProbesDoltEndpointFollowsContainerMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		container bool
		wantAddr  string
	}{
		{name: "host daemon dials the host name", container: false, wantAddr: "127.0.0.1:3307"},
		{name: "container-mode daemon dials the container host", container: true, wantAddr: "172.20.0.5:3307"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeBootTownDoltHosts(t, townRoot, "127.0.0.1", "172.20.0.5", 3307)
			mode := ""
			if tt.container {
				mode = config.SupervisorContainer
			}
			var dialed []string
			seams := bootSeams{container: tt.container}
			deps := seams.deps()
			deps.dial = func(_ context.Context, addr string) error {
				dialed = append(dialed, addr)
				return nil
			}
			probes := newBootProbes(townRoot, bootMode(mode), bootOrigin, deps)
			p, ok := bootProbeByName(probes, "dolt")
			if !ok {
				t.Fatal("dolt probe missing from the set")
			}
			if !bootAnswers(p) {
				t.Error("dolt not answering with its dial seam accepting")
			}
			if !slices.Equal(dialed, []string{tt.wantAddr}) {
				t.Errorf("dialed %v; want [%s]", dialed, tt.wantAddr)
			}
		})
	}
}

func TestBootProbesDoltDialsTheResolvedEndpoint(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-network — the Dolt probe's production dialer must reach a real loopback socket this test owns; nothing is accepted or read past the connect
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	listening := listener.Addr().(*net.TCPAddr).Port

	//testpolicy:allow no-network — a loopback port this test binds and frees, so the dial to it is refused at once and the probe must not answer
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := closedListener.Addr().(*net.TCPAddr).Port
	if err := closedListener.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		port       int
		wantAnswer bool
	}{
		{name: "listener the town names answers", port: listening, wantAnswer: true},
		{name: "closed port does not answer", port: closed, wantAnswer: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeBootTownDolt(t, townRoot, tt.port)
			// The fixture must resolve, or a closed-port case would pass on a
			// missing endpoint rather than on the refused dial.
			if ep, ok := config.ResolveDoltEndpointFor(townRoot, false); !ok || ep.Port != tt.port {
				t.Fatalf("resolver reads port %d (ok %v); want %d", ep.Port, ok, tt.port)
			}
			var seams bootSeams
			deps := seams.deps()
			deps.dial = productionBootProbeDeps().dial
			probes := newBootProbes(townRoot, bootMode(""), bootOrigin, deps)
			p, ok := bootProbeByName(probes, "dolt")
			if !ok {
				t.Fatal("dolt probe missing from the set")
			}
			if got := bootAnswers(p); got != tt.wantAnswer {
				t.Errorf("dolt answers = %v; want %v", got, tt.wantAnswer)
			}
		})
	}
}
