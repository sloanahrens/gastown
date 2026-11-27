package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// driftMutationOf adds every difference a hook test compares: the daemon
// Deployment's replica count and time zone (two lines on one object), the
// dashboard Deployment's replica count, and the Forgejo Service's external
// name. Each is a field its render names, so the compare reports it.
func driftMutationOf(t *testing.T, live map[string]any) {
	t.Helper()
	switch live["kind"] {
	case "Deployment":
		driftObjectAt(live, "spec")["replicas"] = float64(7)
		if driftObjectAt(live, "metadata")["name"] == k8sdaemon.Name {
			driftEnvOf(t, driftContainer(t, live, k8sdaemon.Name), "TZ")["value"] = "UTC"
		}
	case "Service":
		if driftObjectAt(live, "metadata")["name"] == k8sdaemon.ForgejoName {
			driftObjectAt(live, "spec")["externalName"] = "elsewhere:3000"
		}
	}
}

// TestUpdateRenderDriftCountsDistinctObjects counts three objects from four
// differing lines (two on one object) and prints none of them.
func TestUpdateRenderDriftCountsDistinctObjects(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, func(f *clusterRenderFlags) { f.dashboard = true })
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	d.kubectl = driftAnswering(t, m, driftMutationOf)

	got, err := realRenderDrift(d)(context.Background())
	if err != nil {
		t.Fatalf("hook error = %v, want none; the compare reported differences", err)
	}
	if got != 3 {
		t.Errorf("hook answered %d, want 3: deployment/daemon twice, deployment/dashboard and service/forgejo", got)
	}
	if out.Len() != 0 {
		t.Errorf("the hook printed the compare's lines:\n%s", out)
	}
}

// TestUpdateRenderDriftInStepIsZero answers zero for a cluster the render is in
// step with, which prints no difference.
func TestUpdateRenderDriftInStepIsZero(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	d.kubectl = driftAnswering(t, m, nil)

	got, err := realRenderDrift(d)(context.Background())
	if err != nil {
		t.Fatalf("hook error = %v, want none for a cluster in step", err)
	}
	if got != 0 {
		t.Errorf("hook answered %d, want 0", got)
	}
	if out.Len() != 0 {
		t.Errorf("the hook printed the compare's lines:\n%s", out)
	}
}

// TestUpdateRenderDriftNoRecordedRender marks a missing record with the sentinel
// while the drift command's refusal text stays exactly as it was.
func TestUpdateRenderDriftNoRecordedRender(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	if err := os.Remove(renderFlagsPathIn(town)); err != nil {
		t.Fatalf("removing the recorded flags: %v", err)
	}
	d.kubectl = driftNoRead(t)
	want := fmt.Sprintf("no live render has recorded its flags at %s, so there is no render to compare the live Deployments with; run a live render with --record-flags first", renderFlagsPathIn(town))

	_, err := realRenderDrift(d)(context.Background())
	if !errors.Is(err, update.ErrNoRenderRecord) {
		t.Fatalf("hook error = %v, want one wrapping update.ErrNoRenderRecord", err)
	}
	if err == nil || err.Error() != want {
		t.Errorf("hook error = %q, want the drift command's existing words %q", err, want)
	}
	// The drift command still refuses with the same words and status 2.
	cmdErr := runClusterDrift(context.Background(), d)
	if exit := driftExit(cmdErr); exit != driftExitUnreadable {
		t.Fatalf("drift exit = %d, want %d", exit, driftExitUnreadable)
	}
	if cmdErr.Error() != want {
		t.Errorf("drift refusal = %q, want %q", cmdErr, want)
	}
	if out.Len() != 0 {
		t.Errorf("the hook printed the compare's output:\n%s", out)
	}
}

// TestUpdateRenderDriftOtherErrorUnchanged returns a compare error that is not
// the missing-record refusal as it is, and never marks it with the sentinel.
func TestUpdateRenderDriftOtherErrorUnchanged(t *testing.T) {
	t.Parallel()
	d, out, _ := driftTestSeams(t, nil)
	d.exceptions = nil
	d.kubectl = driftNoRead(t)
	want := "the drift exceptions file and the run's clock are not both wired, so no difference can be judged"

	_, err := realRenderDrift(d)(context.Background())
	if err == nil || err.Error() != want {
		t.Fatalf("hook error = %v, want the compare's own error %q", err, want)
	}
	if errors.Is(err, update.ErrNoRenderRecord) {
		t.Errorf("hook marked an unreadable compare as a missing record")
	}
	if out.Len() != 0 {
		t.Errorf("the hook printed the compare's output:\n%s", out)
	}
}

