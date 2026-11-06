package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

// diskMaintControlContext is the context tier_sweep.runner names in these tests.
const diskMaintControlContext = "gt-ctl"

// controlKubectl answers the kubectl calls the patrol makes against the control
// cluster from canned output: the node listing and each node's kubelet summary.
// It records every argv, so a test can check the flags a read carried.
type controlKubectl struct {
	nodes     string
	nodesErr  error
	summaries map[string]string
	errs      map[string]error
	calls     [][]string
}

// install routes the daemon's command seam to the fake. Any other program fails
// the call, so a read the test did not expect is an unreadable disk, not a pass.
func (k *controlKubectl) install(d *Daemon) {
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		args := cmd.Args
		if len(args) == 0 || args[0] != "kubectl" {
			return nil, []byte("unexpected command"), fmt.Errorf("unexpected command %q", args)
		}
		k.calls = append(k.calls, args)
		if slices.Contains(args, "nodes") {
			return []byte(k.nodes), nil, k.nodesErr
		}
		i := slices.Index(args, "--raw")
		if i < 0 || i+1 >= len(args) {
			return nil, []byte("unexpected kubectl"), fmt.Errorf("unexpected kubectl %q", args)
		}
		node := strings.TrimSuffix(strings.TrimPrefix(args[i+1], "/api/v1/nodes/"), "/proxy/stats/summary")
		if err := k.errs[node]; err != nil {
			return nil, []byte(err.Error()), err
		}
		summary, ok := k.summaries[node]
		if !ok {
			return nil, []byte("no summary"), fmt.Errorf("no summary for node %s", node)
		}
		return []byte(summary), nil, nil
	}
}

// nodeReads returns the calls that list the control cluster's nodes or read a
// node's summary, which are the calls the control disk makes.
func (k *controlKubectl) nodeReads() [][]string {
	var reads [][]string
	for _, args := range k.calls {
		if slices.Contains(args, "nodes") || slices.Contains(args, "--raw") {
			reads = append(reads, args)
		}
	}
	return reads
}

// controlSummary is a kubelet stats summary for a node of the given used and
// capacity bytes, with no pods: the consumer list of a node with none.
func controlSummary(used, capacity int64) string {
	return fmt.Sprintf(`{"node":{"fs":{"capacityBytes":%d,"usedBytes":%d}},"pods":[]}`, capacity, used)
}

// diskMaintControlConfig is a patrol config whose tier_sweep runner names the
// control cluster's context on the kubernetes backend.
func diskMaintControlConfig(kubeContext string) *DaemonPatrolConfig {
	cfg := diskMaintEnabled("", "")
	cfg.Patrols.TierSweep = &agentconfig.TierSweepConfig{
		Runner: &agentconfig.TierSweepRunnerConfig{Backend: agentconfig.TierSweepRunnerKubernetes, Context: kubeContext},
	}
	return cfg
}

// diskMaintControlDaemon is a patrol whose production read runs the control
// disk through k. The town's other disks read healthy, so any alert is the
// control disk's.
func diskMaintControlDaemon(t *testing.T, cfg *DaemonPatrolConfig, k *controlKubectl) (*Daemon, *bytes.Buffer, *diskMaintFakes) {
	t.Helper()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = cfg
	d.diskMaint.readDisks = nil // the production read, with the control kubectl below
	healthy := func(context.Context) ([]byte, error) { return []byte(diskMaintHealthyDF), nil }
	// The sandbox disk parses kubelet node stats, not df output.
	sandbox := func(context.Context) ([]byte, error) { return []byte(controlSummary(10000, 160000)), nil }
	d.diskMaint.dfReaders = map[string]DFReader{"mac": healthy, "docker-desktop": healthy, "colima": sandbox}
	k.install(d)
	return d, logs, f
}

