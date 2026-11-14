package gtwrap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// This file is the forwarder the daemon image installs as bd. docs/design/bd-container.md
// has the call path for each caller ("Call path from each caller") and the cutover
// that installs it ("Migration and rollback", stage 5).

// realBdName is the real bd's name beside the wrapper, the layout
// internal/bdshim looks for.
const realBdName = "bd.real"

// ForwarderRoute is the route a bd call takes.
type ForwarderRoute int

const (
	// RouteNone means the wrapper is not in forwarder mode: bd resolves as it did
	// before the wrapper was installed, and the caller keeps its default target.
	RouteNone ForwarderRoute = iota
	// RouteGateway sends the call to the bd gateway the plan's URL names.
	RouteGateway
	// RouteLocal runs the real bd at the plan's RealBd path.
	RouteLocal
)

// ForwarderInputs is what DecideForwarder reads: the name the wrapper was invoked
// as, the wrapper's own path with the reads that find the real bd beside it, and
// the two gateway variables.
type ForwarderInputs struct {
	// Tool is the name the wrapper was invoked as. Only bd is in forwarder mode.
	Tool string
	// Executable names the wrapper running now; the real bd sits beside it.
	Executable func() (string, error)
	// Stat reads the real bd and the wrapper, so the real bd can be checked and a
	// real bd that is the wrapper itself refused rather than run in a loop.
	Stat func(string) (fs.FileInfo, error)
	// ReadFile reads the gateway token file, so an unreadable token refuses.
	ReadFile func(string) ([]byte, error)
	// GatewayURL is BD_GATEWAY_URL. Empty means no gateway is configured.
	GatewayURL string
	// TokenFile is BD_GATEWAY_TOKEN_FILE.
	TokenFile string
}

// ForwarderPlan is the decision: the route, the gateway's URL and token file for
// RouteGateway, the real bd's path for RouteLocal, and the reason the doctor prints.
type ForwarderPlan struct {
	Route     ForwarderRoute
	URL       string
	TokenFile string
	RealBd    string
	Reason    string
}

// DecideForwarder is the forwarder's one decision. Forwarder.Launch and
// Forwarder.Doctor both read it, so the doctor never reports a route the launch
// would refuse.
//
// Forwarder mode needs two facts together: the wrapper is invoked as bd, and a
// bd.real sits beside the running wrapper, the layout the daemon image ships. With
// either missing, bd is RouteNone and the caller keeps its default target. A bd.real
// that cannot be checked refuses; it is never read as absent.
//
// In forwarder mode a configured gateway (BD_GATEWAY_URL) takes the call, and its
// token file (BD_GATEWAY_TOKEN_FILE) must be readable. A URL with no token, or a
// token that cannot be read, refuses, and the local bd never stands in for it. With
// no URL the real bd runs, and it must be a regular executable file that is not the
// wrapper itself.
//
// Rollback is one step: delete BD_GATEWAY_URL from the daemon image's environment,
// and the next call runs the local bd (docs/design/bd-container.md, "Migration and
// rollback", stage 5). Nothing else restates it.
func DecideForwarder(in ForwarderInputs) (ForwarderPlan, error) {
	if in.Tool != nativeBdTool {
		return ForwarderPlan{Route: RouteNone, Reason: fmt.Sprintf("%s is not bd", in.Tool)}, nil
	}
	if in.Executable == nil || in.Stat == nil {
		return ForwarderPlan{}, errors.New("gtwrap: no way to find the wrapper or read the real bd beside it is wired, so the bd call is refused")
	}
	self, err := in.Executable()
	if err != nil {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: find the wrapper the real bd sits beside: %w", err)
	}
	realPath := filepath.Join(filepath.Dir(self), realBdName)
	realInfo, err := in.Stat(realPath)
	if errors.Is(err, fs.ErrNotExist) {
		return ForwarderPlan{Route: RouteNone, Reason: fmt.Sprintf("no %s sits beside the wrapper, so bd is not in forwarder mode", realPath)}, nil
	}
	if err != nil {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: read the real bd %s: %w", realPath, err)
	}
	if in.GatewayURL != "" {
		return gatewayPlan(in)
	}
	return localPlan(in, self, realPath, realInfo)
}

