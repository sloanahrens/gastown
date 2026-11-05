package sweeprunner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// priorityClassItems is the PriorityClass listing the guard's kubectl call
// answers with, the sweep's class at sweep. The other two values are the ones
// deploy/k8s/priority-classes.yaml declares.
func priorityClassItems(sweep int) []string {
	return []string{
		fmt.Sprintf(`{"kind":"PriorityClass","metadata":{"name":"gt-ci"},"value":%d}`, sweep),
		`{"kind":"PriorityClass","metadata":{"name":"gt-ci-gate"},"value":200}`,
		`{"kind":"PriorityClass","metadata":{"name":"gt-control-plane"},"value":1000000}`,
	}
}

// priorityListingJSON is the guard's kubectl output: the classes and the pods.
func priorityListingJSON(sweep int, pods ...string) []byte {
	items := append(priorityClassItems(sweep), pods...)
	return []byte(`{"kind":"List","items":[` + strings.Join(items, ",") + `]}`)
}

// priorityPod is one pod in the guard's listing, at priority (nil leaves the
// field out) and owned by an object named owner (empty names no owner).
func priorityPod(namespace, name string, priority *int, owner string) string {
	var fields []string
	if priority != nil {
		fields = append(fields, fmt.Sprintf(`"priority":%d`, *priority))
	}
	if owner != "" {
		fields = append(fields, fmt.Sprintf(`"ownerReferences":[{"kind":"ReplicaSet","name":%q}]`, owner))
	}
	return fmt.Sprintf(`{"kind":"Pod","metadata":{"name":%q,"namespace":%q},"spec":{%s}}`,
		name, namespace, strings.Join(fields, ","))
}

// priorityDeclineOf returns the refusal err carries, or fails the test.
func priorityDeclineOf(t *testing.T, err error) *PriorityDecline {
	t.Helper()
	var decline *PriorityDecline
	if !errors.As(err, &decline) {
		t.Fatalf("Refuse = %v, want a PriorityDecline the daemon can log and defer", err)
	}
	return decline
}

// The preflight declines a launch while a pod in a protected namespace runs at
// or below the sweep's priority, with the line naming the pod and the fix, and
// lets the same pod through at a priority above it. The daemon repeats that line
// at most once an hour while it stands (gt-gg9hv.39).
func TestPriorityDeclinesWhenAProtectedPodOutranksNothing(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	guard := PriorityGuard(spec, []string{"forgejo"})

	if want := []string{"kubectl", "--context", spec.Context, "get", "pods,priorityclasses", "-A", "-o", "json"}; !slices.Equal(guard.Step.Argv, want) {
		t.Fatalf("the guard runs %v, want the one listing of the pods and the classes %v", guard.Step.Argv, want)
	}

	zero, top := 0, 1000000
	decline := priorityDeclineOf(t, guard.Refuse(priorityListingJSON(100, priorityPod("forgejo", "forgejo-abc", &zero, ""))))
	const wantLine = "tier_sweep: not launching: pod forgejo/forgejo-abc runs at priority 0, not above the sweep's 100; set priorityClassName gt-control-plane"
	if got := decline.Line(); got != wantLine {
		t.Errorf("the decline line =\n%s\nwant\n%s", got, wantLine)
	}

	if err := guard.Refuse(priorityListingJSON(100, priorityPod("forgejo", "forgejo-abc", &top, ""))); err != nil {
		t.Errorf("a pod at the control plane's priority refused the launch: %v", err)
	}

	now := time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)
	var reports DeclineLog
	if !reports.Report(now, wantLine) {
		t.Error("the first decline was not reported")
	}
	if reports.Report(now.Add(59*time.Minute), wantLine) {
		t.Error("the same standing decline was reported again inside the hour")
	}
	if !reports.Report(now.Add(time.Hour), wantLine) {
		t.Error("the standing decline was not reported again after the hour")
	}
}

// The sweep's priority is the value of the class its Job names, read from the
// cluster: the same pod and classes decide the other way when only that value
// changes, so the comparison cannot be a constant's.
func TestPriorityReadsTheSweepClassFromTheCluster(t *testing.T) {
	t.Parallel()
	guard := PriorityGuard(goldenSpec(), []string{"forgejo"})
	at100 := 100

	err := guard.Refuse(priorityListingJSON(100, priorityPod("forgejo", "forgejo-abc", &at100, "")))
	decline := priorityDeclineOf(t, err)
	if !strings.Contains(decline.Line(), "not above the sweep's 100") {
		t.Errorf("the decline names another sweep priority: %s", decline.Line())
	}

	// The class the Job names reads as 50 this time: the same pod is above it.
	if err := guard.Refuse(priorityListingJSON(50, priorityPod("forgejo", "forgejo-abc", &at100, ""))); err != nil {
		t.Errorf("a pod at 100 refused a sweep at 50, so the sweep's priority is not read from the cluster: %v", err)
	}
}

