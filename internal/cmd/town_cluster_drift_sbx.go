package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/deploy/k8s/sbx"
	"github.com/steveyegge/gastown/internal/update"
)

// sbxDriftGroup is one kind in one namespace, read by one list.
type sbxDriftGroup struct {
	kind, namespace string
	cluster         bool
}

// compareSbxDrift compares the gt-sbx set with the sandbox cluster read-only
// through the get-only runner the command was given: it lists each (kind,
// namespace) once, compares every rendered object with the live one by the
// control compare's field walk, reports a set object the cluster lacks, and
// reports a live object the set does not name unless set.IsRuntime names it as
// one the cluster creates itself. Exit 1 is a difference, exit 2 an input it
// could not read (gt-82a9fz). Only a list read, not a get by name, tells a
// missing object from an unreadable read.
func compareSbxDrift(ctx context.Context, d clusterDriftDeps, set sbx.Set, kubectl update.RunFunc) error {
	if kubectl == nil {
		return sbxDriftUnreadable(errors.New("no kubectl read runner is wired, so the sandbox cluster was not compared"))
	}
	if len(set.Objects) == 0 {
		return sbxDriftUnreadable(errors.New("the gt-sbx set names no object, so nothing was compared"))
	}
	bodies := map[sbxDriftGroup]map[string]map[string]any{}
	var order []sbxDriftGroup
	for _, object := range set.Objects {
		if object.Cluster == (object.Namespace != "") {
			return sbxDriftUnreadable(fmt.Errorf("the %s is marked cluster-scoped=%t with namespace %q, so it was not read", staticObjectLabel(object), object.Cluster, object.Namespace))
		}
		doc, err := staticObjectDoc(object)
		if err != nil {
			return sbxDriftUnreadable(err)
		}
		group := sbxDriftGroup{object.Kind, object.Namespace, object.Cluster}
		if bodies[group] == nil {
			order = append(order, group)
			bodies[group] = map[string]map[string]any{}
		}
		bodies[group][object.Name] = doc.body
	}
	differed := false
	for _, group := range order {
		names := slices.Sorted(maps.Keys(bodies[group]))
		live, err := readSbxLive(ctx, kubectl, group, names)
		if err != nil {
			return sbxDriftUnreadable(err)
		}
		for _, name := range names {
			var lines []string
			if body, ok := live[name]; ok {
				lines, err = objectDrift(group.kind, bodies[group][name], body)
			} else {
				lines, err = objectAbsent(group.kind, bodies[group][name])
			}
			if err != nil {
				return sbxDriftUnreadable(err)
			}
			for _, line := range lines {
				fmt.Fprintf(d.out, "%s/%s: %s\n", strings.ToLower(group.kind), name, line)
				differed = true
			}
		}
		for _, name := range slices.Sorted(maps.Keys(live)) {
			if _, ok := bodies[group][name]; ok || set.IsRuntime(sbx.ObjectRef{Kind: group.kind, Namespace: group.namespace, Name: name}) {
				continue
			}
			fmt.Fprintf(d.out, "%s/%s: extra: the rendered set names no object of that name\n", strings.ToLower(group.kind), name)
			differed = true
		}
	}
	if differed {
		return NewSilentExit(1)
	}
	return nil
}

// sbxDriftUnreadable is the refusal for an input it could not read: exit 2.
func sbxDriftUnreadable(err error) error {
	return &ExitCodeError{Code: driftExitUnreadable, Err: err}
}

// readSbxLive lists the group's kind through the guarded runner and returns the
// objects by name. Every refusal names the set objects the read stood for, so an
// unreadable read says which objects were not compared. A non-List answer or an
// item with no printable name is unreadable rather than clean (gt-82a9fz,
// gt-fl4i45).
func readSbxLive(ctx context.Context, kubectl update.RunFunc, group sbxDriftGroup, names []string) (map[string]map[string]any, error) {
	resource := strings.ToLower(group.kind)
	// No name means no object to read; refuse before the list joined below
	// collapses to an empty phrase and the refusal names nothing (gt-fl4i45).
	if len(names) == 0 {
		return nil, fmt.Errorf("no %s object was named to read, so none was compared", resource)
	}
	args := []string{"get", resource, "-o", "json"}
	where := make([]string, len(names))
	for i, name := range names {
		where[i] = resource + "/" + name
	}
	reading := strings.Join(where, ", ")
	if !group.cluster {
		args = append([]string{"-n", group.namespace}, args...)
		reading += " in namespace " + group.namespace
	}
	out, exit, err := kubectl(ctx, "", nil, "kubectl", args...)
	if err != nil {
		return nil, fmt.Errorf("reading the live %s: %w", reading, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("the live %s could not be read: kubectl exited %d", reading, exit)
	}
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || list.Kind != "List" {
		return nil, fmt.Errorf("the live %s answer is not a JSON List, so it was not read", reading)
	}
	live := make(map[string]map[string]any, len(list.Items))
	for _, item := range list.Items {
		metadata, _ := item["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		// A live name is printed, so an empty or control-character one is
		// refused rather than allowed to forge a line.
		if name == "" || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("the live %s holds an object whose name cannot be printed, so it was not read", reading)
		}
		live[name] = item
	}
	return live, nil
}
