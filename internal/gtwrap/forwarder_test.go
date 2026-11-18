package gtwrap

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// gatewayURL is the gateway the forwarder rows configure.
const gatewayURL = "http://gw:7440"

// wrapperAt names a wrapper and the real bd beside it, in a temp dir.
func wrapperAt(t *testing.T) (self, real string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "bd"), filepath.Join(dir, realBdName)
}

// inputs is the decision's input for a wrapper at self whose files answer stat, with
// the gateway variables given. The token file is read with os.ReadFile.
func inputs(self string, stat func(string) (fs.FileInfo, error), url, tokenFile string) ForwarderInputs {
	return ForwarderInputs{
		Tool:       nativeBdTool,
		Executable: func() (string, error) { return self, nil },
		Stat:       stat,
		ReadFile:   os.ReadFile,
		GatewayURL: url,
		TokenFile:  tokenFile,
	}
}

// beside answers a stat that reports the wrapper and the real bd as present, so the
// decision's checks run without a test writing a file.
func beside(self, real string) func(string) (fs.FileInfo, error) {
	return statFiles(map[string]os.FileMode{self: 0o755, real: 0o755})
}

// gatewayForwarder is a Forwarder in forwarder mode with a readable token and a
// gateway whose every request answers with an empty success.
func gatewayForwarder(t *testing.T) Forwarder {
	t.Helper()
	town := t.TempDir()
	self, real := wrapperAt(t)
	token := writeGatewayToken(t, "sekrit\n")
	gateway, _, _, _ := gatewayTarget(t, town, token, doerFunc(func(*http.Request) (*http.Response, error) {
		return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
	}))
	return Forwarder{
		In:      inputs(self, beside(self, real), gatewayURL, token),
		Gateway: gateway,
	}
}

// TestDecideForwarder is the one decision, row by row. Each row is a wrapper's
// files and the gateway variables, and the route or the refusal the decision gives.
func TestDecideForwarder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    func(t *testing.T) ForwarderInputs
		route ForwarderRoute
		// want is the refusal's text, or the route's gateway URL.
		want   string
		refuse bool
	}{
		{
			name: "another tool is not in forwarder mode and reads nothing",
			in: func(t *testing.T) ForwarderInputs {
				return ForwarderInputs{Tool: "gt"}
			},
			route: RouteNone,
		},
		{
			name: "no bd.real beside the wrapper keeps the default",
			in: func(t *testing.T) ForwarderInputs {
				self, _ := wrapperAt(t)
				return inputs(self, statFiles(map[string]os.FileMode{self: 0o755}), "", "")
			},
			route: RouteNone,
		},
		{
			name: "no bd.real beside the wrapper keeps the default, gateway or not",
			in: func(t *testing.T) ForwarderInputs {
				self, _ := wrapperAt(t)
				in := inputs(self, statFiles(map[string]os.FileMode{self: 0o755}), gatewayURL, "")
				in.ReadFile = nil
				return in
			},
			route: RouteNone,
		},
		{
			name: "a bd.real that cannot be checked refuses, never reads as absent",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, func(path string) (fs.FileInfo, error) {
					if path == real {
						return nil, fs.ErrPermission
					}
					return fakeInfo{mode: 0o755}, nil
				}, "", "")
			},
			refuse: true,
			want:   "read the real bd",
		},
		{
			name: "a wrapper that cannot be found refuses",
			in: func(t *testing.T) ForwarderInputs {
				in := ForwarderInputs{Tool: nativeBdTool, Stat: os.Stat}
				in.Executable = func() (string, error) { return "", errors.New("no own path on this platform") }
				return in
			},
			refuse: true,
			want:   "find the wrapper",
		},
		{
			name: "no way to find the wrapper is wired refuses",
			in: func(t *testing.T) ForwarderInputs {
				return ForwarderInputs{Tool: nativeBdTool, Stat: os.Stat}
			},
			refuse: true,
			want:   "no way to find the wrapper",
		},
		{
			name: "no way to read bd.real is wired refuses",
			in: func(t *testing.T) ForwarderInputs {
				return ForwarderInputs{Tool: nativeBdTool, Executable: func() (string, error) { return "/opt/bd/bd", nil }}
			},
			refuse: true,
			want:   "no way to find the wrapper or read",
		},
		{
			name: "a configured gateway carries the call when its token can be read",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, writeGatewayToken(t, "sekrit\n"))
			},
			route: RouteGateway,
			want:  gatewayURL,
		},
		{
			name: "a gateway carries the call whatever bd.real's mode, since it never runs it",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				stat := statFiles(map[string]os.FileMode{self: 0o755, real: 0o644})
				return inputs(self, stat, gatewayURL, writeGatewayToken(t, "sekrit\n"))
			},
			route: RouteGateway,
			want:  gatewayURL,
		},
		{
			name: "a gateway URL with no token file refuses naming both variables",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, "")
			},
			refuse: true,
			want:   "BD_GATEWAY_TOKEN_FILE",
		},
		{
			name: "a gateway URL with an unreadable token refuses naming the file",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, filepath.Join(t.TempDir(), "missing.token"))
			},
			refuse: true,
			want:   "read the gateway token file",
		},
		{
			name: "a gateway URL with no way to read the token refuses",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				in := inputs(self, beside(self, real), gatewayURL, writeGatewayToken(t, "sekrit\n"))
				in.ReadFile = nil
				return in
			},
			refuse: true,
			want:   "no way to read the gateway token file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := DecideForwarder(tt.in(t))
			if tt.refuse {
				if err == nil {
					t.Fatalf("DecideForwarder = %+v, nil; want a refusal", plan)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error %q does not name %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecideForwarder = %v; want route %d", err, tt.route)
			}
			if plan.Route != tt.route {
				t.Fatalf("route = %d, want %d (reason: %s)", plan.Route, tt.route, plan.Reason)
			}
			if tt.route == RouteGateway && plan.URL != tt.want {
				t.Fatalf("gateway URL = %q, want %q", plan.URL, tt.want)
			}
		})
	}
}

