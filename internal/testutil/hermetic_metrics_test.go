package testutil

import (
	"os"
	"strings"
	"testing"
)

// The harness must leave bdTelemetryOff (see its comment in hermetic.go for
// why) set after its own scrub — which removes BD_* — and again after every
// per-test scrub, and CleanGTEnv must carry the same switches to
// subprocesses (gt-wcq2).
func TestStartHermetic_DisablesBDTelemetry(t *testing.T) {
	withSavedEnv(t)
	_ = os.Unsetenv("BD_DISABLE_METRICS")
	_ = os.Unsetenv("BD_DISABLE_EVENT_FLUSH")

	h, err := StartHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	for k, want := range bdTelemetryOff {
		if got := os.Getenv(k); got != want {
			t.Errorf("after StartHermetic %s = %q, want %s", k, got, want)
		}
	}

	// HermeticTest scrubs again per test; the switches must survive it.
	HermeticTest(t)
	for k, want := range bdTelemetryOff {
		if got := os.Getenv(k); got != want {
			t.Errorf("after HermeticTest %s = %q, want %s", k, got, want)
		}
	}

	// And they reach subprocesses built with CleanGTEnv, which strips BD_*.
	env := strings.Join(CleanGTEnv(), "\n")
	for k, v := range bdTelemetryOff {
		if !strings.Contains(env, k+"="+v) {
			t.Errorf("CleanGTEnv() lacks %s=%s", k, v)
		}
	}
}
