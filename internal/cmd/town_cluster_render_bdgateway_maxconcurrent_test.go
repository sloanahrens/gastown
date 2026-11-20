package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// gatewayCommandFlag is the value the rendered gateway's command gives flag, and
// whether the command names the flag at all.
func gatewayCommandFlag(t *testing.T, out, flag string) (string, bool) {
	t.Helper()
	command := renderedDeployment(t, out, "-deployment-bd-gateway.yaml").Spec.Template.Spec.Containers[0].Command
	for i, arg := range command {
		if arg == flag && i+1 < len(command) {
			return command[i+1], true
		}
	}
	return "", false
}

// TestTownClusterRenderBdGatewayMaxConcurrentWritesTheCap holds the cap a live
// render with a bd image writes into the gateway's arguments: the flag's default
// when no cap is given, the cap the flag names, and zero, which leaves the flag off
// so the gateway's own default applies.
func TestTownClusterRenderBdGatewayMaxConcurrentWritesTheCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cap  int
		// want is the value the gateway's command carries; empty means no flag.
		want string
	}{
		{name: "no flag writes the default 8", cap: defaultBdGatewayMaxConcurrent, want: "8"},
		{name: "the flag writes the cap it names", cap: 48, want: "48"},
		{name: "zero leaves the gateway's own default", cap: 0, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := withBdImage(t, clusterRenderFixture(t))
			f.bdGatewayMaxConcurrent = tc.cap
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			got, named := gatewayCommandFlag(t, f.out, "--max-concurrent")
			if tc.want == "" {
				if named {
					t.Errorf("the gateway's command names --max-concurrent %s, want the flag left off", got)
				}
				return
			}
			if !named || got != tc.want {
				t.Errorf("the gateway's --max-concurrent = %q (named %v), want %q", got, named, tc.want)
			}
		})
	}
}

// TestTownClusterRenderBdGatewayMaxConcurrentDefaultPinned holds the default cap at
// 8, which is what the render writes when no cap is named. Raising it is a
// deliberate edit to the burst the VM-to-host path carries: read bead gt-w0iqhu
// first, because the path admits about 12 simultaneous new connections and each bd
// process opens its own.
func TestTownClusterRenderBdGatewayMaxConcurrentDefaultPinned(t *testing.T) {
	t.Parallel()
	const want = 8
	if defaultBdGatewayMaxConcurrent != want {
		t.Fatalf("defaultBdGatewayMaxConcurrent = %d, want %d; read bead gt-w0iqhu before changing the gateway's default cap", defaultBdGatewayMaxConcurrent, want)
	}
}

// TestTownClusterRenderBdGatewayMaxConcurrentFlagDefault holds the flag's
// registered default, which is what the operator sees in the command's help.
func TestTownClusterRenderBdGatewayMaxConcurrentFlagDefault(t *testing.T) {
	t.Parallel()
	flag := townClusterRenderCmd.Flags().Lookup("bd-gateway-max-concurrent")
	if flag == nil {
		t.Fatal("gt town cluster render has no --bd-gateway-max-concurrent flag")
	}
	if want := strconv.Itoa(defaultBdGatewayMaxConcurrent); flag.DefValue != want {
		t.Errorf("--bd-gateway-max-concurrent default = %q, want %q", flag.DefValue, want)
	}
}

// TestTownClusterRenderBdGatewayMaxConcurrentRefusals holds the negative cap's
// refusal, which names the flag and writes nothing, on a live render and on a
// rehearsal, with or without a bd image.
func TestTownClusterRenderBdGatewayMaxConcurrentRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(t *testing.T) clusterRenderFlags
	}{
		{
			name: "a negative cap on a live render with a bd image",
			build: func(t *testing.T) clusterRenderFlags {
				return withBdImage(t, clusterRenderFixture(t))
			},
		},
		{
			name: "a negative cap on a live render with no bd image",
			build: func(t *testing.T) clusterRenderFlags {
				return clusterRenderFixture(t)
			},
		},
		{
			name: "a negative cap on a rehearsal",
			build: func(t *testing.T) clusterRenderFlags {
				f := withBdImage(t, clusterRenderFixture(t))
				f.rehearsal = true
				f.namespace = "gt-rehearsal"
				f.forgeHost = ""
				return f
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.build(t)
			f.bdGatewayMaxConcurrent = -1
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(t, &out), f)
			requireErrorNames(t, err, "--bd-gateway-max-concurrent", "-1")
			if got := outState(f.out); got != "absent" {
				t.Errorf("the refused render left %s: %s", f.out, got)
			}
		})
	}
}

