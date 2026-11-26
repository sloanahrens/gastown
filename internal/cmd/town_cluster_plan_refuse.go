package cmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	updatejob "github.com/steveyegge/gastown/deploy/k8s/update-job"
	"github.com/steveyegge/gastown/internal/secretpreflight"
	"github.com/steveyegge/gastown/internal/update"
)

// planUnreviewedStaticSecret names the first Secret document the render's static
// set holds that deploy/k8s does not review, or "" when every one of them is a
// reviewed document. A document is reviewed when a reviewed Secret of the same
// kind, name and namespace declares the same fields and it carries no data or
// stringData: the name or the declared fields alone are not enough, since a
// Secret may reuse the reviewed one's identity with a field, or a value, the
// operator never reviewed.
//
// Secrets stay operator-created: the reviewed static Secret carries no data, the
// cluster fills it in, and the plan creates no Secret's values. So a Secret
// carrying data or stringData is not the reviewed document, whatever its identity
// and declared fields, and it refuses the plan, its name and the key it carries
// named, and never a value. Any other unreviewed one refuses too. A document
// whose fields cannot be read is refused the same way, since it cannot be shown
// to be the reviewed one. reviewedStatic reads the reviewed set; nil reads
// k8s.Static.
func planUnreviewedStaticSecret(static []k8s.StaticObject, reviewedStatic func() ([]k8s.StaticObject, error)) (string, error) {
	if reviewedStatic == nil {
		reviewedStatic = k8s.Static
	}
	reviewed, err := reviewedStatic()
	if err != nil {
		return "", fmt.Errorf("cannot read the static documents deploy/k8s reviews: %w", err)
	}
	reviewedSecrets := make(map[applyKey]map[string]any, len(reviewed))
	for _, object := range reviewed {
		if object.Kind != secretKind {
			continue
		}
		// A reviewed Secret that carries data or stringData, or whose document
		// cannot be read, is not recorded: nothing can be shown to match it, so a
		// plan copy of it is refused below.
		if declared, carried := planSecretFields(object); declared != nil && len(carried) == 0 {
			reviewedSecrets[applyKey{kind: object.Kind, name: object.Name, namespace: object.Namespace}] = declared
		}
	}
	for _, object := range static {
		if object.Kind != secretKind {
			continue
		}
		reviewedDeclared, known := reviewedSecrets[applyKey{kind: object.Kind, name: object.Name, namespace: object.Namespace}]
		declared, carried := planSecretFields(object)
		switch {
		case !known:
			return staticObjectLabel(object), nil
		case len(carried) > 0:
			return fmt.Sprintf("%s carrying %s", staticObjectLabel(object), strings.Join(carried, " and ")), nil
		case !reflect.DeepEqual(declared, reviewedDeclared):
			return staticObjectLabel(object), nil
		}
	}
	return "", nil
}

// planSecretFields is a Secret document's declared fields, its document without
// data and stringData, and the keys it carries: the cluster fills a Secret's data
// and the reviewed static Secret carries none, so a plan Secret carrying data or
// stringData is not the reviewed document. A document that cannot be read has no
// declared fields, so nil, and matches no reviewed document. A data or stringData
// of any shape is reported, so a value that is not a name-to-string map is
// refused the same way as one that is.
func planSecretFields(object k8s.StaticObject) (map[string]any, []string) {
	doc, err := staticObjectDoc(object)
	if err != nil {
		return nil, nil
	}
	declared := make(map[string]any, len(doc.body))
	for key, value := range doc.body {
		switch key {
		case "data", "stringData":
			continue
		}
		declared[key] = value
	}
	var carried []string
	for _, field := range []string{"data", "stringData"} {
		value, ok := doc.body[field]
		if !ok {
			continue
		}
		// Name each key the Secret carries, never its value. A field that is not
		// a name-to-value map has no keys to name, so the field itself is named.
		keys, isMap := value.(map[string]any)
		if !isMap || len(keys) == 0 {
			carried = append(carried, field)
			continue
		}
		names := make([]string, 0, len(keys))
		for name := range keys {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			carried = append(carried, fmt.Sprintf("%s key %q", field, name))
		}
	}
	return declared, carried
}

type planMountedSecret struct{ namespace, name string }

