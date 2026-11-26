package config

import (
	"errors"
	"slices"
	"testing"
)

// TestDeferredBookkeepingOnOff pins who defers bookkeeping: a nil config, a nil
// block, an empty rigs list and a rig not listed are off; a listed rig is on.
// An empty list is the off reading, never "every rig".
func TestDeferredBookkeepingOnOff(t *testing.T) {
	t.Parallel()
	listed := &LandingWorkerConfig{
		DeferredBookkeeping: &DeferredBookkeepingConfig{Rigs: []string{"gastown"}},
	}
	cases := map[string]struct {
		cfg  *LandingWorkerConfig
		want bool
	}{
		"nil config": {nil, false},
		"nil block":  {&LandingWorkerConfig{}, false},
		"empty rigs": {&LandingWorkerConfig{DeferredBookkeeping: &DeferredBookkeepingConfig{}}, false},
		"rig not listed": {&LandingWorkerConfig{
			DeferredBookkeeping: &DeferredBookkeepingConfig{Rigs: []string{"longeye"}},
		}, false},
		"rig listed": {listed, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.DeferredBookkeepingFor("gastown"); got != tc.want {
				t.Errorf("DeferredBookkeepingFor(gastown) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeferredBookkeepingStrictDecode: the block decodes as written, an absent
// block is nil, and a key under it that the struct does not name is refused
// because the daemon document is decoded strictly.
func TestDeferredBookkeepingStrictDecode(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"landing_worker":{"deferred_bookkeeping":{"rigs":["gastown","longeye"]}}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("deferred_bookkeeping = %v", err)
	}
	lw := cfg.Patrols.LandingWorker
	if lw == nil || lw.DeferredBookkeeping == nil {
		t.Fatalf("deferred_bookkeeping decoded as %+v", lw)
	}
	if got := lw.DeferredBookkeeping.Rigs; !slices.Equal(got, []string{"gastown", "longeye"}) {
		t.Errorf("deferred_bookkeeping rigs = %v, want [gastown longeye]", got)
	}
	if !lw.DeferredBookkeepingFor("gastown") || !lw.DeferredBookkeepingFor("longeye") ||
		lw.DeferredBookkeepingFor("unlisted") {
		t.Errorf("DeferredBookkeepingFor does not follow the decoded rigs")
	}

	// The block was written, so it is not nil; its empty rigs are the off
	// reading.
	var empty DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json",
		[]byte(`{"patrols":{"landing_worker":{"deferred_bookkeeping":{}}}}`), &empty); err != nil {
		t.Fatalf("empty deferred_bookkeeping = %v", err)
	}
	block := empty.Patrols.LandingWorker.DeferredBookkeeping
	if block == nil || len(block.Rigs) != 0 {
		t.Fatalf("empty deferred_bookkeeping decoded as %+v, want a non-nil empty block", block)
	}
	if empty.Patrols.LandingWorker.DeferredBookkeepingFor("gastown") {
		t.Error("an empty deferred_bookkeeping is on for gastown, want off")
	}

	// Absent leaves the field nil, and nil is off.
	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json",
		[]byte(`{"patrols":{"landing_worker":{"enabled":true,"interval":"60s"}}}`), &absent); err != nil {
		t.Fatalf("landing_worker without deferred_bookkeeping = %v", err)
	}
	if lw := absent.Patrols.LandingWorker; lw == nil || lw.DeferredBookkeeping != nil {
		t.Fatalf("an absent deferred_bookkeeping decoded as %+v, want nil", lw)
	}

	for _, body := range []string{
		`{"patrols":{"landing_worker":{"deferred_bookkeeping":{"rigs":["gastown"],"bogus":1}}}}`,
		`{"patrols":{"landing_worker":{"deferred_bookkeeping":{"rig":["gastown"]}}}}`,
	} {
		err := DecodeJSONFile("daemon.json", []byte(body), &DaemonPatrolConfig{})
		if !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s = %v, want ErrUnparseable", body, err)
		}
	}
}
