package sweeprunner

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// capacityConfig is the runner block the capacity tests size their runs from.
func capacityConfig() *config.TierSweepRunnerConfig {
	return &config.TierSweepRunnerConfig{
		CPUs: 8, Memory: 16, Jobs: 4, OutputRoot: "/out", Context: "k3s-sweep",
	}
}

// shortNode is a Ready node whose free room is below the config's run: 1 CPU
// and 2 GiB against the run's 8200m and 16640Mi.
const shortNode = `{"items": [{"kind": "Node", "metadata": {"name": "gt-short"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "1", "memory": "2Gi"}}}]}`

// TestCapacityPreflightFromConfig holds the check to the runner block: the
// amounts it judges, the context it targets, and the blocks it refuses.
func TestCapacityPreflightFromConfig(t *testing.T) {
	t.Parallel()
	t.Run("the amounts and context come from the block", func(t *testing.T) {
		t.Parallel()
		cfg := capacityConfig()
		p, err := CapacityPreflight(cfg, "/town")
		if err != nil {
			t.Fatalf("CapacityPreflight: %v", err)
		}
		if i := slices.Index(p.Step.Argv, "--context"); i < 0 || i+1 >= len(p.Step.Argv) || p.Step.Argv[i+1] != cfg.Context {
			t.Errorf("step argv %q does not name the context %q", p.Step.Argv, cfg.Context)
		}
		// The Job asks for the runner's 8000m/16Gi plus the sync and reader
		// pods' 100m/128Mi each.
		_, err = p.Read([]byte(shortNode))
		var noRoom *NoRoomError
		if !errors.As(err, &noRoom) || !strings.Contains(err.Error(), "job needs 8200m/16640Mi") {
			t.Fatalf("Read = %v, want a decline naming the block's 8200m/16640Mi", err)
		}
	})
	t.Run("a nil config and an unrenderable one are refused", func(t *testing.T) {
		t.Parallel()
		unrenderable := capacityConfig()
		unrenderable.CPUs = 0
		for _, tc := range []struct {
			name string
			cfg  *config.TierSweepRunnerConfig
			want string
		}{
			{name: "nil config", cfg: nil, want: "no tier sweep runner"},
			{name: "cpus 0", cfg: unrenderable, want: "cpus"},
		} {
			p, err := CapacityPreflight(tc.cfg, "/town")
			if err == nil || p != nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: CapacityPreflight = %v, %v, want the error naming %q and no preflight", tc.name, p, err, tc.want)
			}
		}
	})
}

// TestConfigSpecTakesTheRunnerBlock holds ConfigSpec to the block's eight
// fields, the two defaults its methods supply included.
func TestConfigSpecTakesTheRunnerBlock(t *testing.T) {
	t.Parallel()
	cfg := capacityConfig()
	cfg.Dind, cfg.DindCPUs = true, 2
	cfg.Namespace, cfg.ProtectedNamespaces = "sweeps", []string{"forgejo", "registry"}
	want := Spec{CPUs: 8, MemoryGB: 16, Jobs: 4, Dind: true, DindCPUs: 2, Context: "k3s-sweep",
		Namespace: "sweeps", ProtectedNamespaces: []string{"forgejo", "registry"}}
	if got := ConfigSpec(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("ConfigSpec = %+v, want %+v", got, want)
	}
	cfg.Namespace, cfg.ProtectedNamespaces = "", nil
	want.Namespace, want.ProtectedNamespaces = config.DefaultSweepNamespace, []string{"forgejo"}
	if got := ConfigSpec(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("ConfigSpec with the defaults unset = %+v, want %+v", got, want)
	}
	if got := ConfigSpec(nil); !reflect.DeepEqual(got, Spec{}) {
		t.Errorf("ConfigSpec(nil) = %+v, want the zero Spec", got)
	}
}
