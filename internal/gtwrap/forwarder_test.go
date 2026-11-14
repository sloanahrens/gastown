package gtwrap

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// gatewayURL is the gateway the forwarder rows configure.
const gatewayURL = "http://gw:7440"

// processRecorder is a Process that records the call and answers from its fields,
// so a test drives the local bd without starting one.
type processRecorder struct {
	ran  bool
	argv []string
	env  []string
	dir  string
	exit int
	err  error
}

func (p *processRecorder) Run(argv, env []string, dir string) (int, error) {
	p.ran, p.argv, p.env, p.dir = true, argv, env, dir
	return p.exit, p.err
}

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

// beside answers a stat that reports the wrapper and the real bd as executable
// regular files, so the decision's checks run without a test writing an executable.
func beside(self, real string) func(string) (fs.FileInfo, error) {
	return statFiles(map[string]os.FileMode{self: 0o755, real: 0o755})
}

// forwarderLocal is a Forwarder in forwarder mode with the real bd beside it, no
// gateway configured, and the runner given.
func forwarderLocal(t *testing.T, run Process) Forwarder {
	t.Helper()
	self, real := wrapperAt(t)
	return Forwarder{
		In:      inputs(self, beside(self, real), "", ""),
		Run:     run,
		Environ: []string{"PATH=/usr/bin", "BEADS_DIR=/town/.beads"},
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
		// want is the refusal's text, or the route's real bd name or gateway URL.
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
			name: "no gateway routes the call to the real bd beside the wrapper",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), "", "")
			},
			route: RouteLocal,
			want:  realBdName,
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
			name: "a gateway carries the call even when the real bd is not executable",
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
		{
			name: "the local route refuses a real bd that is not executable",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, statFiles(map[string]os.FileMode{self: 0o755, real: 0o644}), "", "")
			},
			refuse: true,
			want:   "not an executable file",
		},
		{
			name: "the local route refuses a real bd that is the wrapper itself",
			in: func(t *testing.T) ForwarderInputs {
				// The wrapper is the test binary, a real executable, and bd.real beside
				// it is a link to the same file; neither is written by the test.
				exe, err := os.Executable()
				if err != nil {
					t.Fatalf("find the test binary: %v", err)
				}
				dir := t.TempDir()
				self, real := filepath.Join(dir, "bd"), filepath.Join(dir, realBdName)
				for _, path := range []string{self, real} {
					if err := os.Symlink(exe, path); err != nil {
						t.Fatalf("symlink the test binary: %v", err)
					}
				}
				return inputs(self, os.Stat, "", "")
			},
			refuse: true,
			want:   "would run it again",
		},
		{
			name: "the local route refuses a wrapper whose metadata cannot be read",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				stat := beside(self, real)
				return inputs(self, func(path string) (fs.FileInfo, error) {
					if path == self {
						return nil, errors.New("the wrapper's own metadata is unreadable")
					}
					return stat(path)
				}, "", "")
			},
			refuse: true,
			want:   "read the wrapper",
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
			switch tt.route {
			case RouteLocal:
				if filepath.Base(plan.RealBd) != realBdName {
					t.Fatalf("real bd = %q, want the bd.real beside the wrapper", plan.RealBd)
				}
			case RouteGateway:
				if plan.URL != tt.want {
					t.Fatalf("gateway URL = %q, want %q", plan.URL, tt.want)
				}
			}
		})
	}
}