// TestUpdateRenderDriftSkipsExplained counts only the unexplained object: the
// explained difference is printed as "explained:" and is not drift.
func TestUpdateRenderDriftSkipsExplained(t *testing.T) {
	t.Parallel()
	const tz = "spec.template.spec.containers[name=daemon].env[name=TZ].value"
	d, _, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	d.exceptions = driftExceptionsFor("Deployment/"+m.Deployment.Metadata.Namespace+"/"+k8sdaemon.Name, tz, "2026-12-01")
	// One explained difference on the daemon and one unexplained on the Service,
	// so only the daemon's replica count is left out of the mutation.
	d.kubectl = driftAnswering(t, m, func(t *testing.T, live map[string]any) {
		t.Helper()
		switch live["kind"] {
		case "Deployment":
			if driftObjectAt(live, "metadata")["name"] == k8sdaemon.Name {
				driftEnvOf(t, driftContainer(t, live, k8sdaemon.Name), "TZ")["value"] = "UTC"
			}
		case "Service":
			if driftObjectAt(live, "metadata")["name"] == k8sdaemon.ForgejoName {
				driftObjectAt(live, "spec")["externalName"] = "elsewhere:3000"
			}
		}
	})

	got, err := realRenderDrift(d)(context.Background())
	if err != nil {
		t.Fatalf("hook error = %v, want none", err)
	}
	// The TZ difference is explained; the Forgejo Service's is not. Counting the
	// explained line too would answer 2.
	if got != 1 {
		t.Errorf("hook answered %d, want 1: the service's difference only", got)
	}
}

// TestUpdateRenderDriftNotComparedMarkerIsAnchored counts a difference whose
// own text holds ": not compared: ", which only a marker matched after the
// resource leaves out.
func TestUpdateRenderDriftNotComparedMarkerIsAnchored(t *testing.T) {
	t.Parallel()
	d, _, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	d.kubectl = driftAnswering(t, m, func(t *testing.T, live map[string]any) {
		t.Helper()
		if live["kind"] != "Deployment" || driftObjectAt(live, "metadata")["name"] != k8sdaemon.Name {
			return
		}
		// The Forgejo host argument prints its value (the reviewed args list),
		// unlike the token paths beside it, which hide theirs.
		args := driftContainer(t, live, k8sdaemon.Name)["args"].([]any)
		for i, arg := range args {
			if arg == "--forge-host" {
				args[i] = "--forge-host: not compared: x"
				return
			}
		}
		t.Fatalf("fixture: the daemon container has no --forge-host argument")
	})

	got, err := realRenderDrift(d)(context.Background())
	if err != nil {
		t.Fatalf("hook error = %v, want none", err)
	}
	if got != 1 {
		t.Errorf("hook answered %d, want 1: the daemon's difference counts though its value holds the marker", got)
	}
}

// TestUpdateRenderDriftPinsTheTownRoot pins the compare's town lookup to the
// root the update run found.
func TestUpdateRenderDriftPinsTheTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	got, err := renderDriftDepsAt(town).render.findTown()
	if err != nil {
		t.Fatalf("pinned findTown: %v", err)
	}
	if got != town {
		t.Errorf("findTown = %q, want the run's town %q", got, town)
	}
}

// TestUpdateRenderDriftWiresTheHook gives the run a hook over the town it found:
// realUpdateDeps wires one, and it reads the record under that town, so its
// refusal names the record's path there.
func TestUpdateRenderDriftWiresTheHook(t *testing.T) {
	t.Parallel()
	townRoot := townWithGtSource(t)
	recordPath := renderFlagsPathIn(townRoot)
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
		t.Fatalf("creating the record's directory: %v", err)
	}
	if err := os.WriteFile(recordPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing the unreadable record: %v", err)
	}
	deps, err := realUpdateDeps(townRoot)
	if err != nil {
		t.Fatalf("realUpdateDeps: %v", err)
	}
	if deps.RenderDrift == nil {
		t.Fatal("realUpdateDeps left RenderDrift nil, so the run prints no drift line")
	}
	_, err = deps.RenderDrift(context.Background())
	if err == nil || !strings.Contains(err.Error(), recordPath) {
		t.Fatalf("hook error = %v, want the refused record at %q", err, recordPath)
	}
	if errors.Is(err, update.ErrNoRenderRecord) {
		t.Errorf("a refused record read as a missing one")
	}
}
