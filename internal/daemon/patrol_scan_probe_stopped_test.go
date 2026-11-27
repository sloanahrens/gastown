package daemon

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// probeStoppedHost is a host whose gateway answers one phase question through
// answer, or fails it when answer returns an error, and records each question as
// "gateway|name".
func probeStoppedHost(answer func(gateway, name string) (sandbox.Status, error)) (*patrolScanHost, *[]string) {
	asked := &[]string{}
	h := &patrolScanHost{
		asks: newGatewayAskBudget(2),
		sandboxStatus: func(gateway, name string) (sandbox.Status, error) {
			*asked = append(*asked, gateway+"|"+name)
			return answer(gateway, name)
		},
	}
	return h, asked
}

// A sandbox whose session probe errored is Stopped only when its gateway says
// so, in any case; every other phase, and an empty one, answers false and the
// one ask is spent either way (gt-zqxlmu).
func TestProbeErrorStopped(t *testing.T) {
	t.Parallel()

	rec := session.SandboxRecord{Gateway: "gw-a", Session: "gt-seat"}

	t.Run("the phase decides", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name  string
			phase string
			want  bool
		}{
			{name: "Stopped", phase: "Stopped", want: true},
			{name: "stopped", phase: "stopped", want: true},
			{name: "STOPPED", phase: "STOPPED", want: true},
			{name: "stopped with surrounding space", phase: " stopped\n", want: true},
			{name: "Provisioning", phase: "Provisioning", want: false},
			{name: "Running", phase: "Running", want: false},
			{name: "Error", phase: "Error", want: false},
			{name: "an empty phase", phase: "", want: false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				h, asked := probeStoppedHost(func(string, string) (sandbox.Status, error) {
					return sandbox.Status{Phase: tt.phase}, nil
				})
				left := h.asks.left

				if got := h.probeErrorStopped(rec); got != tt.want {
					t.Errorf("probeErrorStopped(phase %q) = %v, want %v", tt.phase, got, tt.want)
				}
				if want := []string{"gw-a|gt-seat"}; !reflect.DeepEqual(*asked, want) {
					t.Errorf("gateway asked %v, want %v", *asked, want)
				}
				if got, want := h.asks.left, left-1; got != want {
					t.Errorf("asks left = %d, want %d: one call spends one ask", got, want)
				}
				if got, want := h.asks.asked, 1; got != want {
					t.Errorf("asks asked = %d, want %d: one call spends one ask", got, want)
				}
			})
		}
	})

	t.Run("an errored read is not Stopped and still spends the ask", func(t *testing.T) {
		t.Parallel()
		h, asked := probeStoppedHost(func(string, string) (sandbox.Status, error) {
			return sandbox.Status{Phase: "Stopped"}, errors.New("gateway did not answer")
		})
		left := h.asks.left

		if h.probeErrorStopped(rec) {
			t.Error("probeErrorStopped = true for a phase read that errored, want false")
		}
		if want := []string{"gw-a|gt-seat"}; !reflect.DeepEqual(*asked, want) {
			t.Errorf("gateway asked %v, want %v", *asked, want)
		}
		if got, want := h.asks.left, left-1; got != want {
			t.Errorf("asks left = %d, want %d: a failed read spends its ask too", got, want)
		}
		if got, want := h.asks.asked, 1; got != want {
			t.Errorf("asks asked = %d, want %d: a failed read spends its ask too", got, want)
		}
	})
}
