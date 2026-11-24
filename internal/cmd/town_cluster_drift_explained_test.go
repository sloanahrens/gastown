package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

func driftExceptionsFor(object, path, expires string) func() []byte {
	return func() []byte {
		return []byte(driftExceptionsYAML(driftExceptionEntryYAML(object, path, "explained for the test", "overseer", expires)))
	}
}

// TestTownClusterDriftExplained holds the exceptions file through the compare.
func TestTownClusterDriftExplained(t *testing.T) {
	t.Parallel()
	const tz = "spec.template.spec.containers[name=daemon].env[name=TZ].value"
	d, out, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	daemon := "Deployment/" + m.Deployment.Metadata.Namespace + "/" + k8sdaemon.Name

	t.Run("an explained difference exits 0 and the fake runner reads only with get", func(t *testing.T) {
		d.kubectl = driftAnswering(t, m, func(t *testing.T, live map[string]any) {
			if live["kind"] == "Deployment" {
				driftEnvOf(t, driftContainer(t, live, k8sdaemon.Name), "TZ")["value"] = "UTC"
			}
		})
		d.exceptions = driftExceptionsFor(daemon, tz, "2026-12-01")
		if got := driftExit(runClusterDrift(context.Background(), d)); got != 0 {
			t.Fatalf("exit = %d, want 0; printed:\n%s", got, out.String())
		}
		want := "deployment/" + k8sdaemon.Name + ": explained: " + driftWantDifference(t, tz, "America/Chicago", "UTC")
		if !strings.Contains(out.String(), want) {
			t.Errorf("printed\n%s, want %q", out, want)
		}
	})
	t.Run("an absent object stays drift", func(t *testing.T) {
		out.Reset()
		answer := driftAnswering(t, m, nil)
		d.kubectl = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
			if len(args) > 1 && args[1] == "priorityclass/gt-control-plane" {
				return "", 0, nil
			}
			return answer(ctx, dir, env, name, args...)
		}
		d.exceptions = driftExceptionsFor("PriorityClass//gt-control-plane", "kind", "2026-12-01")
		if got := driftExit(runClusterDrift(context.Background(), d)); got != 1 {
			t.Fatalf("exit = %d, want 1; printed:\n%s", got, out.String())
		}
		if slices.ContainsFunc(driftLines(out), func(l string) bool { return strings.Contains(l, ": explained:") }) {
			t.Errorf("printed an explained line for an absent object:\n%s", out)
		}
	})
	t.Run("an expired entry exits 2 naming it", func(t *testing.T) {
		out.Reset()
		d.kubectl = driftNoRead(t)
		d.exceptions = driftExceptionsFor(daemon, "spec.replicas", "2026-11-18")
		err := runClusterDrift(context.Background(), d)
		if got := driftExit(err); got != driftExitUnreadable {
			t.Fatalf("exit = %d, want %d (err: %v)", got, driftExitUnreadable, err)
		}
		if want := daemon + " path spec.replicas expired after 2026-11-18"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	})

	t.Run("the same path on another object stays unexplained", func(t *testing.T) {
		d, out, town := driftTestSeams(t, func(f *clusterRenderFlags) { f.dashboard = true })
		m := driftTestRendered(t, d, driftTestFlags(t, town))
		d.kubectl = driftAnswering(t, m, func(t *testing.T, live map[string]any) {
			if live["kind"] == "Deployment" {
				driftObjectAt(live, "spec")["replicas"] = float64(7)
			}
		})
		d.exceptions = driftExceptionsFor(daemon, "spec.replicas", "2026-12-01")
		if got := driftExit(runClusterDrift(context.Background(), d)); got != 1 {
			t.Fatalf("exit = %d, want 1; printed:\n%s", got, out.String())
		}
		diff := driftWantDifference(t, "spec.replicas", 1, float64(7))
		if !strings.Contains(out.String(), "deployment/"+k8sdaemon.Name+": explained: "+diff) ||
			!strings.Contains(out.String(), "deployment/"+k8sdaemon.DashboardName+": "+diff) {
			t.Errorf("printed\n%s, want the path explained only for %s", out, k8sdaemon.Name)
		}
	})
}

// TestTownClusterDriftExceptionsWiring refuses a run with no file or clock.
func TestTownClusterDriftExceptionsWiring(t *testing.T) {
	t.Parallel()
	for _, drop := range []func(*clusterDriftDeps){func(d *clusterDriftDeps) { d.exceptions = nil }, func(d *clusterDriftDeps) { d.now = nil }} {
		d, _, _ := driftTestSeams(t, nil)
		d.kubectl = driftNoRead(t)
		drop(&d)
		if err := runClusterDrift(context.Background(), d); driftExit(err) != driftExitUnreadable {
			t.Fatalf("exit = %d, want %d (err: %v)", driftExit(err), driftExitUnreadable, err)
		}
	}
}
