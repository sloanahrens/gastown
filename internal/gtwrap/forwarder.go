package gtwrap

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// This file is the forwarder the daemon image installs as bd. docs/design/bd-container.md
// has the call path for each caller ("Call path from each caller") and the cutover
// that installs it ("Migration and rollback", stage 5).

// realBdName is the real bd's name beside the wrapper, the layout
// internal/bdshim looks for. A bd.real beside the running wrapper alone puts bd in
// forwarder mode; the forwarder never runs it.
const realBdName = "bd.real"

// ExitRefused is the exit status for a command refused before it ran, as the
// gateway protocol already ends a refused call, so a caller can tell a refusal
// from the wrapper's own failure.
const ExitRefused = 125

// ErrNoGateway is the forwarder's refusal when bd is in forwarder mode and
// BD_GATEWAY_URL is unset. bd runs only in the bd container, so there is no
// local bd to fall back to (gt-q1beru).
var ErrNoGateway = errors.New("gtwrap: BD_GATEWAY_URL is unset, and bd is in forwarder mode")

// ForwarderRoute is the route a bd call takes.
type ForwarderRoute int

const (
	// RouteNone means the wrapper is not in forwarder mode: bd resolves as it did
	// before the wrapper was installed, and the caller keeps its default target.
	RouteNone ForwarderRoute = iota
	// RouteGateway sends the call to the bd gateway the plan's URL names.
	RouteGateway
)

// ForwarderInputs is what DecideForwarder reads: the name the wrapper was invoked
// as, the wrapper's own path with the reads that find the real bd beside it, and
// the two gateway variables.
type ForwarderInputs struct {
	// Tool is the name the wrapper was invoked as. Only bd is in forwarder mode.
	Tool string
	// Executable names the wrapper running now; the real bd sits beside it.
	Executable func() (string, error)
	// Stat checks for the real bd beside the wrapper. A real bd that cannot be
	// checked refuses; it is never read as absent.
	Stat func(string) (fs.FileInfo, error)
	// ReadFile reads the gateway token file, so an unreadable token refuses.
	ReadFile func(string) ([]byte, error)
	// GatewayURL is BD_GATEWAY_URL. Forwarder mode requires it; an empty value
	// refuses rather than run a local bd.
	GatewayURL string
	// TokenFile is BD_GATEWAY_TOKEN_FILE.
	TokenFile string
}

// ForwarderPlan is the decision: the route, the gateway's URL and token file for
// RouteGateway, and the reason the doctor prints.
type ForwarderPlan struct {
	Route     ForwarderRoute
	URL       string
	TokenFile string
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
// In forwarder mode BD_GATEWAY_URL is required, since bd runs only in the bd
// container: an unset or empty URL refuses with ErrNoGateway and never falls back to
// a local bd. A configured gateway's token file (BD_GATEWAY_TOKEN_FILE) must be
// readable; a URL with no token, or a token that cannot be read, refuses too.
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
	if _, err := in.Stat(realPath); errors.Is(err, fs.ErrNotExist) {
		return ForwarderPlan{Route: RouteNone, Reason: fmt.Sprintf("no %s sits beside the wrapper, so bd is not in forwarder mode", realPath)}, nil
	} else if err != nil {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: read the real bd %s: %w", realPath, err)
	}
	if in.GatewayURL == "" {
		return ForwarderPlan{}, fmt.Errorf("%w: %s sits beside the wrapper, so set BD_GATEWAY_URL and BD_GATEWAY_TOKEN_FILE for the bd gateway", ErrNoGateway, realBdName)
	}
	return gatewayPlan(in)
}

// gatewayPlan is the gateway route: the URL, with a token file that can be read.
func gatewayPlan(in ForwarderInputs) (ForwarderPlan, error) {
	if in.TokenFile == "" {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: BD_GATEWAY_URL is %q but BD_GATEWAY_TOKEN_FILE is unset, so the call has no token; set the token file", in.GatewayURL)
	}
	if in.ReadFile == nil {
		return ForwarderPlan{}, errors.New("gtwrap: no way to read the gateway token file is wired, so the bd call is refused")
	}
	if _, err := in.ReadFile(in.TokenFile); err != nil {
		return ForwarderPlan{}, fmt.Errorf("gtwrap: read the gateway token file %s: %w; the call has no token, so it is refused", in.TokenFile, err)
	}
	return ForwarderPlan{
		Route:     RouteGateway,
		URL:       in.GatewayURL,
		TokenFile: in.TokenFile,
		Reason:    fmt.Sprintf("the bd gateway at %s carries the call", in.GatewayURL),
	}, nil
}

// Forwarder is the target a bd call takes in forwarder mode. It only carries out the
// route DecideForwarder names, which is always the gateway.
type Forwarder struct {
	// In is what the decision reads.
	In ForwarderInputs
	// Gateway is the template for a gateway call. Its Town, Caller, timeout and
	// streams are the caller's; its URL and token file are the plan's.
	Gateway BdGateway
}

// plan is the decision Launch and Doctor both read. Both a refusal and RouteNone
// come back as an error.
func (f Forwarder) plan() (ForwarderPlan, error) {
	p, err := DecideForwarder(f.In)
	if err != nil {
		return ForwarderPlan{}, err
	}
	if p.Route == RouteNone {
		return ForwarderPlan{}, errors.New("gtwrap: " + p.Reason + "; the call is refused")
	}
	return p, nil
}

// Launch runs the call on the route the decision names: the gateway. An error means
// the call did not run; bd's own output is never wrapped or repeated.
func (f Forwarder) Launch(call Call) (int, error) {
	if len(call.Argv) == 0 || call.Argv[0] != nativeBdTool {
		return exitDown, fmt.Errorf("gtwrap: the forwarder runs bd, but the call names %q", argvName(call.Argv))
	}
	p, err := f.plan()
	if err != nil {
		return refusalExit(err), err
	}
	gateway := f.Gateway
	gateway.URL, gateway.TokenFile = p.URL, p.TokenFile
	return gateway.Launch(call)
}

// Doctor reports the route the decision names and why. A decision that refuses
// reports the rung down, with the refusal as the reason.
func (f Forwarder) Doctor() (Diagnosis, error) {
	p, err := f.plan()
	if err != nil {
		return Diagnosis{Rung: RungDown, Reason: err.Error()}, nil
	}
	gateway := f.Gateway
	gateway.URL, gateway.TokenFile = p.URL, p.TokenFile
	return gateway.Doctor()
}

// refusalExit is the status a decision error exits with: bd's refusal status when
// the forwarder has no gateway, the wrapper's own failure status for any other
// refusal.
func refusalExit(err error) int {
	if errors.Is(err, ErrNoGateway) {
		return ExitRefused
	}
	return exitDown
}

// argvName is argv[0] for a refusal, or a stand-in for an empty vector.
func argvName(argv []string) string {
	if len(argv) == 0 {
		return "no arguments"
	}
	return argv[0]
}