// readingFor returns the control disk's reading from the production read.
func readingFor(t *testing.T, d *Daemon, name string) disks.Reading {
	t.Helper()
	for _, r := range d.diskMaintenanceSeams().readDisks(context.Background()) {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no reading named %s", name)
	return disks.Reading{}
}

// TestDiskMaintControlDiskReadsTheWorstNode: the control disk is the busiest
// node's kubelet summary, read through the control context; a node whose summary
// cannot be read makes the reading an error naming it, never a lower number.
func TestDiskMaintControlDiskReadsTheWorstNode(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		k       controlKubectl
		wantPct float64
		wantErr string
	}{
		"the busier node wins": {
			k: controlKubectl{
				nodes:     "node/gt-ctl-1\nnode/gt-ctl-2\n",
				summaries: map[string]string{"gt-ctl-1": controlSummary(80000, 160000), "gt-ctl-2": controlSummary(120000, 160000)},
			},
			wantPct: 75,
		},
		"a node whose summary cannot be read is named": {
			k: controlKubectl{
				nodes:     "node/gt-ctl-1\nnode/gt-ctl-2\n",
				summaries: map[string]string{"gt-ctl-1": controlSummary(120000, 160000)},
				errs:      map[string]error{"gt-ctl-2": errors.New("the connection to the server was refused")},
			},
			wantErr: "gt-ctl-2",
		},
		"a node whose summary has no filesystem is named": {
			k: controlKubectl{
				nodes:     "node/gt-ctl-1\n",
				summaries: map[string]string{"gt-ctl-1": `{"pods":[]}`},
			},
			wantErr: "gt-ctl-1",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, _, _ := diskMaintControlDaemon(t, diskMaintControlConfig(diskMaintControlContext), &tc.k)

			got := readingFor(t, d, "control")

			if tc.wantErr != "" {
				if got.Err == nil || !strings.Contains(got.Err.Error(), tc.wantErr) {
					t.Fatalf("control reading = %+v, want an error naming %s", got, tc.wantErr)
				}
			} else if got.Err != nil || got.UsedPct != tc.wantPct {
				t.Fatalf("control reading = %+v, want %.0f%%", got, tc.wantPct)
			}
			reads := tc.k.nodeReads()
			if len(reads) == 0 {
				t.Fatal("the control disk ran no kubectl")
			}
			wantKubeconfig := filepath.Join(d.config.TownRoot, ".runtime", "sbx-k8s", "kubeconfig")
			for _, args := range reads {
				if len(args) < 5 || args[1] != "--kubeconfig" || args[2] != wantKubeconfig ||
					args[3] != "--context" || args[4] != diskMaintControlContext {
					t.Errorf("kubectl ran %q, want --kubeconfig %s --context %s", args, wantKubeconfig, diskMaintControlContext)
				}
			}
		})
	}
}