// TestTownClusterRenderBdGatewayMaxConcurrentRehearsal holds that a rehearsal with
// a bd image renders no gateway, so a cap it is given has no effect and is not
// refused.
func TestTownClusterRenderBdGatewayMaxConcurrentRehearsal(t *testing.T) {
	t.Parallel()
	f := withBdImage(t, clusterRenderFixture(t))
	f.rehearsal = true
	f.namespace = "gt-rehearsal"
	f.forgeHost = ""
	f.bdGatewayMaxConcurrent = 48
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	if objects := renderedBdGatewayObjects(t, f.out); len(objects) != 0 {
		t.Errorf("the rehearsal wrote gateway objects %q, want none", objects)
	}
}

// TestTownClusterRenderBdGatewayMaxConcurrentReplay holds the recorded cap: a
// --use-recorded render takes the cap the last live render recorded, and a cap
// named on the command line wins over it.
func TestTownClusterRenderBdGatewayMaxConcurrentReplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		named func(flag string) bool
		cap   int
		want  string
	}{
		{
			name:  "the recorded cap replays when the flag is not named",
			named: func(flag string) bool { return flag == "out" },
			cap:   32,
			want:  "48",
		},
		{
			name:  "a cap named on the command line wins over the record",
			named: func(flag string) bool { return flag == "out" || flag == "bd-gateway-max-concurrent" },
			cap:   64,
			want:  "64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := clusterRenderTestTown(t)

			recorded := withBdImage(t, clusterRenderFixture(t))
			recorded.bdGatewayMaxConcurrent = 48
			var first bytes.Buffer
			firstDeps := clusterRenderDepsAt(t, town, &first)
			firstDeps.record = clusterRenderRecordOptions{record: true}
			if err := runClusterRender(firstDeps, recorded); err != nil {
				t.Fatalf("recording render: %v", err)
			}

			// The replay names its own output directory, which is new, and the rest
			// of the flags come from the record.
			replay := clusterRenderFlags{out: filepath.Join(t.TempDir(), "out"), bdGatewayMaxConcurrent: tc.cap}
			var second bytes.Buffer
			secondDeps := clusterRenderDepsAt(t, town, &second)
			secondDeps.record = clusterRenderRecordOptions{use: true, named: tc.named}
			if err := runClusterRender(secondDeps, replay); err != nil {
				t.Fatalf("replaying render: %v", err)
			}
			got, named := gatewayCommandFlag(t, replay.out, "--max-concurrent")
			if !named || got != tc.want {
				t.Errorf("the replayed --max-concurrent = %q (named %v), want %q", got, named, tc.want)
			}
		})
	}
}

// TestTownClusterRenderBdGatewayMaxConcurrentRecordedDefault holds what a record
// with no cap key means: a record written before the cap was recorded replays the
// flag's default, not zero, which the gateway would read as its own default of 16.
// A key that holds zero keeps zero.
func TestTownClusterRenderBdGatewayMaxConcurrentRecordedDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cap     int
		dropKey bool
		want    int
	}{
		{name: "a record with no cap key takes the default", cap: 48, dropKey: true, want: defaultBdGatewayMaxConcurrent},
		{name: "a record whose cap is zero keeps zero", cap: 0, want: 0},
		{name: "a record whose cap is set keeps it", cap: 48, want: 48},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "render-flags.json")
			f := renderFlagsFixture()
			f.bdGatewayMaxConcurrent = tc.cap
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
				delete(fields, "bd_gateway_max_concurrent")
				data, err = json.Marshal(fields)
				if err != nil {
					t.Fatalf("encoding the record without the cap: %v", err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("writing the record without the cap: %v", err)
				}
			}
			got, ok, err := loadRenderFlags(path)
			if err != nil {
				t.Fatalf("loadRenderFlags: %v", err)
			}
			if !ok {
				t.Fatal("loadRenderFlags reported no record")
			}
			if got.bdGatewayMaxConcurrent != tc.want {
				t.Errorf("the replayed cap = %d, want %d", got.bdGatewayMaxConcurrent, tc.want)
			}
		})
	}
}