// TestForwarderNoGateway: in forwarder mode with BD_GATEWAY_URL unset or empty, the
// plan is a refusal naming BD_GATEWAY_URL and nothing runs, whether or not a token
// file is configured (gt-q1beru).
func TestForwarderNoGateway(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		tokenFile func(t *testing.T) string
	}{
		{name: "with no token file either", tokenFile: func(*testing.T) string { return "" }},
		{name: "with a token file a configured gateway would use", tokenFile: func(t *testing.T) string {
			return writeGatewayToken(t, "sekrit\n")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			self, real := wrapperAt(t)
			token := tt.tokenFile(t)
			in := inputs(self, beside(self, real), "", token)

			plan, err := DecideForwarder(in)
			if err == nil {
				t.Fatalf("DecideForwarder = %+v, nil; want a refusal", plan)
			}
			if !errors.Is(err, ErrNoGateway) {
				t.Fatalf("DecideForwarder error %q does not wrap ErrNoGateway", err)
			}
			if !strings.Contains(err.Error(), "BD_GATEWAY_URL") {
				t.Fatalf("error %q does not name BD_GATEWAY_URL", err)
			}

			calls := 0
			gateway, _, _, _ := gatewayTarget(t, town, token, doerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
			}))
			f := Forwarder{In: in, Gateway: gateway}
			exit, err := f.Launch(Call{Argv: []string{"bd", "show", "gt-x"}, Dir: town})
			if err == nil {
				t.Fatalf("Launch = %d, nil; want a refusal", exit)
			}
			if exit != ExitRefused {
				t.Fatalf("Launch = %d, want bd's refusal status %d", exit, ExitRefused)
			}
			if calls != 0 {
				t.Fatalf("the gateway was called %d times with no BD_GATEWAY_URL", calls)
			}
		})
	}
}

