package doltserver

import (
	"errors"
	"strings"
	"testing"

	configpkg "github.com/steveyegge/gastown/internal/config"
)

// A server is started on a port picked before the start runs — freePort binds
// and releases one, dolt init and the bind follow a second later — so anything
// on the host can take that port in between. The 2026-10-15 sweep lost
// TestIntegrationDoltServerLifecycle to it: dolt answered "Port 49575 already
// in use." and a test that had done nothing wrong went red (gt-8dbyw). Every
// start of a server on a picked port goes through startOnFreePort.

// portRaceAttempts bounds what one port race costs: three starts, the refused
// one and up to two retries. A fourth would hide a host handing the same port
// out twice.
const portRaceAttempts = 3

// portRaceErr reports whether err is dolt refusing the port it was handed,
// rather than failing for a reason worth reporting. Dolt writes "Port 49575
// already in use." to its log, and the start error carries the tail; the
// adapter's own preflight says "port 49575 is already in use".
func portRaceErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already in use")
}

// realHostOnPort is the real machine with every town's Dolt endpoint at port
// for this adapter alone.
func realHostOnPort(port int) *host {
	return &host{doltEndpoint: func(string) (configpkg.DoltEndpoint, bool) {
		return configpkg.DoltEndpoint{Port: port}, true
	}}
}

// startOnFreePort starts a Dolt server through start, which is handed a host
// whose Dolt endpoint is the port nextPort picks. A start refused because that
// port is already in use is retried on a fresh port, at most portRaceAttempts
// times; any other failure comes back at once, and the returned host is the
// one whose port bound.
//
// nextPort is a parameter rather than freePort itself so both tiers can use
// this: freePort binds a socket, which the unit tier may not do, and a test
// watching the retry hands in a port it has bound.
func startOnFreePort(t *testing.T, nextPort func() int, start func(h *host) error) (*host, error) {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= portRaceAttempts; attempt++ {
		h := realHostOnPort(nextPort())
		lastErr = start(h)
		if lastErr == nil {
			return h, nil
		}
		if !portRaceErr(lastErr) {
			return nil, lastErr
		}
		if attempt < portRaceAttempts {
			t.Logf("start attempt %d lost the port race, retrying on a fresh port: %v", attempt, lastErr)
		}
	}
	return nil, lastErr
}

// doltPort is the port this host's town endpoint resolves to.
func doltPort(t *testing.T, h *host) int {
	t.Helper()
	ep, ok := h.resolveDoltEndpoint("")
	if !ok {
		t.Fatal("host resolves no Dolt endpoint")
	}
	return ep.Port
}

// The two answers dolt gives for a taken port, and two failures that are not
// one.
func TestPortRaceErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "dolt's own refusal, as the start error carries it",
			err:  errors.New("Dolt server process (PID 42) died during startup: exit status 1\nPort 41001 already in use."),
			want: true,
		},
		{
			name: "the adapter's preflight",
			err:  errors.New("port 41001 is already in use.\nPort is held by PID 42"),
			want: true,
		},
		{name: "no failure", err: nil, want: false},
		{
			name: "a start that failed for another reason",
			err:  errors.New("creating data directory: permission denied"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := portRaceErr(tc.err); got != tc.want {
				t.Errorf("portRaceErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A start refused for the port it was given comes back on a fresh one, and the
// host it returns is the one that bound.
func TestStartOnFreePortRetriesOnPortInUse(t *testing.T) {
	t.Parallel()
	picked := 0
	var tried []int
	h, err := startOnFreePort(t,
		func() int { picked++; return 41000 + picked },
		func(h *host) error {
			tried = append(tried, doltPort(t, h))
			if len(tried) == 1 {
				return errors.New("Dolt server process (PID 42) died during startup: exit status 1\nPort 41001 already in use.")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("startOnFreePort = %v, want the second port to start", err)
	}
	if len(tried) != 2 {
		t.Fatalf("start attempts on ports %v, want the refused port and a fresh one", tried)
	}
	if tried[0] == tried[1] {
		t.Errorf("the retry reused port %d", tried[1])
	}
	if got := doltPort(t, h); got != tried[1] {
		t.Errorf("the returned host is on port %d, want the port that started (%d)", got, tried[1])
	}
}

// A start that failed for any other reason is reported, not retried: a retry
// would spend a fresh port on a host that is not handing ports out twice.
func TestStartOnFreePortReportsOtherFailuresWithoutRetrying(t *testing.T) {
	t.Parallel()
	want := errors.New("opening log file: permission denied")
	calls := 0
	h, err := startOnFreePort(t, func() int { return 41000 }, func(*host) error {
		calls++
		return want
	})
	if h != nil {
		t.Errorf("host = %v, want nil when no start succeeded", h)
	}
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want %v unchanged", err, want)
	}
	if calls != 1 {
		t.Errorf("start calls = %d, want 1", calls)
	}
}

// The retry runs out: every attempt loses the race, and the last failure is
// what comes back.
func TestStartOnFreePortGivesUpAfterThreeStarts(t *testing.T) {
	t.Parallel()
	race := errors.New("Port 41000 already in use.")
	picks, calls := 0, 0
	h, err := startOnFreePort(t,
		func() int { picks++; return 41000 + picks },
		func(*host) error {
			calls++
			return race
		})
	if h != nil {
		t.Errorf("host = %v, want nil when no start succeeded", h)
	}
	if !errors.Is(err, race) {
		t.Errorf("error = %v, want the last port race %v", err, race)
	}
	if calls != portRaceAttempts {
		t.Errorf("start calls = %d, want %d", calls, portRaceAttempts)
	}
	if picks != portRaceAttempts {
		t.Errorf("ports picked = %d, want %d: every attempt gets a fresh one", picks, portRaceAttempts)
	}
}
