package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// container is one entry of a pod template's or a Pod's containers and
// initContainers.
type container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// containerStatus is one entry of a Pod's status. imageID is what the runtime
// pulled, e.g. docker-pullable://repo@sha256:... or containerd://sha256:...
type containerStatus struct {
	Name    string `json:"name"`
	ImageID string `json:"imageID"`
}

type podSpec struct {
	Containers     []container `json:"containers"`
	InitContainers []container `json:"initContainers"`
}

// kubeObject is the part of a Job, Deployment or Pod that the prune reads.
type kubeObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		podSpec  // a Pod's own containers
		Template struct {
			Spec podSpec `json:"spec"` // a Job's or Deployment's pod template
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses          []containerStatus `json:"containerStatuses"`
		InitContainerStatuses      []containerStatus `json:"initContainerStatuses"`
		EphemeralContainerStatuses []containerStatus `json:"ephemeralContainerStatuses"`
	} `json:"status"`
}

// nodeImagePruneSpares returns the sorted, distinct sha256 digests that the gt
// namespace's Jobs, Deployments and Pods run, from
// `kubectl -n gt get job,deploy,pod -o json`. An image with no digest cannot be
// spared by digest, so it refuses the whole list, naming the object. So does a
// list with no items, no Deployment, or no digest at all: a live town always has
// Deployments, so an empty answer is a failed read, never "spare nothing".
func nodeImagePruneSpares(listJSON []byte) ([]string, error) {
	var list struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(listJSON, &list); err != nil {
		return nil, fmt.Errorf("nodeImagePruneSpares: the listing does not parse: %w", err)
	}
	if list.Kind != "List" {
		return nil, fmt.Errorf("nodeImagePruneSpares: the document is a %q named %q, not a List", list.Kind, list.Metadata.Name)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("nodeImagePruneSpares: the listing has no items, so it cannot say what the node runs")
	}

	spares := map[string]bool{}
	deployments := 0
	for _, raw := range list.Items {
		var obj kubeObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("nodeImagePruneSpares: an item does not parse: %w", err)
		}
		switch obj.Kind {
		case "Job", "Deployment", "Pod":
		default:
			return nil, fmt.Errorf("nodeImagePruneSpares: %s %q is not a Job, Deployment or Pod", obj.Kind, obj.Metadata.Name)
		}
		if obj.Metadata.Name == "" {
			return nil, fmt.Errorf("nodeImagePruneSpares: a %s has no name", obj.Kind)
		}
		if obj.Kind == "Deployment" {
			deployments++
		}
		digests, err := objectDigests(obj)
		if err != nil {
			return nil, err
		}
		for _, d := range digests {
			spares[d] = true
		}
	}
	if deployments == 0 {
		return nil, fmt.Errorf("nodeImagePruneSpares: the listing has no Deployment, so it cannot say what the node runs")
	}
	if len(spares) == 0 {
		return nil, fmt.Errorf("nodeImagePruneSpares: the listing names no image digest, so it cannot say what the node runs")
	}

	out := make([]string, 0, len(spares))
	for d := range spares {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// objectDigests returns the digests that obj's images run. A Pod's container
// with no digest in its spec image is spared by the imageID its runtime reported;
// a container with neither is refused, as is an imageID with no valid digest.
func objectDigests(obj kubeObject) ([]string, error) {
	var digests []string

	imageIDDigest := map[string]string{}
	statuses := [][]containerStatus{obj.Status.ContainerStatuses, obj.Status.InitContainerStatuses, obj.Status.EphemeralContainerStatuses}
	for _, group := range statuses {
		for _, st := range group {
			if st.ImageID == "" {
				// Not pulled yet: the spec image is the only record of it.
				continue
			}
			d, ok := digestIn(st.ImageID)
			if !ok {
				return nil, refuseObject(obj, "reports an imageID with no valid sha256 digest")
			}
			digests = append(digests, d)
			imageIDDigest[st.Name] = d
		}
	}

	var specs []container
	if obj.Kind == "Pod" {
		specs = append(specs, obj.Spec.Containers...)
		specs = append(specs, obj.Spec.InitContainers...)
	} else {
		specs = append(specs, obj.Spec.Template.Spec.Containers...)
		specs = append(specs, obj.Spec.Template.Spec.InitContainers...)
	}
	for _, c := range specs {
		if d, ok := digestIn(c.Image); ok {
			digests = append(digests, d)
			continue
		}
		if d, ok := imageIDDigest[c.Name]; ok {
			digests = append(digests, d)
			continue
		}
		return nil, refuseObject(obj, "runs an image with no sha256 digest")
	}
	return digests, nil
}

// digestIn returns the sha256 digest at the end of an image reference or imageID,
// and whether it is a valid digest. A reference with no digest reports false.
func digestIn(ref string) (string, bool) {
	i := strings.LastIndex(ref, "sha256:")
	if i < 0 {
		return "", false
	}
	d := ref[i:]
	return d, imagevalues.ValidDigest(d)
}

// refuseObject names the object whose image cannot be spared by digest. The
// image itself is not echoed: it is captured cluster output.
func refuseObject(obj kubeObject, why string) error {
	return fmt.Errorf("nodeImagePruneSpares: %s %q %s, so its image cannot be spared by digest", obj.Kind, obj.Metadata.Name, why)
}
