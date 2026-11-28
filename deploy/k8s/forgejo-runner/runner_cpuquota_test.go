package forgejorunner

import (
	"strings"
	"testing"
)

// TestCPUQuotaWritesTheValue drives Values.CPUQuota into runner.envs CI_CPU_QUOTA:
// a set quota is written as given, and an empty one still derives the whole cores
// of the engine limit.
func TestCPUQuotaWritesTheValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		quota string
		want  string
	}{
		{"a set quota below the limit", "8", "8"},
		{"an empty quota derives the limit", "", "12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues()
			v.DindResources.CPU = "12"
			v.CPUQuota = tc.quota
			got := decodeConfig(t, mustBuild(t, v)).Runner.Envs["CI_CPU_QUOTA"]
			if got != tc.want {
				t.Errorf("runner.envs CI_CPU_QUOTA = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCPUQuotaDecodesFromValuesFile drives the cpuQuota key: a values document that
// names it decodes into Values.CPUQuota, and the quota reaches the rendered config.
func TestCPUQuotaDecodesFromValuesFile(t *testing.T) {
	t.Parallel()
	const doc = `namespace: forgejo
labels:
  - "gate:docker://gate:1"
forgejoHostIP: "10.0.0.7"
dindStorage: 40Gi
runnerStorage: 1Gi
runnerResources:
  cpu: "1"
  memory: 1Gi
dindResources:
  cpu: "12"
  memory: 12Gi
cpuQuota: "8"
`
	v, err := decodeValues("values.control.yaml", []byte(doc))
	if err != nil {
		t.Fatalf("decodeValues: %v", err)
	}
	if v.CPUQuota != "8" {
		t.Errorf("Values.CPUQuota = %q, want %q", v.CPUQuota, "8")
	}
	if got := decodeConfig(t, mustBuild(t, v)).Runner.Envs["CI_CPU_QUOTA"]; got != "8" {
		t.Errorf("runner.envs CI_CPU_QUOTA = %q, want the decoded quota %q", got, "8")
	}
}

// TestCPUQuotaRefusesAnUnusableValue drives the quota validation: a quota that is
// not a positive whole number, or that needs more cores than the engine limit's
// whole cores, is refused and the error names Values.CPUQuota. A limit left empty
// takes DefaultDindCPU, as the render does.
func TestCPUQuotaRefusesAnUnusableValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		limit   string
		quota   string
		refused bool
	}{
		{"zero", "12", "0", true},
		{"a fraction", "12", "2.5", true},
		{"not a number", "12", "abc", true},
		{"above the limit", "12", "13", true},
		{"above the default limit", "", "9", true},
		{"above the limit's whole cores", "6500m", "8", true},
		{"at the limit", "12", "12", false},
		{"at the default limit", "", "8", false},
		{"at the limit's whole cores", "6500m", "7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues()
			v.DindResources.CPU = tc.limit
			v.CPUQuota = tc.quota
			m, err := Build(v)
			if tc.refused {
				if err == nil {
					t.Fatalf("Build accepted CPUQuota %q under limit %q", tc.quota, tc.limit)
				}
				if !strings.Contains(err.Error(), "Values.CPUQuota") {
					t.Errorf("error = %q, want it to name Values.CPUQuota", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build refused CPUQuota %q under limit %q: %v", tc.quota, tc.limit, err)
			}
			if got := decodeConfig(t, m).Runner.Envs["CI_CPU_QUOTA"]; got != tc.quota {
				t.Errorf("runner.envs CI_CPU_QUOTA = %q, want %q", got, tc.quota)
			}
		})
	}
}
