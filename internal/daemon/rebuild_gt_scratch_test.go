package daemon

import (
	"bytes"
	"fmt"
	"log"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/scratchtown"
	"github.com/steveyegge/gastown/internal/slot"
)

// listedContainers is a slot.ContainerRuntime that answers `docker ps` with the
// lines a test names, or with an error when the listing could not be taken. The
// quiet check's classification is driven through the real gate over this
// runtime, so the fixture is what docker would print rather than a report a
// test built by hand (gt-4gixm).
type listedContainers struct {
	lines []string
	err   error
}

func (r listedContainers) List() ([]string, error) { return r.lines, r.err }

func (r listedContainers) Remove(string, slot.RemoveOptions) error { return nil }

func (r listedContainers) Info() (slot.VMInfo, error) { return slot.VMInfo{}, nil }

// gateContainerLine renders one container the way `docker ps --format {{json .}}`
// does, labels and state included: the quiet check judges a container on its
// name, its compose labels, its image and its state, so a fixture with an image
// and a name alone cannot drive that judgement.
func gateContainerLine(id, image, name string, created time.Time, labels map[string]string, state string) string {
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return fmt.Sprintf(`{"ID":%q,"Image":%q,"Names":%q,"CreatedAt":%q,"Labels":%q,"State":%q}`,
		id, image, name, created.Format(time.RFC3339Nano), strings.Join(pairs, ","), state)
}

// scratchComposeLabels is the label set the project's compose puts on its
// containers (docs/daemon-container.md).
func scratchComposeLabels(parent string) map[string]string {
	return map[string]string{
		scratchtown.ProjectLabel:     scratchtown.Project,
		scratchtown.ConfigFilesLabel: parent + "/l1/compose.yml",
	}
}

// scratchQuietDaemon is a Daemon whose quiet check reads one gate over the
// given docker listing, and the buffer its log lines land in. The scratch
// parent is the test's own, so nothing here reads a developer's home directory.
func scratchQuietDaemon(t *testing.T, parent string, lines []string, listErr error) (*Daemon, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	gate := slot.NewGate(
		slot.WithRuntime(listedContainers{lines: lines, err: listErr}),
		slot.WithScratchIdentity(slot.ScratchIdentity{Parent: parent}),
	)
	var logs bytes.Buffer
	d := &Daemon{
		config:   &Config{TownRoot: town},
		logger:   log.New(&logs, "", 0),
		slotGate: gate,
	}
	return d, &logs
}

