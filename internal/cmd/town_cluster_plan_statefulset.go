package cmd

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/steveyegge/gastown/internal/update"
)

// planLiveStatefulSet reads the StatefulSet a rendered file names, then the
// ControllerRevision its status.updateRevision names in the same namespace, and
// records that object's .revision: the number `kubectl rollout undo
// statefulset/<name> --to-revision` takes. A StatefulSet adds no compare lines
// and no resourceVersion. One the cluster does not hold is absent with no
// revision and no second read; a failed read of either object, a missing or
// empty status.updateRevision, an absent ControllerRevision, and a .revision
// that is missing, not a number, not whole or not positive refuse the plan.
func planLiveStatefulSet(ctx context.Context, kube update.RunFunc, rendered map[string]any) (planLiveDeployment, error) {
	read := planLiveDeployment{Kind: "StatefulSet"}
	metadata, _ := rendered["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	resource := "statefulset/" + name
	live, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: kube}, resource, namespace, false)
	if err != nil {
		return planLiveDeployment{}, fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	if absent {
		read.Absent = true
		return read, nil
	}
	status, _ := live["status"].(map[string]any)
	current, _ := status["updateRevision"].(string)
	if current == "" {
		return planLiveDeployment{}, fmt.Errorf("gt town cluster plan: %s in %s has no status.updateRevision, so no plan was written", resource, namespace)
	}
	revisionResource := "controllerrevision/" + current
	revision, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: kube}, revisionResource, namespace, false)
	if err != nil {
		return planLiveDeployment{}, fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	if absent {
		return planLiveDeployment{}, fmt.Errorf("gt town cluster plan: %s in %s names %s, which the cluster does not hold, so no plan was written", resource, namespace, revisionResource)
	}
	number, ok := revision["revision"]
	if !ok {
		return planLiveDeployment{}, fmt.Errorf("gt town cluster plan: %s in %s has no revision, so no plan was written", revisionResource, namespace)
	}
	value, ok := planStatefulSetRevision(number)
	if !ok {
		return planLiveDeployment{}, fmt.Errorf("gt town cluster plan: %s in %s has a revision that is not a positive whole number, so no plan was written", revisionResource, namespace)
	}
	read.Revision = value
	return read, nil
}

// planStatefulSetRevision is a ControllerRevision's .revision as the plan
// records it: the decimal of a positive whole JSON number, which decodes as
// float64. A string, a fraction, zero and a negative are not a revision.
func planStatefulSetRevision(value any) (string, bool) {
	number, ok := value.(float64)
	if !ok || number <= 0 || number != math.Trunc(number) {
		return "", false
	}
	return strconv.FormatFloat(number, 'f', 0, 64), true
}
