package config

import (
	"errors"
	"slices"
	"testing"
)

// TestGateAheadOnOff pins who may gate a second candidate ahead: a nil config,
// a nil block, an empty rigs list and a rig not listed are off; a listed rig
// is on with min_ready, which is 2 when unset or below 2.
func TestGateAheadOnOff(t *testing.T) {
	t.Parallel()
	listed := func(minReady int) *LandingWorkerConfig {
		return &LandingWorkerConfig{GateAhead: &GateAheadConfig{Rigs: []string{"gastown"}, MinReady: minReady}}
	}
	cases := map[string]struct {
		cfg     *LandingWorkerConfig
		wantOn  bool
		wantMin int
	}{
		"nil config":                {nil, false, 0},
		"nil block":                 {&LandingWorkerConfig{}, false, 0},
		"empty rigs":                {&LandingWorkerConfig{GateAhead: &GateAheadConfig{}}, false, 0},
		"rig not listed":            {&LandingWorkerConfig{GateAhead: &GateAheadConfig{Rigs: []string{"longeye"}}}, false, 0},
		"listed, unset min_ready":   {listed(0), true, 2},
		"listed, min_ready 1":       {listed(1), true, 2},
		"listed, min_ready 2":       {listed(2), true, 2},
		"listed, min_ready above 2": {listed(5), true, 5},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			on, minReady := tc.cfg.GateAheadFor("gastown")
			if on != tc.wantOn || minReady != tc.wantMin {
				t.Errorf("GateAheadFor(gastown) = (%v, %d), want (%v, %d)",
					on, minReady, tc.wantOn, tc.wantMin)
			}
		})
	}
}

// TestGateAheadStrictDecode: the block decodes as written, an absent block is
// nil, and a key under it that the struct does not name is refused because
// the daemon document is decoded strictly.
func TestGateAheadStrictDecode(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"landing_worker":{"gate_ahead":{"rigs":["gastown","longeye"],"min_ready":3}}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("gate_ahead = %v", err)
	}
	lw := cfg.Patrols.LandingWorker
	if lw == nil || lw.GateAhead == nil {
		t.Fatalf("gate_ahead decoded as %+v", lw)
	}
	if got := lw.GateAhead.Rigs; !slices.Equal(got, []string{"gastown", "longeye"}) {
		t.Errorf("gate_ahead rigs = %v, want [gastown longeye]", got)
	}
	if on, minReady := lw.GateAheadFor("gastown"); !on || minReady != 3 {
		t.Errorf("GateAheadFor(gastown) = (%v, %d), want (true, 3)", on, minReady)
	}
	if on, _ := lw.GateAheadFor("unlisted"); on {
		t.Error("GateAheadFor(unlisted) is on, want off")
	}
	// An empty written block is not nil, but its empty rigs are the off reading.
	var empty DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"landing_worker":{"gate_ahead":{}}}}`), &empty); err != nil {
		t.Fatalf("empty gate_ahead = %v", err)
	}
	if block := empty.Patrols.LandingWorker.GateAhead; block == nil || len(block.Rigs) != 0 {
		t.Fatalf("empty gate_ahead decoded as %+v, want a non-nil empty block", block)
	}
	if on, _ := empty.Patrols.LandingWorker.GateAheadFor("gastown"); on {
		t.Error("an empty gate_ahead is on for gastown, want off")
	}
	// Absent leaves the field nil, and nil is off.
	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"landing_worker":{"enabled":true,"interval":"60s"}}}`), &absent); err != nil {
		t.Fatalf("landing_worker without gate_ahead = %v", err)
	}
	if lw := absent.Patrols.LandingWorker; lw == nil || lw.GateAhead != nil {
		t.Fatalf("an absent gate_ahead decoded as %+v, want nil", lw)
	}

	for _, body := range []string{
		`{"patrols":{"landing_worker":{"gate_ahead":{"rigs":["gastown"],"bogus":1}}}}`,
		`{"patrols":{"landing_worker":{"gate_ahead":{"rig":["gastown"]}}}}`,
	} {
		err := DecodeJSONFile("daemon.json", []byte(body), &DaemonPatrolConfig{})
		if !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s = %v, want ErrUnparseable", body, err)
		}
	}
}
