package daemon

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// bootProbeDialTimeout bounds one Dolt dial. A listener that is up accepts at
// once and a closed port refuses at once, so the bound only bites on an address
// that never answers the SYN.
const bootProbeDialTimeout = 2 * time.Second

// bootProbeDeps are the seams the boot probes reach the outside world through.
// productionBootProbeDeps is the real value; tests supply fakes.
type bootProbeDeps struct {
	// forgejoHealthy asks the Forgejo at base its health endpoint; nil answers.
	forgejoHealthy func(ctx context.Context, base string) error
	// dial opens a TCP connection to addr, the Dolt endpoint; nil answers.
	dial func(ctx context.Context, addr string) error
	// docker reads the running containers; a nil error answers.
	docker func(ctx context.Context) ([]string, error)
}

// productionBootProbeDeps is the real seams: the Forgejo health read the
// attention collector makes, a TCP dial, and the docker ps read behind
// systemHostProbes.Containers.
func productionBootProbeDeps() bootProbeDeps {
	return bootProbeDeps{
		forgejoHealthy: func(ctx context.Context, base string) error {
			return systemHostProbes{forgejoBaseURL: func() (string, error) { return base, nil }}.ForgejoHealthy(ctx)
		},
		dial:   dialBootEndpoint,
		docker: systemHostProbes{}.Containers,
	}
}

// dialBootEndpoint connects to addr within bootProbeDialTimeout and closes the
// connection again; a connection accepted in time is an answer.
func dialBootEndpoint(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: bootProbeDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// newBootProbes returns the probe set a daemon waits on at boot. A host daemon
// waits on docker, forgejo and dolt. A daemon in container mode, which is the
// compose file's daemon and a pod's, has no container engine to ask, so its set
// has no docker probe. The Forgejo origin is read once here: a town that runs
// no Forgejo gets no forgejo probe, and a read that failed keeps the probe in
// the set, not answering.
func newBootProbes(townRoot string, getenv func(string) string, forgejoBaseURL func() (string, error), deps bootProbeDeps) []bootProbe {
	inContainer := getenv(config.ContainerModeEnv) == config.SupervisorContainer
	var probes []bootProbe
	if !inContainer {
		probes = append(probes, bootProbe{Name: "docker", Check: func(ctx context.Context) error {
			_, err := deps.docker(ctx)
			return err
		}})
	}
	if probe, ok := forgejoBootProbe(forgejoBaseURL, deps); ok {
		probes = append(probes, probe)
	}
	return append(probes, doltBootProbe(townRoot, inContainer, deps))
}

// forgejoBootProbe is the forgejo probe, and false when the town runs no
// Forgejo. An empty origin is the same absence ForgejoHealthy reads as
// errForgejoNotConfigured.
func forgejoBootProbe(forgejoBaseURL func() (string, error), deps bootProbeDeps) (bootProbe, bool) {
	base, err := forgejoBaseURL()
	if errors.Is(err, errForgejoNotConfigured) || (err == nil && base == "") {
		return bootProbe{}, false
	}
	return bootProbe{Name: "forgejo", Check: func(ctx context.Context) error {
		if err != nil {
			return err
		}
		return deps.forgejoHealthy(ctx, base)
	}}, true
}

// doltBootProbe dials the endpoint the town's resolver names for this process,
// read at each check so a town.json written during boot is picked up. A town
// that names no endpoint keeps the probe in the set, not answering: a Dolt
// nobody can locate is not up.
func doltBootProbe(townRoot string, inContainer bool, deps bootProbeDeps) bootProbe {
	return bootProbe{Name: "dolt", Check: func(ctx context.Context) error {
		ep, ok := config.ResolveDoltEndpointFor(townRoot, inContainer)
		if !ok {
			return errors.New("the town names no Dolt endpoint")
		}
		return deps.dial(ctx, net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)))
	}}
}
