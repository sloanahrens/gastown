//go:build integration

package cmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestIntegrationSchedulerInitArgsKeepPortArgv(t *testing.T) {
	base := []string{"init", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents", "--prefix", "gt"}
	tests := []struct {
		name string
		port string
		want []string
	}{
		{"no port keeps the bare argv", "", base},
		{"a port adds the external server flags", "41234", append(append([]string{}, base...), "--server", "--external", "--server-port", "41234")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := schedulerInitArgs("gt", tt.port); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("schedulerInitArgs(gt, %q) = %q, want %q", tt.port, got, tt.want)
			}
		})
	}
}

// TestIntegrationDoltInitRefusesWithoutPort pins the guard the init helpers
// run before any git or bd work: with no port variable set it refuses, and
// any one of the three set lets the init proceed.
func TestIntegrationDoltInitRefusesWithoutPort(t *testing.T) {
	keys := []string{"GT_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

	t.Run("no port set refuses", func(t *testing.T) {
		for _, key := range keys {
			t.Setenv(key, "")
		}
		err := doltInitPortError()
		if err == nil || !strings.Contains(err.Error(), "refusing bd init") {
			t.Fatalf("doltInitPortError() = %v, want a refusal", err)
		}
	})

	for _, key := range keys {
		t.Run(key+" set allows", func(t *testing.T) {
			for _, other := range keys {
				t.Setenv(other, "")
			}
			t.Setenv(key, "41234")
			if err := doltInitPortError(); err != nil {
				t.Fatalf("doltInitPortError() with %s set = %v, want nil", key, err)
			}
		})
	}
}
