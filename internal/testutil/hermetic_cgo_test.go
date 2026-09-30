package testutil

import (
	"errors"
	"testing"
)

// preserveGoEnv pins GOENV to the file the go tool reads before HOME is
// redirected (gt-mjll: a keg-only icu4c's cgo flags live there), and carries
// the cache locations the redirect would otherwise move. The go tool's own
// resolution under a moved HOME is TestIntegrationPreserveGoEnvPinsGoEnvPastHomeRedirect's.
func TestPreserveGoEnv_PinsGoEnvAndCarriesCaches(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, "HOME=/home/dev", "GOCACHE=/already/set").
		answer("/fake/bin/go env GOENV", "/home/dev/.config/go/env\n", nil).
		answer("/fake/bin/go env -json GOPATH GOMODCACHE", `{"GOPATH":"/home/dev/go","GOMODCACHE":""}`, nil)

	if err := f.preserveGoEnv(); err != nil {
		t.Fatalf("preserveGoEnv: %v", err)
	}
	for k, want := range map[string]string{
		"GOENV":   "/home/dev/.config/go/env",
		"GOPATH":  "/home/dev/go",
		"GOCACHE": "/already/set", // set by the caller: kept, not asked for
	} {
		if got := f.env.get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := f.env.LookupEnv("GOMODCACHE"); ok {
		t.Error("an empty go env value was carried")
	}
	for _, k := range []string{"CGO_CPPFLAGS", "CGO_LDFLAGS"} {
		if _, ok := f.env.LookupEnv(k); ok {
			t.Errorf("%s landed in the environment; only the GOENV pointer carries it", k)
		}
	}
}

// A GOENV the caller already set is kept, and nothing is asked when every
// carried variable is set too.
func TestPreserveGoEnv_KeepsWhatIsSet(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, "GOENV=/mine", "GOPATH=/p", "GOCACHE=/c", "GOMODCACHE=/m")
	if err := f.preserveGoEnv(); err != nil {
		t.Fatalf("preserveGoEnv: %v", err)
	}
	if got := f.env.get("GOENV"); got != "/mine" {
		t.Errorf("GOENV = %q, want the caller's", got)
	}
	if calls := f.commands(); len(calls) != 0 {
		t.Errorf("go was asked %q with everything already set", calls)
	}
}

// TestPreserveGoEnv_FailsLoudOnGoEnvError pins the loud half: a `go env`
// failure must surface as an error, not be treated as "nothing to carry";
// only a missing go tool is.
func TestPreserveGoEnv_FailsLoudOnGoEnvError(t *testing.T) {
	t.Parallel()
	failing := errors.New("exit status 1")
	f := newFakeHarness(t).answer("/fake/bin/go env GOENV", "", failing)
	if err := f.preserveGoEnv(); !errors.Is(err, failing) {
		t.Errorf("preserveGoEnv with a failing `go env GOENV` = %v, want the failure", err)
	}
	f = newFakeHarness(t, "GOENV=/set").answer("/fake/bin/go env -json GOPATH GOCACHE GOMODCACHE", "", failing)
	if err := f.preserveGoEnv(); !errors.Is(err, failing) {
		t.Errorf("preserveGoEnv with a failing `go env -json` = %v, want the failure", err)
	}
	f = newFakeHarness(t, "GOENV=/set").answer("/fake/bin/go env -json GOPATH GOCACHE GOMODCACHE", "not json", nil)
	if err := f.preserveGoEnv(); err == nil {
		t.Error("preserveGoEnv with unparseable `go env -json` = nil")
	}
	f = newFakeHarness(t).noTool("go")
	if err := f.preserveGoEnv(); err != nil || len(f.commands()) != 0 || len(f.env.Environ()) != 0 {
		t.Errorf("with no go tool: err %v, commands %q, env %q; want a silent no-op", err, f.commands(), f.env.Environ())
	}
}