// The table's live pods are checked row by row: a pod at its row's class value
// passes, a runner pod is read against the runner's row rather than the Forgejo
// Deployment whose name it also starts with, a pod at another priority is
// declined, and a pod whose priority or class cannot be read is declined, never
// passed (gt-gg9hv.39).
func TestPriorityTableChecksTheLivePods(t *testing.T) {
	t.Parallel()
	guard := PriorityGuard(goldenSpec(), []string{"forgejo"})
	top, gate := 1000000, 200

	for name, tc := range map[string]struct {
		listing []byte
		refused string
	}{
		"a pod at its row's class": {
			listing: priorityListingJSON(100, priorityPod("forgejo", "forgejo-abc", &top, "forgejo-57658978cf")),
		},
		"the runner is read against its own row": {
			listing: priorityListingJSON(100, priorityPod("forgejo", "forgejo-runner-abc", &gate, "forgejo-runner-df77dd979")),
		},
		"a pod at another row's class": {
			listing: priorityListingJSON(100, priorityPod("forgejo", "forgejo-abc", &gate, "forgejo-57658978cf")),
			refused: "not the 1000000 the table records for deployment forgejo",
		},
		"a pod whose priority cannot be read": {
			listing: priorityListingJSON(100, priorityPod("forgejo", "forgejo-abc", nil, "forgejo-57658978cf")),
			refused: "names no priority",
		},
		"a table class the cluster does not list": {
			listing: []byte(`{"kind":"List","items":[` +
				`{"kind":"PriorityClass","metadata":{"name":"gt-ci"},"value":100},` +
				`{"kind":"PriorityClass","metadata":{"name":"gt-ci-gate"},"value":200}]}`),
			refused: `lists no PriorityClass "gt-control-plane"`,
		},
		"a listing that is not JSON": {
			listing: []byte("Error from server (Forbidden)\n"),
			refused: "reading the pods' priorities",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := guard.Refuse(tc.listing)
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("Refuse = %v, want the launch to pass", err)
				}
				return
			}
			decline := priorityDeclineOf(t, err)
			if !strings.Contains(decline.Line(), tc.refused) {
				t.Errorf("the decline = %q, want it to name %q", decline.Line(), tc.refused)
			}
		})
	}
}

// A guard whose namespaces were never wired checks nothing, so it refuses: a
// preflight that did not run is not a preflight that passed.
func TestPriorityRefusesWithNoProtectedNamespaces(t *testing.T) {
	t.Parallel()
	guard := PriorityGuard(goldenSpec(), nil)
	decline := priorityDeclineOf(t, guard.Refuse(priorityListingJSON(100)))
	if !strings.Contains(decline.Line(), "protected_namespaces") {
		t.Errorf("the decline = %q, want it to name the setting", decline.Line())
	}
}

// The checked-in table is the live state: exactly the three control-plane
// workloads, every row at a class deploy/k8s/priority-classes.yaml declares
// above the sweep's, and the table the daemon carries equal to the file
// (gt-gg9hv.39).
func TestPriorityTableMatchesTheLiveDeployments(t *testing.T) {
	t.Parallel()
	rows := readControlPrioritiesFile(t, filepath.Join("..", "..", "deploy", "k8s", "control-priorities.yaml"))
	want := []ControlPriority{
		{Namespace: "forgejo", Deployment: "forgejo", Class: "gt-control-plane"},
		{Namespace: "forgejo-trial", Deployment: "forgejo", Class: "gt-control-plane"},
		{Namespace: "forgejo", Deployment: "forgejo-runner", Class: "gt-ci-gate"},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("deploy/k8s/control-priorities.yaml =\n%v\nwant exactly\n%v", rows, want)
	}
	if !slices.Equal(ControlPriorities, want) {
		t.Errorf("the daemon's table =\n%v\nwant the file's rows\n%v", ControlPriorities, want)
	}

	classes := readPriorityClassesFile(t, filepath.Join("..", "..", "deploy", "k8s", "priority-classes.yaml"))
	sweep, ok := classes[PriorityClass]
	if !ok {
		t.Fatalf("deploy/k8s/priority-classes.yaml declares no %q, the sweep's class", PriorityClass)
	}
	if err := checkPriorityTable(rows, classes, sweep); err != nil {
		t.Errorf("the checked-in table is not above the sweep at %d: %v", sweep, err)
	}

	below := append(slices.Clone(rows), ControlPriority{Namespace: "forgejo", Deployment: "forgejo-extra", Class: PriorityClass})
	err := checkPriorityTable(below, classes, sweep)
	if err == nil || !strings.Contains(err.Error(), "not above the sweep's") {
		t.Errorf("a row naming the sweep's own class = %v, want it refused as below the sweep", err)
	}
}

// controlPrioritiesFile is deploy/k8s/control-priorities.yaml's shape.
type controlPrioritiesFile struct {
	Rows []struct {
		Namespace  string `yaml:"namespace"`
		Deployment string `yaml:"deployment"`
		Class      string `yaml:"class"`
	} `yaml:"rows"`
}

// readControlPrioritiesFile parses the checked-in table.
func readControlPrioritiesFile(t *testing.T, path string) []ControlPriority {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc controlPrioritiesFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
	rows := make([]ControlPriority, len(doc.Rows))
	for i, row := range doc.Rows {
		rows[i] = ControlPriority{Namespace: row.Namespace, Deployment: row.Deployment, Class: row.Class}
	}
	return rows
}

// priorityClassDoc is one PriorityClass document of deploy/k8s/priority-classes.yaml.
type priorityClassDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Value int `yaml:"value"`
}

// readPriorityClassesFile parses the cluster's class manifest into name→value.
func readPriorityClassesFile(t *testing.T, path string) map[string]int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	classes := make(map[string]int)
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc priorityClassDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return classes
		}
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", path, err)
		}
		if doc.Kind == "" {
			continue
		}
		if doc.Kind != "PriorityClass" {
			t.Fatalf("%s holds a %q document; want PriorityClass documents only", path, doc.Kind)
		}
		classes[doc.Metadata.Name] = doc.Value
	}
}