// TestForwarderAsBd drives the two routes Launch takes: a configured gateway
// carries the call, and with none configured the real bd beside the wrapper runs
// with bd's own arguments.
func TestForwarderAsBd(t *testing.T) {
	t.Parallel()
	t.Run("a configured gateway carries the call", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		tokenFile := writeGatewayToken(t, "sekrit\n")
		gateway, stdout, _, _ := gatewayTarget(t, town, "", doerFunc(func(*http.Request) (*http.Response, error) {
			return gatewayResponse(t,
				bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("gt-x\n")},
				bdgateway.ExitFrame(0)), nil
		}))
		self, real := wrapperAt(t)
		local := &processRecorder{exit: 9}
		f := Forwarder{
			In:      inputs(self, beside(self, real), gateway.URL, tokenFile),
			Gateway: gateway,
			Run:     local,
		}

		exit, err := f.Launch(Call{
			Argv: []string{"bd", "show", "gt-x"},
			Env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
			Dir:  town,
		})
		if err != nil || exit != 0 {
			t.Fatalf("Launch = %d, %v; want the gateway's 0, nil", exit, err)
		}
		if local.ran {
			t.Fatalf("the local bd ran %q beside a configured gateway", local.argv)
		}
		if got := stdout.String(); got != "gt-x\n" {
			t.Fatalf("stdout = %q, want the gateway's frame", got)
		}
	})

	t.Run("no gateway runs the local real bd", func(t *testing.T) {
		t.Parallel()
		run := &processRecorder{exit: 3}
		f := forwarderLocal(t, run)
		self, err := f.In.Executable()
		if err != nil {
			t.Fatalf("wrapper path: %v", err)
		}

		exit, err := f.Launch(Call{Argv: []string{"bd", "create", "--title", "t"}, Dir: t.TempDir()})
		if err != nil {
			t.Fatalf("Launch = %v", err)
		}
		if exit != 3 {
			t.Fatalf("Launch = %d, want the real bd's 3", exit)
		}
		if !run.ran {
			t.Fatal("the local bd did not run with no gateway configured")
		}
		want := []string{filepath.Join(filepath.Dir(self), realBdName), "create", "--title", "t"}
		if !slices.Equal(run.argv, want) {
			t.Fatalf("ran %q, want %q", run.argv, want)
		}
		if !slices.Equal(run.env, f.Environ) {
			t.Fatalf("env = %q, want the caller's whole environment %q", run.env, f.Environ)
		}
	})
}