// TestRebuildGTGate_ScratchProjectIsANamedKnownHolder: the quiet check names
// the documented scratch project's containers as a known holder it proceeds
// past, keeps blocking for a container that only looks like the project's, and
// refuses while the project is mid-work — an image build and a Docker restart
// are what the quiet check protects (gt-4gixm).
func TestRebuildGTGate_ScratchProjectIsANamedKnownHolder(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	labels := scratchComposeLabels(parent)
	now := time.Now()

	cases := []struct {
		name     string
		lines    []string
		listErr  error
		wantBusy string // "" means the check proceeds
		wantLog  string // a line the log must carry verbatim
	}{
		{
			name:    "the project's dolt server running",
			lines:   []string{gateContainerLine("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer, now.Add(-40*time.Minute), labels, "running")},
			wantLog: "rebuild_gt: proceeding despite the documented scratch project's containers: dolthub/dolt-sql-server:2.3.2 " + scratchtown.DoltContainer + " (a known holder, not an unwrapped suite)",
		},
		{
			name:    "the project's daemon running",
			lines:   []string{gateContainerLine("c-1", "gt-daemon:4f2a1c9", scratchtown.DaemonContainer, now.Add(-40*time.Minute), labels, "running")},
			wantLog: "rebuild_gt: proceeding despite the documented scratch project's containers: gt-daemon:4f2a1c9 " + scratchtown.DaemonContainer + " (a known holder, not an unwrapped suite)",
		},
		{
			name:     "the project's dolt server created",
			lines:    []string{gateContainerLine("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer, now.Add(-2*time.Minute), labels, "created")},
			wantBusy: "the documented scratch project is being built or restarted: dolthub/dolt-sql-server:2.3.2 " + scratchtown.DoltContainer + " (an image build or a Docker restart is what this check protects)",
		},
		{
			name:     "the project's dolt server restarting",
			lines:    []string{gateContainerLine("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer, now.Add(-2*time.Minute), labels, "restarting")},
			wantBusy: "the documented scratch project is being built or restarted: dolthub/dolt-sql-server:2.3.2 " + scratchtown.DoltContainer + " (an image build or a Docker restart is what this check protects)",
		},
		{
			name:     "a build for the project in flight",
			lines:    []string{gateContainerLine("c-1", "moby/buildkit:buildx-stable-1", "buildx_buildkit_gt-scratch0", now.Add(-2*time.Minute), nil, "running")},
			wantBusy: "the documented scratch project is being built or restarted: moby/buildkit:buildx-stable-1 buildx_buildkit_gt-scratch0 (an image build or a Docker restart is what this check protects)",
		},
		{
			name:     "a name spoof",
			lines:    []string{gateContainerLine("c-1", "dolthub/dolt-sql-server:2.3.2", "gt-scratch-evil-1", now.Add(-2*time.Minute), labels, "running")},
			wantBusy: "container(s) are running outside the gate: dolthub/dolt-sql-server:2.3.2 gt-scratch-evil-1",
		},
		{
			name:     "an unknown dolt container",
			lines:    []string{gateContainerLine("c-1", "dolthub/dolt-sql-server:2.3.2", "someone-elses-dolt", now.Add(-2*time.Minute), nil, "running")},
			wantBusy: "container(s) are running outside the gate: dolthub/dolt-sql-server:2.3.2 someone-elses-dolt",
		},
		{
			name:     "a ryuk reaper",
			lines:    []string{gateContainerLine("c-1", "testcontainers/ryuk:0.5.1", "reaper", now.Add(-2*time.Minute), nil, "running")},
			wantBusy: "container(s) are running outside the gate: testcontainers/ryuk:0.5.1 reaper",
		},
		{
			// A holder and something else blocking: the holder is still named,
			// so a reader can tell what was on the VM either way (gt-4gixm).
			name: "the project's container beside an unknown one",
			lines: []string{
				gateContainerLine("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer, now.Add(-40*time.Minute), labels, "running"),
				gateContainerLine("c-2", "dolthub/dolt-sql-server:2.3.2", "someone-elses-dolt", now.Add(-2*time.Minute), nil, "running"),
			},
			wantLog:  "rebuild_gt: proceeding despite the documented scratch project's containers: dolthub/dolt-sql-server:2.3.2 " + scratchtown.DoltContainer + " (a known holder, not an unwrapped suite)",
			wantBusy: "container(s) are running outside the gate: dolthub/dolt-sql-server:2.3.2 someone-elses-dolt",
		},
		{
			// Docker is installed but its socket cannot be read: the VM is
			// unread, not empty, and an install must not walk into it.
			name:     "docker unreadable",
			listErr:  fmt.Errorf("docker ps failed: permission denied while trying to connect to the Docker daemon socket"),
			wantBusy: "the docker container listing could not be read, so the VM's containers are unknown",
			wantLog:  "rebuild_gt: not quiet: the docker container listing could not be read (",
		},
		{
			// Docker itself says it cannot reach its daemon: nothing can be
			// running, which is the one failed read that still proceeds.
			name:    "docker daemon unreachable",
			listErr: fmt.Errorf("docker ps failed: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"),
			wantLog: "rebuild_gt: treating the town as quiet: docker's daemon refused the connection (",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, logs := scratchQuietDaemon(t, parent, tc.lines, tc.listErr)
			busy, reservable := d.rebuildGTGate()
			out := logs.String()

			if busy != tc.wantBusy {
				t.Fatalf("busy = %q, want %q", busy, tc.wantBusy)
			}
			if reservable {
				t.Errorf("reservable = true for %q; want the install to wait, not take a slot", busy)
			}
			if tc.wantLog != "" && !strings.Contains(out, tc.wantLog) {
				t.Errorf("log = %q, want it to carry %q", out, tc.wantLog)
			}
			if tc.wantBusy == "" && strings.Contains(out, "not quiet:") {
				t.Errorf("log = %q, want no refusal for a quiet town", out)
			}
			// The holder line is written on every path that read the gate, and
			// only when the project's containers were there to name (gt-4gixm).
			if holderNamed := strings.Contains(tc.wantLog, "proceeding despite"); !holderNamed &&
				strings.Contains(out, "proceeding despite") {
				t.Errorf("log = %q, want no holder named when the project has no container up", out)
			}
		})
	}
}