// TestForwarderGatewayRoutes: with the URL and a readable token file the call routes
// to the gateway target, and a URL without a token file still errors as before.
func TestForwarderGatewayRoutes(t *testing.T) {
	t.Parallel()
	t.Run("a URL and a readable token route the call to the gateway", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		token := writeGatewayToken(t, "sekrit\n")
		self, real := wrapperAt(t)
		in := inputs(self, beside(self, real), gatewayURL, token)

		plan, err := DecideForwarder(in)
		if err != nil {
			t.Fatalf("DecideForwarder = %v; want the gateway route", err)
		}
		if plan.Route != RouteGateway || plan.URL != gatewayURL || plan.TokenFile != token {
			t.Fatalf("plan = %+v, want the gateway %s with the token %s", plan, gatewayURL, token)
		}

		calls := 0
		gateway, stdout, _, _ := gatewayTarget(t, town, token, doerFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if got, want := req.URL.String(), gatewayURL+"/v1/exec"; got != want {
				t.Errorf("request URL = %q, want %q", got, want)
			}
			return gatewayResponse(t,
				bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("gt-x\n")},
				bdgateway.ExitFrame(0)), nil
		}))
		f := Forwarder{In: in, Gateway: gateway}
		exit, err := f.Launch(Call{
			Argv: []string{"bd", "show", "gt-x"},
			Env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
			Dir:  town,
		})
		if err != nil || exit != 0 {
			t.Fatalf("Launch = %d, %v; want the gateway's 0, nil", exit, err)
		}
		if calls != 1 {
			t.Fatalf("the gateway was called %d times, want 1", calls)
		}
		if got := stdout.String(); got != "gt-x\n" {
			t.Fatalf("stdout = %q, want the gateway's frame", got)
		}
	})

	t.Run("a URL without a token file still refuses", func(t *testing.T) {
		t.Parallel()
		self, real := wrapperAt(t)
		in := inputs(self, beside(self, real), gatewayURL, "")
		plan, err := DecideForwarder(in)
		if err == nil {
			t.Fatalf("DecideForwarder = %+v, nil; want a refusal", plan)
		}
		if !strings.Contains(err.Error(), "BD_GATEWAY_TOKEN_FILE") {
			t.Fatalf("error %q does not name BD_GATEWAY_TOKEN_FILE", err)
		}
		f := Forwarder{In: in}
		if exit, err := f.Launch(Call{Argv: []string{"bd", "show", "gt-x"}}); err == nil || exit != exitDown {
			t.Fatalf("Launch = %d, %v; want the refusal %d", exit, err, exitDown)
		}
	})
}

// TestForwarderNoSilentFallback: a gateway that is configured but cannot answer is
// an error naming its URL, so a caller never reads some other result as the
// gateway's.
func TestForwarderNoSilentFallback(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tokenFile := writeGatewayToken(t, "sekrit\n")
	gateway, _, _, _ := gatewayTarget(t, town, "", doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))
	self, real := wrapperAt(t)
	f := Forwarder{
		In:      inputs(self, beside(self, real), gateway.URL, tokenFile),
		Gateway: gateway,
	}

	exit, err := f.Launch(Call{
		Argv: []string{"bd", "show", "gt-x"},
		Env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
		Dir:  town,
	})
	if err == nil {
		t.Fatal("a down gateway returned no error")
	}
	if exit != exitDown {
		t.Fatalf("Launch = %d, want the wrapper's %d", exit, exitDown)
	}
	if !strings.Contains(err.Error(), gateway.URL) {
		t.Fatalf("error %q does not name the gateway URL %s", err, gateway.URL)
	}
}