// TestDiskMaintControlDiskAlertNamesTheConsumers: at 81% the control alert names
// the three biggest consumers by name and size, a claim-backed pod included; it
// clears below 70%; and a cluster name with newlines or 5,000 characters reaches
// the alert as one bounded line.
func TestDiskMaintControlDiskAlertNamesTheConsumers(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile("../diskmaint/disks/testdata/control-summary.json")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the golden names the three biggest and clears below seventy", func(t *testing.T) {
		t.Parallel()
		k := &controlKubectl{nodes: "node/gt-ctl-1\n", summaries: map[string]string{"gt-ctl-1": string(golden)}}
		d, _, f := diskMaintControlDaemon(t, diskMaintControlConfig(diskMaintControlContext), k)

		d.runDiskMaintenance()

		raised := f.notify.Escalations()
		if len(raised) != 1 || raised[0].Escalation.Fingerprint != "diskmaint:control" {
			t.Fatalf("escalations = %+v, want one diskmaint:control raise", raised)
		}
		const consumers = "biggest consumers: forgejo/forgejo-runner-6c8f9 61 GB, forgejo/forgejo-0 12 GB, forgejo-trial/forgejo-trial-0 9 GB"
		if desc := raised[0].Escalation.Description; !strings.Contains(desc, "81% used") || !strings.Contains(desc, consumers) {
			t.Fatalf("description = %q, want 81%% and %q", desc, consumers)
		}

		diskMaintNextDay(d, 1)
		k.summaries["gt-ctl-1"] = controlSummary(65000000000, 100000000000)
		d.runDiskMaintenance()

		clears := f.notify.Clears()
		if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, "diskmaint:control") {
			t.Fatalf("clears = %+v, want diskmaint:control cleared at 65%%", clears)
		}
	})

	t.Run("a hostile cluster name reads as one bounded line", func(t *testing.T) {
		t.Parallel()
		hostile, err := json.Marshal(map[string]any{
			"node": map[string]any{"fs": map[string]any{"capacityBytes": 100000000000, "usedBytes": 81000000000}},
			"pods": []any{map[string]any{
				"podRef":            map[string]string{"name": "evil\nname" + strings.Repeat("b", 5000), "namespace": "ns"},
				"ephemeral-storage": map[string]string{"usedBytes": "61000000000"},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		k := &controlKubectl{nodes: "node/gt-ctl-1\n", summaries: map[string]string{"gt-ctl-1": string(hostile)}}
		d, _, f := diskMaintControlDaemon(t, diskMaintControlConfig(diskMaintControlContext), k)

		d.runDiskMaintenance()

		raised := f.notify.Escalations()
		if len(raised) != 1 {
			t.Fatalf("escalations = %+v, want one raise", raised)
		}
		desc := raised[0].Escalation.Description
		if strings.Contains(desc, "\n") || !strings.Contains(desc, "ns/evil name") {
			t.Fatalf("description = %q, want the name on one line", desc)
		}
		for _, word := range strings.Fields(desc) {
			if n := utf8.RuneCountInString(word); n > 200 {
				t.Fatalf("a word of the description is %d runes, want at most 200", n)
			}
		}
	})
}

// TestDiskMaintControlDiskIsAlertOnly: an unreadable control disk and a full one
// change no keep and block no target, while the same full reading on the sandbox
// disk still tightens the keep.
func TestDiskMaintControlDiskIsAlertOnly(t *testing.T) {
	t.Parallel()
	full := controlSummary(152000, 160000) // 95%
	cases := map[string]struct {
		k        controlKubectl
		sandbox  string // the colima reading, when injected
		wantKeep int
	}{
		"an unreadable control disk": {
			k:        controlKubectl{nodesErr: errors.New("the connection to the server was refused")},
			wantKeep: diskMaintDefaultKeep,
		},
		"a full control disk": {
			k: controlKubectl{
				nodes:     "node/gt-ctl-1\n",
				summaries: map[string]string{"gt-ctl-1": full},
			},
			wantKeep: diskMaintDefaultKeep,
		},
		"the same full reading on the sandbox disk": {
			k:        controlKubectl{nodesErr: errors.New("the connection to the server was refused")},
			sandbox:  full,
			wantKeep: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintControlDaemon(t, diskMaintControlConfig(diskMaintControlContext), &tc.k)
			if tc.sandbox != "" {
				d.diskMaint.dfReaders["colima"] = func(context.Context) ([]byte, error) { return []byte(tc.sandbox), nil }
			}

			d.runDiskMaintenance()

			if len(f.target.policies) != 1 {
				t.Fatalf("planned %d times, want 1: the control disk must not block a target", len(f.target.policies))
			}
			if got := f.target.policies[0].Keep; got != tc.wantKeep {
				t.Fatalf("keep = %d, want %d", got, tc.wantKeep)
			}
		})
	}
}

// TestDiskMaintControlDiskNeedsAContext: with no control context configured
// nothing is read, one line is logged over any number of passes, and no
// unreadable alert opens; with one configured, a failing read opens the ordinary
// unreadable alert.
func TestDiskMaintControlDiskNeedsAContext(t *testing.T) {
	t.Parallel()
	const unread = "control disk not read"
	cases := map[string]struct {
		cfg *DaemonPatrolConfig
	}{
		"no tier_sweep block": {
			cfg: diskMaintEnabled("", ""),
		},
		"an empty context": {
			cfg: diskMaintControlConfig(""),
		},
		"a docker backend": {
			cfg: func() *DaemonPatrolConfig {
				cfg := diskMaintControlConfig("desktop-linux")
				cfg.Patrols.TierSweep.Runner.Backend = agentconfig.TierSweepRunnerDocker
				return cfg
			}(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := &controlKubectl{nodesErr: errors.New("must not be read")}
			d, logs, f := diskMaintControlDaemon(t, tc.cfg, k)

			d.runDiskMaintenance()
			diskMaintNextDay(d, 1)
			d.runDiskMaintenance()

			if reads := k.nodeReads(); len(reads) != 0 {
				t.Fatalf("control kubectl ran %q, want no read", reads)
			}
			if got := strings.Count(logs.String(), unread); got != 1 {
				t.Fatalf("%q logged %d times over two passes, want once:\n%s", unread, got, logs.String())
			}
			if diskMaintAlertIsOpen(f.notify, "diskmaint:control:unreadable") {
				t.Fatal("an unreadable alert opened for a disk that was never read")
			}
		})
	}

	t.Run("a configured context whose read fails opens the unreadable alert", func(t *testing.T) {
		t.Parallel()
		k := &controlKubectl{nodesErr: errors.New("the connection to the server was refused")}
		d, _, f := diskMaintControlDaemon(t, diskMaintControlConfig(diskMaintControlContext), k)

		d.runDiskMaintenance()

		if !diskMaintAlertIsOpen(f.notify, "diskmaint:control:unreadable") {
			t.Fatalf("escalations = %+v, want an open diskmaint:control:unreadable alert", f.notify.Escalations())
		}
	})
}
