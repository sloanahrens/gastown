package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestTownClusterRenderBdGatewayReplicasWritesTheCount holds the count a live render
// with a bd image writes into the gateway's Deployment: two renders two pods, and one,
// or zero, which reads as one, renders one.
func TestTownClusterRenderBdGatewayReplicasWritesTheCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		replicas int
		want     int32
	}{
		{name: "the flag's default renders one pod", replicas: defaultBdGatewayReplicas, want: 1},
		{name: "zero reads as one pod", replicas: 0, want: 1},
		{name: "two renders two pods", replicas: 2, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := withBdImage(t, clusterRenderFixture(t))
			f.bdGatewayReplicas = tc.replicas
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			if got := renderedDeployment(t, f.out, "-deployment-bd-gateway.yaml").Spec.Replicas; got != tc.want {
				t.Errorf("the gateway's replicas = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTownClusterRenderBdGatewayReplicasFlagDefault holds the flag's registered default,
// which is what the operator sees in the command's help: one pod.
func TestTownClusterRenderBdGatewayReplicasFlagDefault(t *testing.T) {
	t.Parallel()
	flag := townClusterRenderCmd.Flags().Lookup("bd-gateway-replicas")
	if flag == nil {
		t.Fatal("gt town cluster render has no --bd-gateway-replicas flag")
	}
	if want := strconv.Itoa(defaultBdGatewayReplicas); flag.DefValue != want {
		t.Errorf("--bd-gateway-replicas default = %q, want %q", flag.DefValue, want)
	}
}

// TestTownClusterRenderBdGatewayReplicasRefusals holds a count below zero or above two
// to a refusal that names the flag and writes nothing, on a live render with or
// without a bd image, and on a rehearsal, which renders no gateway but still refuses.
func TestTownClusterRenderBdGatewayReplicasRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(t *testing.T) clusterRenderFlags
		count int
	}{
		{
			name:  "three on a live render with a bd image",
			build: func(t *testing.T) clusterRenderFlags { return withBdImage(t, clusterRenderFixture(t)) },
			count: 3,
		},
		{
			name:  "negative on a live render with a bd image",
			build: func(t *testing.T) clusterRenderFlags { return withBdImage(t, clusterRenderFixture(t)) },
			count: -1,
		},
		{
			name:  "three on a live render with no bd image",
			build: func(t *testing.T) clusterRenderFlags { return clusterRenderFixture(t) },
			count: 3,
		},
		{
			name: "three on a rehearsal",
			build: func(t *testing.T) clusterRenderFlags {
				f := withBdImage(t, clusterRenderFixture(t))
				f.rehearsal = true
				f.namespace = "gt-rehearsal"
				f.forgeHost = ""
				return f
			},
			count: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.build(t)
			f.bdGatewayReplicas = tc.count
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(t, &out), f)
			requireErrorNames(t, err, "--bd-gateway-replicas", strconv.Itoa(tc.count))
			if got := outState(f.out); got != "absent" {
				t.Errorf("the refused render left %s: %s", f.out, got)
			}
		})
	}
}

// TestTownClusterRenderBdGatewayReplicasReplay holds the recorded count: a --use-recorded
// render takes the count the last live render recorded, and a count named on the
// command line wins over it.
func TestTownClusterRenderBdGatewayReplicasReplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		named    func(flag string) bool
		replicas int
		want     int32
	}{
		{
			name:     "the recorded count replays when the flag is not named",
			named:    func(flag string) bool { return flag == "out" },
			replicas: 1,
			want:     2,
		},
		{
			name:     "a count named on the command line wins over the record",
			named:    func(flag string) bool { return flag == "out" || flag == "bd-gateway-replicas" },
			replicas: 1,
			want:     1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := clusterRenderTestTown(t)

			recorded := withBdImage(t, clusterRenderFixture(t))
			recorded.bdGatewayReplicas = 2
			var first bytes.Buffer
			firstDeps := clusterRenderDepsAt(t, town, &first)
			firstDeps.record = clusterRenderRecordOptions{record: true}
			if err := runClusterRender(firstDeps, recorded); err != nil {
				t.Fatalf("recording render: %v", err)
			}

			// The replay names its own output directory, which is new, and the rest
			// of the flags come from the record unless the command line named them.
			replay := clusterRenderFlags{out: filepath.Join(t.TempDir(), "out"), bdGatewayReplicas: tc.replicas}
			var second bytes.Buffer
			secondDeps := clusterRenderDepsAt(t, town, &second)
			secondDeps.record = clusterRenderRecordOptions{use: true, named: tc.named}
			if err := runClusterRender(secondDeps, replay); err != nil {
				t.Fatalf("replaying render: %v", err)
			}
			if got := renderedDeployment(t, replay.out, "-deployment-bd-gateway.yaml").Spec.Replicas; got != tc.want {
				t.Errorf("the replayed gateway's replicas = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTownClusterRenderBdGatewayReplicasRecordedDefault holds what a record with no
// replica key means: a record written before the count was recorded replays the flag's
// default of one, not zero. A key that holds two keeps two.
func TestTownClusterRenderBdGatewayReplicasRecordedDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		count   int
		dropKey bool
		want    int
	}{
		{name: "a record with no replica key takes the default", count: 2, dropKey: true, want: defaultBdGatewayReplicas},
		{name: "a record whose count is two keeps it", count: 2, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "render-flags.json")
			f := renderFlagsFixture()
			f.bdGatewayReplicas = tc.count
			if err := saveRenderFlags(path, f); err != nil {
				t.Fatalf("saveRenderFlags: %v", err)
			}
			if tc.dropKey {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading the record: %v", err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatalf("decoding the record: %v", err)
				}
				delete(fields, "bd_gateway_replicas")
				data, err = json.Marshal(fields)
				if err != nil {
					t.Fatalf("encoding the record without the count: %v", err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("writing the record without the count: %v", err)
				}
			}
			got, ok, err := loadRenderFlags(path)
			if err != nil {
				t.Fatalf("loadRenderFlags: %v", err)
			}
			if !ok {
				t.Fatal("loadRenderFlags reported no record")
			}
			if got.bdGatewayReplicas != tc.want {
				t.Errorf("the replayed replica count = %d, want %d", got.bdGatewayReplicas, tc.want)
			}
		})
	}
}