// TestForwarderRefusesACallItCannotRun: each way a bd call cannot reach a route is a
// refusal naming why, never a call that appears to have run.
func TestForwarderRefusesACallItCannotRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		f      func(t *testing.T) Forwarder
		argv   []string
		reason string
	}{
		{
			name:   "a call not named bd",
			f:      gatewayForwarder,
			argv:   []string{"gt", "status"},
			reason: `names "gt"`,
		},
		{
			name:   "a call with no arguments",
			f:      gatewayForwarder,
			argv:   nil,
			reason: "no arguments",
		},
		{
			name: "no bd.real, so the wrapper is not in forwarder mode",
			f: func(t *testing.T) Forwarder {
				self, _ := wrapperAt(t)
				return Forwarder{In: inputs(self, statFiles(map[string]os.FileMode{self: 0o755}), "", "")}
			},
			argv:   []string{"bd", "show", "gt-x"},
			reason: "not in forwarder mode",
		},
		{
			name: "the wrapper cannot be found",
			f: func(t *testing.T) Forwarder {
				in := ForwarderInputs{Tool: nativeBdTool, Stat: os.Stat}
				in.Executable = func() (string, error) { return "", errors.New("no own path") }
				return Forwarder{In: in}
			},
			argv:   []string{"bd", "show", "gt-x"},
			reason: "find the wrapper",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exit, err := tt.f(t).Launch(Call{Argv: tt.argv})
			if err == nil {
				t.Fatalf("Launch = %d, nil; want a refusal", exit)
			}
			if exit != exitDown {
				t.Fatalf("Launch = %d, want %d", exit, exitDown)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("error %q does not name %q", err, tt.reason)
			}
		})
	}
}

// TestForwarderLaunchAndDoctorAgree drives Launch and Doctor from the same rows. Each
// row's rung is the one the doctor reports, and the launch refuses exactly when that
// rung is down: the doctor never shows a route the launch would refuse.
func TestForwarderLaunchAndDoctorAgree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   func(t *testing.T) ForwarderInputs
		rung Rung
		// reason is text the doctor's reason must contain.
		reason string
		// exit is the launch's status: 0 when the call runs, the refusal status
		// otherwise.
		exit int
	}{
		{
			name: "a gateway with its token: the gateway carries the call",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, writeGatewayToken(t, "sekrit\n"))
			},
			rung:   RungExec,
			reason: gatewayURL,
			exit:   0,
		},
		{
			name: "a gateway with no token file: refused",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, "")
			},
			rung:   RungDown,
			reason: "BD_GATEWAY_TOKEN_FILE",
			exit:   exitDown,
		},
		{
			name: "a gateway with an unreadable token: refused",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, filepath.Join(t.TempDir(), "missing.token"))
			},
			rung:   RungDown,
			reason: "read the gateway token file",
			exit:   exitDown,
		},
		{
			name: "no gateway configured: refused",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), "", "")
			},
			rung:   RungDown,
			reason: "BD_GATEWAY_URL",
			exit:   ExitRefused,
		},
		{
			name: "no real bd beside the wrapper: not in forwarder mode",
			in: func(t *testing.T) ForwarderInputs {
				self, _ := wrapperAt(t)
				return inputs(self, statFiles(map[string]os.FileMode{self: 0o755}), "", "")
			},
			rung:   RungDown,
			reason: "not in forwarder mode",
			exit:   exitDown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			gateway, _, _, _ := gatewayTarget(t, town, "", doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return statusResponse(http.StatusOK, ""), nil
				}
				return gatewayResponse(t,
					bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("gt-x\n")},
					bdgateway.ExitFrame(0)), nil
			}))
			f := Forwarder{In: tt.in(t), Gateway: gateway}

			diag, err := f.Doctor()
			if err != nil {
				t.Fatalf("Doctor = %v", err)
			}
			if diag.Rung != tt.rung {
				t.Fatalf("rung = %s, want %s (reason: %s)", diag.Rung, tt.rung, diag.Reason)
			}
			if !strings.Contains(diag.Reason, tt.reason) {
				t.Fatalf("reason %q does not name %q", diag.Reason, tt.reason)
			}

			exit, lerr := f.Launch(Call{
				Argv: []string{"bd", "show", "gt-x"},
				Env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
				Dir:  town,
			})
			if (diag.Rung == RungDown) != (lerr != nil) {
				t.Fatalf("doctor says %s but Launch = %d, %v; they must agree", diag.Rung, exit, lerr)
			}
			if exit != tt.exit {
				t.Fatalf("Launch = %d, want %d", exit, tt.exit)
			}
		})
	}
}
