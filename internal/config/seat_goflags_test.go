package config

import (
	"strconv"
	"strings"
	"testing"
)

// seatGoFlagsSettings decodes a town settings file whose polecat_pool seats are
// body, so the tests read go_flags through the same JSON the loader reads.
func seatGoFlagsSettings(t *testing.T, body string) *TownSettings {
	t.Helper()
	raw := `{"type":"town-settings","version":1,"polecat_pool":{"seats":{` + body + `}}}`
	var ts TownSettings
	if err := DecodeJSONFile("town.json", []byte(raw), &ts); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return &ts
}

// A seat's go_flags are a whitelist: the four build-mode flags load, and a flag
// that could retarget the toolchain (or any other) is refused by name at load,
// so a settings file cannot smuggle one past the operator (gt-gg9hv.42).
func TestSeatGoFlagsAreValidatedAtLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goFlags     string
		want        string   // the seat's loaded flags
		wantInError []string // the seat and the flag the refusal must name
	}{
		{"trimpath", "-trimpath", "-trimpath", nil},
		{"mod=readonly", "-mod=readonly", "-mod=readonly", nil},
		{"race", "-race", "-race", nil},
		{"a parallelism", "-p=3", "-p=3", nil},
		{"several flags keep their order", "-trimpath -race", "-trimpath -race", nil},
		{"toolexec", "-toolexec=x", "", []string{"seat-trial", "-toolexec=x"}},
		{"ldflags", "-ldflags=x", "", []string{"seat-trial", "-ldflags=x"}},
		{"gcflags", "-gcflags=x", "", []string{"seat-trial", "-gcflags=x"}},
		{"an empty flag", "-trimpath  -race", "", []string{"seat-trial", "empty flag"}},
		{"no leading dash", "trimpath", "", []string{"seat-trial", "trimpath"}},
		{"a -p with no count", "-p=many", "", []string{"seat-trial", "-p=many"}},
		{"a -p of zero", "-p=0", "", []string{"seat-trial", "-p=0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := seatGoFlagsSettings(t, `"seat-trial":{"cap":1,"go_flags":`+strconv.Quote(tt.goFlags)+`}`)
			seats, err := Seats(ts, nil)
			if len(tt.wantInError) > 0 {
				if err == nil {
					t.Fatalf("go_flags %q loaded as %+v, want a refusal", tt.goFlags, seats)
				}
				for _, want := range tt.wantInError {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to name %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("go_flags %q: %v", tt.goFlags, err)
			}
			if len(seats) != 1 || seats[0].GoFlags != tt.want {
				t.Fatalf("seats = %+v, want one seat with go_flags %q", seats, tt.want)
			}
		})
	}
}

// The seat's go_flags follow every GOFLAGS value the spawn already had and
// precede the -p cap, so an operator's -p keeps winning and a seat with no
// operator value still gains the cap (gt-gg9hv.42).
func TestSeatGoFlagsMergeKeepsTheOperatorCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		agentGOFLAGS string
		process      string
		seatGoFlags  string
		want         string
	}{
		{"an agent -p stays first and wins", "-p=2", "", "-trimpath", "-p=2 -trimpath"},
		{"no agent value: the flags come before the computed cap", "", "", "-trimpath", "-trimpath -p=2"},
		{"an empty go_flags leaves today's value unchanged", "-mod=mod", "", "", "-mod=mod -p=2"},
		{"the spawn environment is the base", "", "-count=1", "-trimpath", "-count=1 -trimpath -p=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			process := tt.process
			env := AgentEnv(AgentEnvConfig{
				Role:         "polecat",
				Rig:          "myrig",
				AgentName:    "Toast",
				TownRoot:     "/town",
				AgentGOFLAGS: tt.agentGOFLAGS,
				SeatGoFlags:  tt.seatGoFlags,
				SeatCPUs:     12,
				SeatCount:    6,
				Getenv:       func(string) string { return process },
			})
			assertEnv(t, env, "GOFLAGS", tt.want)
		})
	}
}