// planMountedSecrets returns the Secrets the rendered Deployments mount by volume,
// once each, in its Deployment's namespace. A document whose spec or volumes
// cannot be read is an error, since its Secrets would otherwise pass unchecked.
func planMountedSecrets(bodies [][]byte) ([]planMountedSecret, error) {
	var mounted []planMountedSecret
	seen := map[planMountedSecret]bool{}
	for _, body := range bodies {
		var doc map[string]any
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("cannot read a rendered file to find the Secrets its Deployment mounts: %w", err)
		}
		if kind, _ := doc["kind"].(string); kind != "Deployment" {
			continue
		}
		metadata, _ := doc["metadata"].(map[string]any)
		namespace, _ := metadata["namespace"].(string)
		names, err := planDeploymentSecretVolumes(doc)
		if err != nil {
			return nil, fmt.Errorf("cannot read the Secrets the Deployment %v mounts: %w", metadata["name"], err)
		}
		for _, name := range names {
			if mount := (planMountedSecret{namespace, name}); !seen[mount] {
				seen[mount] = true
				mounted = append(mounted, mount)
			}
		}
	}
	return mounted, nil
}

// planDeploymentSecretVolumes returns the Secret names a Deployment's volumes mount.
func planDeploymentSecretVolumes(doc map[string]any) ([]string, error) {
	object := doc
	for _, field := range []string{"spec", "template", "spec"} {
		next, ok := object[field].(map[string]any)
		if !ok {
			return nil, errors.New("its spec is not an object")
		}
		object = next
	}
	raw, ok := object["volumes"]
	if !ok {
		return nil, nil
	}
	volumes, ok := raw.([]any)
	if !ok {
		return nil, errors.New("its spec.template.spec.volumes is not a list")
	}
	var names []string
	for _, rawVolume := range volumes {
		volume, ok := rawVolume.(map[string]any)
		if !ok {
			return nil, errors.New("a spec.template.spec.volumes entry is not an object")
		}
		rawSecret, ok := volume["secret"]
		if !ok {
			continue
		}
		secret, ok := rawSecret.(map[string]any)
		if !ok {
			return nil, errors.New("a volume's secret is not an object")
		}
		name, _ := secret["secretName"].(string)
		if name == "" {
			return nil, errors.New("a volume's secret names no Secret")
		}
		names = append(names, name)
	}
	return names, nil
}

// planSecretContext is the context name Missing requires; the runner replaces it.
const planSecretContext = "apply"

// planSecretPreflight refuses a plan whose Deployments mount a Secret the cluster
// does not hold, naming it, and errors for one it could not judge, never as missing.
func planSecretPreflight(ctx context.Context, kube update.RunFunc, kubeconfig string, mounted []planMountedSecret) error {
	if len(mounted) == 0 {
		return nil
	}
	if err := checkClusterKubeconfig(kubeconfig); err != nil {
		return err
	}
	if kubeconfig == "" {
		return errors.New("the Deployments mount Secrets and the plan has no --apply-kubeconfig, so they were not checked")
	}
	names := map[string][]string{}
	var order []string
	for _, mount := range mounted {
		if _, ok := names[mount.namespace]; !ok {
			order = append(order, mount.namespace)
		}
		names[mount.namespace] = append(names[mount.namespace], mount.name)
	}
	for _, namespace := range order {
		missing, err := secretpreflight.Missing(ctx, planSecretRunner(kube, kubeconfig), planSecretContext, namespace, names[namespace])
		if err != nil {
			return fmt.Errorf("checking the Secrets the Deployments mount in namespace %s (%s): %w", namespace, strings.Join(names[namespace], ", "), err)
		}
		if len(missing) > 0 {
			return fmt.Errorf("the Deployment in namespace %s mounts Secret %s, which the cluster does not hold, so no plan.json was written", namespace, strings.Join(missing, ", "))
		}
	}
	return nil
}

// planSecretRunner reads through kube under the apply kubeconfig, carrying a
// failed exit's status so only NotFound reads as absent.
func planSecretRunner(kube update.RunFunc, kubeconfig string) secretpreflight.Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "--context" {
			args = args[2:]
		}
		out, exit, err := kube(ctx, "", nil, name, append([]string{"--kubeconfig", kubeconfig}, args...)...)
		if err != nil {
			return nil, err
		}
		if exit != 0 {
			return []byte(out), &planSecretExit{code: exit}
		}
		return []byte(out), nil
	}
}

