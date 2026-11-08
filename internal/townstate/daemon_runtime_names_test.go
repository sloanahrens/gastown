package townstate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestDaemonRuntimeNames is the set gt report publish refuses a bird name
// from: the daemon-owned directories directly under .runtime, sorted, as both
// the manifest in Go and its YAML mirror hold them. The session-owned
// .runtime/<bird> row is not in it.
func TestDaemonRuntimeNames(t *testing.T) {
	t.Parallel()
	want := []string{
		"agents", "attention", "bead-respawn-counts.json",
		"bead-respawn-counts.json.flock", "ci-failures.jsonl", "claude-plan",
		"cloud-check", "composer_pending", "container", "daemon-image",
		"daemon-liveness.json", "daemon-respawn.json", "dashboard-load.jsonl",
		"deploy-watch.json", "diskmaint", "gate-logs", "heartbeats", "hm-src",
		"hooks", "host", "jev", "jev-dataset", "landing-logs", "landing-om",
		"landings", "locks", "metrics", "nudge_poller", "nudge_queue", "om-src",
		"openshell", "overrun-guard", "patrol_scan", "pids", "polecat-admission",
		"polecat-image-install.log", "polecat-image.lock.flock",
		"polecat-image.published", "polecat-pool-claims", "polecat-prompts",
		"polecat-spans", "red-main", "rehearsal", "reports", "rigs",
		"sandbox-seat-ends", "sandboxes", "sbx-k8s", "scheduler-dispatch.lock",
		"seat-refill.json", "signing", "sling-context-scan",
		"sling-startup-failures", "spec-lint", "spec_dispatch", "supervisor",
		"sweep-runs", "tier-sweep", "tiertrigger", "town-metrics.lock",
		"townhealth.json", "update", "usage", "watch",
	}

	mirror, err := os.ReadFile(filepath.Join("..", "..", "deploy", "daemon", "town-state.yaml"))
	if err != nil {
		t.Fatalf("reading the mirror: %v", err)
	}
	parsed, err := Parse(mirror)
	if err != nil {
		t.Fatalf("parsing the mirror: %v", err)
	}

	for _, c := range []struct {
		name string
		m    *Manifest
	}{
		{name: "the manifest in Go", m: Default()},
		{name: "the YAML mirror", m: parsed},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.m.DaemonRuntimeNames()
			if !slices.Equal(got, want) {
				t.Fatalf("DaemonRuntimeNames() = %v, want %v", got, want)
			}
			if !slices.IsSorted(got) {
				t.Errorf("DaemonRuntimeNames() = %v, want sorted names", got)
			}
			// Every name is a daemon-owned row directly under .runtime, and no
			// session-owned row is among them.
			owners := map[string]Owner{}
			for _, e := range c.m.Entries {
				name, ok := strings.CutPrefix(e.Path, ".runtime/")
				if !ok || name == "" || strings.Contains(name, "/") {
					continue
				}
				owners[name] = e.Owner
			}
			for _, name := range got {
				if owners[name] != OwnerDaemon {
					t.Errorf("%q is in DaemonRuntimeNames() but its manifest row is %q", name, owners[name])
				}
			}
			if _, ok := owners["<bird>"]; !ok || owners["<bird>"] != OwnerSession {
				t.Errorf("the manifest's bird row is %q, want a session-owned .runtime/<bird>", owners["<bird>"])
			}
		})
	}
}
