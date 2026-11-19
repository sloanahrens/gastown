package cmd

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// TestClusterShapeDriftObjectsHoldsAClusterInStep drives the compare over every
// namespaced object a control-cluster render builds with the live objects in
// step, and over the same objects differing only in a field the API server fills
// in: neither is a difference, so each run exits 0 and prints nothing
// (gt-03u02a).
func TestClusterShapeDriftObjectsHoldsAClusterInStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(t *testing.T, live map[string]any)
		// read is the object the row differs in, which the run must have read:
		// an object the run never asked the cluster for could not report the
		// difference the row keeps out of the output.
		read string
	}{
		{
			name:   "every rendered object in step",
			mutate: nil,
		},
		{
			name: "a Service that differs only in its server-assigned clusterIP",
			mutate: func(_ *testing.T, live map[string]any) {
				if live["kind"] != "Service" || live["metadata"].(map[string]any)["name"] != k8sdaemon.DashboardName {
					return
				}
				driftObjectAt(live, "spec")["clusterIP"] = "10.43.0.99"
			},
			read: "service/" + k8sdaemon.DashboardName,
		},
		{
			name: "a claim that differs only in its bound volumeName and status",
			mutate: func(_ *testing.T, live map[string]any) {
				if live["kind"] != "PersistentVolumeClaim" || live["metadata"].(map[string]any)["name"] != k8sdaemon.TownStateClaim {
					return
				}
				driftObjectAt(live, "spec")["volumeName"] = "pvc-99999999-0000-4000-8000-000000000000"
				live["status"] = map[string]any{"phase": "Bound"}
			},
			read: "persistentvolumeclaim/" + k8sdaemon.TownStateClaim,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftEveryObjectSeams(t)
			var reads []string
			d.kubectl = driftRecordReads(driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), tc.mutate), &reads)
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != 0 {
				t.Fatalf("exit = %d, want 0 (err: %v); printed:\n%s", got, err, out.String())
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q for a cluster in step, want nothing", got)
			}
			if tc.read != "" && !slices.Contains(reads, tc.read) {
				t.Errorf("read %q, want the row's %q among them", reads, tc.read)
			}
		})
	}
}

// TestClusterShapeDriftObjectsReportsADifference moves one field of a rendered
// namespaced object in the live one, so the run must exit 1 and name the
// resource and the path (gt-03u02a).
func TestClusterShapeDriftObjectsReportsADifference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resource string
		path     string
		rendered func(m k8sdaemon.Manifests) any
		observed any
		mutate   func(t *testing.T, live map[string]any)
	}{
		{
			name:     "a Service port",
			resource: "service/" + k8sdaemon.DashboardName,
			path:     "spec.ports[0].port",
			rendered: func(m k8sdaemon.Manifests) any { return m.DashboardService.Spec.Ports[0].Port },
			observed: 9000,
			mutate: func(_ *testing.T, live map[string]any) {
				if live["kind"] != "Service" || live["metadata"].(map[string]any)["name"] != k8sdaemon.DashboardName {
					return
				}
				driftObjectAt(live, "spec")["ports"].([]any)[0].(map[string]any)["port"] = 9000
			},
		},
		{
			name:     "a claim's storage request",
			resource: "persistentvolumeclaim/" + k8sdaemon.TownStateClaim,
			path:     "spec.resources.requests.storage",
			rendered: func(m k8sdaemon.Manifests) any { return m.TownStateClaim.Spec.Resources.Requests["storage"] },
			observed: "21Gi",
			mutate: func(_ *testing.T, live map[string]any) {
				if live["kind"] != "PersistentVolumeClaim" || live["metadata"].(map[string]any)["name"] != k8sdaemon.TownStateClaim {
					return
				}
				driftObjectAt(live, "spec", "resources", "requests")["storage"] = "21Gi"
			},
		},
		{
			name:     "the backup CronJob's schedule",
			resource: "cronjob/" + k8sdaemon.TrackerBackupName,
			path:     "spec.schedule",
			rendered: func(m k8sdaemon.Manifests) any { return m.TrackerBackupCronJob.Spec.Schedule },
			observed: "0 4 * * *",
			mutate: func(_ *testing.T, live map[string]any) {
				if live["kind"] != "CronJob" {
					return
				}
				driftObjectAt(live, "spec")["schedule"] = "0 4 * * *"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftEveryObjectSeams(t)
			m := driftTestRendered(t, d, driftTestFlags(t, town))
			d.kubectl = driftAnswering(t, m, tc.mutate)
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != 1 {
				t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
			}
			wantLines := []string{tc.resource + ": " + driftWantDifference(t, tc.path, tc.rendered(m), tc.observed)}
			if got := driftLines(out); !slices.Equal(got, wantLines) {
				t.Errorf("printed\n%q\nwant\n%q", got, wantLines)
			}
		})
	}
}

// TestClusterShapeDriftObjectsRefusesAnObjectItCannotRead holds each namespaced
// kind the cluster does not answer for to the unreadable exit status, naming the
// object, never 0 or 1 (gt-03u02a).
func TestClusterShapeDriftObjectsRefusesAnObjectItCannotRead(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{
		"service/" + k8sdaemon.DashboardName,
		"persistentvolumeclaim/" + k8sdaemon.TownStateClaim,
		"serviceaccount/" + k8sdaemon.ServiceAccountName,
		"cronjob/" + k8sdaemon.TrackerBackupName,
	} {
		t.Run(resource, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftEveryObjectSeams(t)
			answer := driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), nil)
			d.kubectl = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
				if len(args) > 1 && args[1] == resource {
					// kubectl's status for an object the cluster does not hold.
					return "", 1, nil
				}
				return answer(ctx, dir, env, name, args...)
			}
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
			}
			if want := "the live " + resource + " could not be read"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// TestClusterShapeDriftObjectsNeverPrintsAValue holds a Service port and a claim
// request that differ to lines naming the path and a fingerprint of each side,
// with neither the rendered nor the live value anywhere in the output
// (gt-03u02a).
func TestClusterShapeDriftObjectsNeverPrintsAValue(t *testing.T) {
	t.Parallel()
	d, out, town := driftEveryObjectSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	renderedPort := m.DashboardService.Spec.Ports[0].Port
	renderedStorage := m.TownStateClaim.Spec.Resources.Requests["storage"]
	d.kubectl = driftAnswering(t, m, func(_ *testing.T, live map[string]any) {
		switch live["kind"] {
		case "Service":
			if live["metadata"].(map[string]any)["name"] == k8sdaemon.DashboardName {
				driftObjectAt(live, "spec")["ports"].([]any)[0].(map[string]any)["port"] = 9100
			}
		case "PersistentVolumeClaim":
			if live["metadata"].(map[string]any)["name"] == k8sdaemon.TownStateClaim {
				driftObjectAt(live, "spec", "resources", "requests")["storage"] = "64Gi"
			}
		}
	})
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
	}
	lines := driftLines(out)
	if len(lines) != 2 {
		t.Fatalf("printed %d lines, want the two differences:\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		if !driftLineShape.MatchString(line) {
			t.Errorf("line %q is not its path and a fingerprint of each side", line)
		}
	}
	// A value is printed with a space before it; a fingerprint carries none, so
	// this holds whichever fingerprint the sides hash to.
	for _, value := range []string{" 9100", " " + strconv.Itoa(int(renderedPort)), "64Gi", renderedStorage} {
		if strings.Contains(out.String(), value) {
			t.Errorf("printed the value %q, want paths and fingerprints only:\n%s", value, out.String())
		}
	}
}
