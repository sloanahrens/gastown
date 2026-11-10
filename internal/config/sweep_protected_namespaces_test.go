package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The sweep's priority preflight protects Forgejo's namespace unless the town
// settings name others: the setting round-trips through settings/config.json,
// an unset list reads as the default, and an empty or malformed list is refused
// naming the setting, never read as "check nothing" (gt-gg9hv.39).
func TestSweepProtectedNamespacesIsReadFromTownSettings(t *testing.T) {
	t.Parallel()

	const runner = `{"backend":"kubernetes","image":"installed","cpus":4,"memory":5,"jobs":4,"output_root":"/out","context":"gt-control"}`

	t.Run("the setting round-trips", func(t *testing.T) {
		t.Parallel()
		doc := `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"runner":` +
			strings.TrimSuffix(runner, "}") + `,"protected_namespaces":["forgejo","forgejo-trial"]}}}}`
		var cfg DaemonPatrolConfig
		if err := DecodeJSONFile("settings.json", []byte(doc), &cfg); err != nil {
			t.Fatalf("settings.json with protected_namespaces = %v", err)
		}
		got := cfg.Patrols.TierSweep.Runner.ProtectedNamespaces
		if want := []string{"forgejo", "forgejo-trial"}; !slices.Equal(got, want) {
			t.Fatalf("protected_namespaces = %v, want %v", got, want)
		}

		encoded, err := json.Marshal(cfg.Patrols.TierSweep.Runner)
		if err != nil {
			t.Fatalf("marshalling the runner block: %v", err)
		}
		var back TierSweepRunnerConfig
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("the runner block does not decode again: %v\n%s", err, encoded)
		}
		if want := []string{"forgejo", "forgejo-trial"}; !slices.Equal(back.ProtectedNamespaces, want) {
			t.Errorf("protected_namespaces decoded again as %v, want %v", back.ProtectedNamespaces, want)
		}
	})

	t.Run("unset reads as the default", func(t *testing.T) {
		t.Parallel()
		var cfg PatrolsConfig
		if err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+runner+`}}`), &cfg); err != nil {
			t.Fatalf("a runner block without protected_namespaces does not decode: %v", err)
		}
		if cfg.TierSweep.Runner.ProtectedNamespaces != nil {
			t.Errorf("protected_namespaces = %v, want it unset so the default applies", cfg.TierSweep.Runner.ProtectedNamespaces)
		}
		if got, want := cfg.TierSweep.Runner.ProtectedNamespaceList(), []string{"forgejo"}; !slices.Equal(got, want) {
			t.Errorf("the preflight's namespaces = %v, want the default %v", got, want)
		}
	})

	for name, tc := range map[string]struct{ block, wantKey string }{
		"an empty list": {strings.TrimSuffix(runner, "}") + `,"protected_namespaces":[]}`, "runner.protected_namespaces"},
		"a namespace that is not a name": {
			strings.TrimSuffix(runner, "}") + `,"protected_namespaces":["Forgejo"]}`, "runner.protected_namespaces",
		},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			var cfg PatrolsConfig
			err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+tc.block+`}}`), &cfg)
			if err == nil {
				t.Fatalf("decoded %s: %+v", name, cfg.TierSweep.Runner)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantKey)
			}
		})
	}
}

// The sweep's namespace is read from tier_sweep.runner.namespace: set, it is
// used as typed; unset, it reads as DefaultSweepNamespace; a value that is not a
// namespace name is refused naming the key (gt-42xdun).
func TestSweepRunnerNamespaceIsReadFromTownSettings(t *testing.T) {
	t.Parallel()

	const base = `{"backend":"kubernetes","image":"installed","cpus":4,"memory":5,"jobs":4,"output_root":"/out","context":"gt-control"`

	var set PatrolsConfig
	if err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+base+`,"namespace":"gt-ci"}}}`), &set); err != nil {
		t.Fatalf("a runner block with a namespace does not decode: %v", err)
	}
	if got := set.TierSweep.Runner.NamespaceName(); got != "gt-ci" {
		t.Errorf("NamespaceName() = %q, want the setting gt-ci", got)
	}

	var unset PatrolsConfig
	if err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+base+`}}}`), &unset); err != nil {
		t.Fatalf("a runner block without a namespace does not decode: %v", err)
	}
	if got := unset.TierSweep.Runner.NamespaceName(); got != "gt-sweeps" {
		t.Errorf("NamespaceName() = %q, want the default gt-sweeps", got)
	}

	var bad PatrolsConfig
	err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+base+`,"namespace":"Not A Name"}}}`), &bad)
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("a malformed namespace gave %v, want a refusal naming namespace", err)
	}
}
