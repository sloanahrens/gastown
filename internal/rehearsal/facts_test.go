package rehearsal

import (
	"reflect"
	"testing"
)

const readyNote = "READY TO LAND\nBranch: b\nHead: h\nTarget: main\nWorker: toast\nGate: "

func TestRehearsalFacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		notes    string
		comments []string
		want     []Gate
	}{
		{"passing Gate line gives a pass of its wall", readyNote + "pass presubmit 3m5s, slot wait 0s, slowest: x 1s", nil, []Gate{{Verdict: GatePass, Seconds: 185}}},
		{"fail comment gives a fail of its wall", "", []string{"GATE RUN: fail 2m3s, slot wait 0s, head abc1234"}, []Gate{{Verdict: GateFail, Seconds: 123}}},
		{"unavailable comment gives an unavailable of its wall", "", []string{"GATE RUN: unavailable 7s, slot wait 0s, head abc1234"}, []Gate{{Verdict: GateUnavailable, Seconds: 7}}},
		{"note gate then comments in order", readyNote + "pass presubmit 1m, slot wait 0s", []string{"GATE RUN: fail 2m, slot wait 0s, head a", "GATE RUN: unavailable 0s, slot wait 0s, head b"}, []Gate{{Verdict: GatePass, Seconds: 60}, {Verdict: GateFail, Seconds: 120}, {Verdict: GateUnavailable}}},
		{"another verdict is unavailable with zero seconds", readyNote + "fail presubmit 3m5s, slot wait 0s", nil, []Gate{{Verdict: GateUnavailable}}},
		{"under three fields is unavailable with zero seconds", readyNote + "pass presubmit", nil, []Gate{{Verdict: GateUnavailable}}},
		{"unparseable Gate wall is unavailable with zero seconds", readyNote + "pass presubmit soon, slot wait 0s", nil, []Gate{{Verdict: GateUnavailable}}},
		{"unknown GATE RUN verdict is unavailable with zero seconds", "", []string{"GATE RUN: pass 2m3s, slot wait 0s, head a"}, []Gate{{Verdict: GateUnavailable}}},
		{"fail comment with an unparseable wall keeps fail with zero seconds", "", []string{"GATE RUN: fail soon, slot wait 0s, head a"}, []Gate{{Verdict: GateFail}}},
		{"empty Gate line is unavailable with zero seconds", readyNote, nil, []Gate{{Verdict: GateUnavailable}}},
		{"negative Gate wall is unavailable with zero seconds", readyNote + "pass presubmit -3m, slot wait 0s", nil, []Gate{{Verdict: GateUnavailable}}},
		{"blank GATE RUN rest is unavailable with zero seconds", "", []string{"GATE RUN: "}, []Gate{{Verdict: GateUnavailable}}},
		{"bare fail verdict keeps fail with zero seconds", "", []string{"GATE RUN: fail"}, []Gate{{Verdict: GateFail}}},
		{"negative comment wall keeps fail with zero seconds", "", []string{"GATE RUN: fail -2m, slot wait 0s, head a"}, []Gate{{Verdict: GateFail}}},
		{"no Gate line gives no gate", "READY TO LAND\nBranch: b\nHead: h\nTarget: main\nWorker: toast\n", nil, nil},
		{"comment without the prefix gives no gate", "", []string{"looks good", "the GATE RUN: fail 1m note is old", "GATE RUN:"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := GateFacts(tt.notes, tt.comments); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GateFacts(%q, %q) = %+v, want %+v", tt.notes, tt.comments, got, tt.want)
			}
		})
	}
}