// gatewayPlan is the gateway route: the URL, with a token file that can be read.
func gatewayPlan(in ForwarderInputs) (ForwarderPlan, error) {
	if in.TokenFile == "" {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: BD_GATEWAY_URL is %q but BD_GATEWAY_TOKEN_FILE is unset, so the call has no token; set the token file, or unset BD_GATEWAY_URL to run the local bd", in.GatewayURL)
	}
	if in.ReadFile == nil {
		return ForwarderPlan{}, errors.New("gtwrap: no way to read the gateway token file is wired, so the bd call is refused")
	}
	if _, err := in.ReadFile(in.TokenFile); err != nil {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: read the gateway token file %s: %w; the call has no token, so it is refused, not run locally", in.TokenFile, err)
	}
	return ForwarderPlan{
		Route:     RouteGateway,
		URL:       in.GatewayURL,
		TokenFile: in.TokenFile,
		Reason:    fmt.Sprintf("the bd gateway at %s carries the call", in.GatewayURL),
	}, nil
}

// localPlan is the local route: the real bd beside the wrapper, which must be an
// executable file other than the wrapper.
func localPlan(in ForwarderInputs, self, realPath string, realInfo fs.FileInfo) (ForwarderPlan, error) {
	if !realInfo.Mode().IsRegular() || realInfo.Mode().Perm()&0o111 == 0 {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: %s is not an executable file, so no bd can run", realPath)
	}
	selfInfo, err := in.Stat(self)
	if err != nil {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: read the wrapper %s: %w", self, err)
	}
	if os.SameFile(selfInfo, realInfo) {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: %s is this wrapper itself and would run it again; install the real bd beside the wrapper under a name of its own", realPath)
	}
	return ForwarderPlan{
		Route:  RouteLocal,
		RealBd: realPath,
		Reason: fmt.Sprintf("no bd gateway is configured, so bd runs the real binary at %s", realPath),
	}, nil
}

// Forwarder is the target a bd call takes in forwarder mode. It only carries out the
// route DecideForwarder names.
type Forwarder struct {
	// In is what the decision reads.
	In ForwarderInputs
	// Gateway is the template for a gateway call. Its Town, Caller, timeout and
	// streams are the caller's; its URL and token file are the plan's.
	Gateway BdGateway
	// Run runs the real bd on the local route. Nil refuses that route.
	Run Process
	// Environ is the caller's whole environment, for the local bd. Plan's container
	// filters do not apply to a call that stays on this machine.
	Environ []string
}

// plan is the decision Launch and Doctor both read. The local route's runner is
// checked here, so Doctor reports the local rung only when Launch can run it. Both
// a refusal and RouteNone come back as an error.
func (f Forwarder) plan() (ForwarderPlan, error) {
	p, err := DecideForwarder(f.In)
	if err != nil {
		return ForwarderPlan{}, err
	}
	if p.Route == RouteNone {
		return ForwarderPlan{}, errors.New("gtwrap: " + p.Reason + "; the call is refused")
	}
	if p.Route == RouteLocal && f.Run == nil {
		return ForwarderPlan{}, errors.New("gtwrap: no runner is wired for the local bd, so the call is refused")
	}
	return p, nil
}

// Launch runs the call on the route the decision names: the gateway, or the real
// bd with bd's own arguments. An error means the call did not run; bd's own output
// is never wrapped or repeated.
func (f Forwarder) Launch(call Call) (int, error) {
	if len(call.Argv) == 0 || call.Argv[0] != nativeBdTool {
		return exitDown, fmt.Errorf("gtwrap: the forwarder runs bd, but the call names %q", argvName(call.Argv))
	}
	p, err := f.plan()
	if err != nil {
		return exitDown, err
	}
	if p.Route == RouteGateway {
		gateway := f.Gateway
		gateway.URL, gateway.TokenFile = p.URL, p.TokenFile
		return gateway.Launch(call)
	}
	return f.Run.Run(append([]string{p.RealBd}, call.Argv[1:]...), f.Environ, call.Dir)
}

// Doctor reports the route the decision names and why. A decision that refuses
// reports the rung down, with the refusal as the reason.
func (f Forwarder) Doctor() (Diagnosis, error) {
	p, err := f.plan()
	if err != nil {
		return Diagnosis{Rung: RungDown, Reason: err.Error()}, nil
	}
	if p.Route == RouteGateway {
		gateway := f.Gateway
		gateway.URL, gateway.TokenFile = p.URL, p.TokenFile
		return gateway.Doctor()
	}
	return Diagnosis{Rung: RungLocal, Reason: p.Reason}, nil
}

// argvName is argv[0] for a refusal, or a stand-in for an empty vector.
func argvName(argv []string) string {
	if len(argv) == 0 {
		return "no arguments"
	}
	return argv[0]
}