// planSecretExit carries a failed Secret read's status, and never its output.
type planSecretExit struct{ code int }

func (e *planSecretExit) Error() string { return fmt.Sprintf("the Secret read exited %d", e.code) }

func (e *planSecretExit) ExitCode() int { return e.code }

// planNestedObject is doc's field at path as an object, and false when the
// document lacks one of the fields. A field present but not an object is an
// error, since a value the compare cannot read cannot be shown to differ or to
// match.
func planNestedObject(doc map[string]any, path ...string) (map[string]any, bool, error) {
	object := doc
	walked := ""
	for _, field := range path {
		walked = joinPath(walked, field)
		raw, ok := object[field]
		if !ok {
			return nil, false, nil
		}
		child, ok := raw.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("%s is not an object", walked)
		}
		object = child
	}
	return object, true, nil
}

// planContainerImages is the image every container of a Deployment's pod
// template names, keyed by its list and name. The lists are the ones
// containerEntryPath reads an image in, so this compare sees exactly what the
// drift compare skips. A container whose name or image is not a non-empty
// string is an error: an image that cannot be read cannot be shown to differ
// from the cluster's or to match it.
func planContainerImages(doc map[string]any) (map[string]string, error) {
	pod, ok, err := planNestedObject(doc, "spec", "template", "spec")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the Deployment has no spec.template.spec, so its container images cannot be read")
	}
	images := map[string]string{}
	for _, list := range []string{"initContainers", "containers", "ephemeralContainers"} {
		raw, ok := pod[list]
		if !ok {
			continue
		}
		entries, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("the pod template's %s is not a list", list)
		}
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("a pod template %s entry is not an object", list)
			}
			name, _ := entry["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("a pod template %s entry names no container", list)
			}
			image, _ := entry["image"].(string)
			if image == "" {
				return nil, fmt.Errorf("a pod template %s entry names no image, so an image roll cannot be told from a match", list)
			}
			images[list+"/"+name] = image
		}
	}
	return images, nil
}

// planPolecatImage is a Deployment pod template's gastown.dev/polecat-image
// annotation, and "" when it names none: the render carries the annotation only
// when it was given a polecat image. A present annotations field that is not an
// object, and a present value that is not a string, are errors, so an
// unreadable annotation never reads as absent.
func planPolecatImage(doc map[string]any) (string, error) {
	object := doc
	for _, path := range [][]string{{"spec", "template"}, {"metadata"}, {"annotations"}} {
		next, ok, err := planNestedObject(object, path...)
		if err != nil || !ok {
			return "", err
		}
		object = next
	}
	raw, ok := object[k8sdaemon.PolecatImageAnnotation]
	if !ok {
		return "", nil
	}
	image, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("the pod template's %s annotation is not a string", k8sdaemon.PolecatImageAnnotation)
	}
	return image, nil
}

// planImageRollDiffers reports whether an image roll would change a Deployment:
// a container image, or the pod template's gastown.dev/polecat-image annotation,
// differs between the rendered object and the live one. The drift compare skips
// both, so it cannot answer this and the plan asks here. A container the live
// object lacks is left to that compare, which reports the entry absent.
func planImageRollDiffers(rendered, live map[string]any) (bool, error) {
	renderedImages, err := planContainerImages(rendered)
	if err != nil {
		return false, err
	}
	liveImages, err := planContainerImages(live)
	if err != nil {
		return false, err
	}
	for key, image := range renderedImages {
		if liveImage, ok := liveImages[key]; ok && liveImage != image {
			return true, nil
		}
	}
	renderedPolecat, err := planPolecatImage(rendered)
	if err != nil {
		return false, err
	}
	livePolecat, err := planPolecatImage(live)
	if err != nil {
		return false, err
	}
	return renderedPolecat != livePolecat, nil
}

// planImageRollRefusal refuses a plan whose only differences are an image roll.
// The roll belongs to the update Job, whose waits and record an operator's
// apply would not reach. The refusal is a stopgap: revisit it after the first
// five deployer applies (gt-s3vqzy).
func planImageRollRefusal() error {
	return fmt.Errorf("gt town cluster plan: the plan's only differences are container image digests and the %s annotation, so it is an image roll; roll an image with the %s Job (Role %s), not a plan apply, and no plan.json was written", k8sdaemon.PolecatImageAnnotation, updatejob.Name, updateRoleName)
}
