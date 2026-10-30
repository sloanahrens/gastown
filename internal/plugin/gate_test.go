package plugin

import (
	"testing"
	"time"
)

// gateKinds lists every gate type a plugin may declare. A new kind joins this
// list, and TestGateRunLedgerWindowClassifiesEveryKind then fails until the
// kind's ledger behaviour is stated.
var gateKinds = []GateType{GateCooldown, GateCron, GateCondition, GateEvent, GateManual}

// TestGateRunLedgerWindowClassifiesEveryKind pins which gate kinds read the
// run ledger and how far back: only a cooldown gate does, and only through a
// positive duration.
func TestGateRunLedgerWindowClassifiesEveryKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		gate    *Gate
		want    time.Duration
		wantErr bool
	}{
		{"nil gate", nil, 0, false},
		{"cooldown", &Gate{Type: GateCooldown, Duration: "90m"}, 90 * time.Minute, false},
		{"cooldown without a duration", &Gate{Type: GateCooldown}, 0, false},
		{"cooldown with a zero window", &Gate{Type: GateCooldown, Duration: "0"}, 0, false},
		{"cooldown with a negative window", &Gate{Type: GateCooldown, Duration: "-5m"}, 0, false},
		{"cooldown with an unparsable duration", &Gate{Type: GateCooldown, Duration: "soon"}, 0, true},
		{"cron", &Gate{Type: GateCron, Duration: "1h"}, 0, false},
		{"condition", &Gate{Type: GateCondition}, 0, false},
		{"event", &Gate{Type: GateEvent}, 0, false},
		{"manual", &Gate{Type: GateManual}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.gate.RunLedgerWindow()
			if (err != nil) != tc.wantErr {
				t.Fatalf("RunLedgerWindow() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("RunLedgerWindow() = %s, want %s", got, tc.want)
			}
		})
	}

	// Every kind must be classified above; a kind missing from the table has
	// no stated ledger behaviour.
	classified := map[GateType]bool{}
	for _, tc := range cases {
		if tc.gate != nil {
			classified[tc.gate.Type] = true
		}
	}
	for _, kind := range gateKinds {
		if !classified[kind] {
			t.Errorf("gate kind %q has no run-ledger classification: add it to this table", kind)
		}
	}
}