// TestForwarderNoSilentFallback: a gateway that is configured but cannot answer is
// an error naming its URL, and the local bd never runs in its place, so a caller
// never reads a local bd's result as the gateway's.
func TestForwarderNoSilentFallback(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tokenFile := writeGatewayToken(t, "sekrit\n")
	gateway, _, _, _ := gatewayTarget(t, town, "", doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))
	self, real := wrapperAt(t)
	local := &processRecorder{exit: 0}
	f := Forwarder{
		In:      inputs(self, beside(self, real), gateway.URL, tokenFile),
		Gateway: gateway,
		Run:     local,
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
	if local.ran {
		t.Fatalf("the local bd ran %q in place of a down gateway", local.argv)
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
			name: "a call not named bd",
			f: func(t *testing.T) Forwarder {
				return forwarderLocal(t, &processRecorder{})
			},
			argv:   []string{"gt", "status"},
			reason: `names "gt"`,
		},
		{
			name: "a call with no arguments",
			f: func(t *testing.T) Forwarder {
				return forwarderLocal(t, &processRecorder{})
			},
			argv:   nil,
			reason: "no arguments",
		},
		{
			name: "no runner wired for the local route",
			f: func(t *testing.T) Forwarder {
				return forwarderLocal(t, nil)
			},
			argv:   []string{"bd", "show", "gt-x"},
			reason: "no runner is wired",
		},
		{
			name: "no bd.real, so the wrapper is not in forwarder mode",
			f: func(t *testing.T) Forwarder {
				self, _ := wrapperAt(t)
				return Forwarder{In: inputs(self, statFiles(map[string]os.FileMode{self: 0o755}), "", ""), Run: &processRecorder{}}
			},
			argv:   []string{"bd", "show", "gt-x"},
			reason: "not in forwarder mode",
		},
		{
			name: "the wrapper cannot be found",
			f: func(t *testing.T) Forwarder {
				in := ForwarderInputs{Tool: nativeBdTool, Stat: os.Stat}
				in.Executable = func() (string, error) { return "", errors.New("no own path") }
				return Forwarder{In: in, Run: &processRecorder{}}
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
		// nilRun leaves the local route's runner unwired.
		nilRun bool
		rung   Rung
		// reason is text the doctor's reason must contain.
		reason string
		// local is whether the launch runs the real bd.
		local bool
	}{
		{
			name: "no gateway: the real bd runs",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), "", "")
			},
			rung:   RungLocal,
			reason: realBdName,
			local:  true,
		},
		{
			name: "a gateway with its token: the gateway carries the call",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, writeGatewayToken(t, "sekrit\n"))
			},
			rung:   RungExec,
			reason: gatewayURL,
		},
		{
			name: "a gateway with no token file: refused",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, "")
			},
			rung:   RungDown,
			reason: "BD_GATEWAY_TOKEN_FILE",
		},
		{
			name: "a gateway with an unreadable token: refused",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), gatewayURL, filepath.Join(t.TempDir(), "missing.token"))
			},
			rung:   RungDown,
			reason: "read the gateway token file",
		},
		{
			name: "no real bd beside the wrapper: not in forwarder mode",
			in: func(t *testing.T) ForwarderInputs {
				self, _ := wrapperAt(t)
				return inputs(self, statFiles(map[string]os.FileMode{self: 0o755}), "", "")
			},
			rung:   RungDown,
			reason: "not in forwarder mode",
		},
		{
			name: "the real bd is not executable: refused",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, statFiles(map[string]os.FileMode{self: 0o755, real: 0o644}), "", "")
			},
			rung:   RungDown,
			reason: "not an executable file",
		},
		{
			name: "the real bd is the wrapper: refused",
			in: func(t *testing.T) ForwarderInputs {
				exe, err := os.Executable()
				if err != nil {
					t.Fatalf("find the test binary: %v", err)
				}
				dir := t.TempDir()
				self, real := filepath.Join(dir, "bd"), filepath.Join(dir, realBdName)
				for _, path := range []string{self, real} {
					if err := os.Symlink(exe, path); err != nil {
						t.Fatalf("symlink the test binary: %v", err)
					}
				}
				return inputs(self, os.Stat, "", "")
			},
			rung:   RungDown,
			reason: "would run it again",
		},
		{
			name: "no runner wired for the local route: the doctor reports down, as the launch refuses",
			in: func(t *testing.T) ForwarderInputs {
				self, real := wrapperAt(t)
				return inputs(self, beside(self, real), "", "")
			},
			nilRun: true,
			rung:   RungDown,
			reason: "no runner is wired",
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
			var run Process
			local := &processRecorder{exit: 0}
			if !tt.nilRun {
				run = local
			}
			f := Forwarder{In: tt.in(t), Gateway: gateway, Run: run, Environ: []string{"PATH=/usr/bin"}}

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
			if lerr != nil {
				if exit != exitDown {
					t.Fatalf("refused Launch = %d, want %d", exit, exitDown)
				}
			} else if exit != 0 {
				t.Fatalf("Launch = %d, want 0", exit)
			}
			if local.ran != tt.local {
				t.Fatalf("the local bd ran = %v, want %v", local.ran, tt.local)
			}
		})
	}
}

// TestRungLocalHasItsOwnName keeps the word the doctor prints distinct from the
// other rungs.
func TestRungLocalHasItsOwnName(t *testing.T) {
	t.Parallel()
	seen := map[string]Rung{}
	for _, rung := range []Rung{RungExec, RungRun, RungDown, RungLocal} {
		name := rung.String()
		if other, ok := seen[name]; ok {
			t.Fatalf("rungs %s and %s both print %q", other, rung, name)
		}
		seen[name] = rung
	}
	if got := RungLocal.String(); got != "local" {
		t.Fatalf("RungLocal prints %q, want %q", got, "local")
	}
}
