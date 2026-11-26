package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// captureRedacted stands in for every value a capture may not carry, but the
// runner ConfigMap's config.yml, which the drift review reads and no secret backs.
const (
	captureRedacted        = "<redacted>"
	captureRunnerConfigMap = "forgejo-runner-config"
	captureRunnerConfigKey = "config.yml"
	captureLastApplied     = "kubectl.kubernetes.io/last-applied-configuration"
)

// captureContainerPaths are the only places a capture looks for container env
// values: a Pod's own containers, a workload's pod template, and a CronJob's pod
// template, which nests one level deeper under jobTemplate. A key named
// containers anywhere else is left alone, so a field that merely shares the name
// is never mistaken for a container list (gt-ck45yu).
var captureContainerPaths = [][]string{
	{"spec", "containers"},
	{"spec", "initContainers"},
	{"spec", "template", "spec", "containers"},
	{"spec", "template", "spec", "initContainers"},
	{"spec", "jobTemplate", "spec", "template", "spec", "containers"},
	{"spec", "jobTemplate", "spec", "template", "spec", "initContainers"},
}

// captureObject returns the map a capture file holds: a copy of obj under a
// "_capture" header naming the cluster, the instant (RFC 3339 UTC) and the
// object's uid, with the fields and values a capture may not keep removed or
// redacted. It reads no clock and does no I/O; the command in gt-5jwtdg owns both.
func captureObject(obj map[string]any, kubeContext string, at time.Time) (map[string]any, error) {
	object := make(map[string]any, len(obj))
	for key, value := range obj {
		object[key] = captureCopy(value)
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("the object has no metadata, so its uid cannot be read")
	}
	uid, ok := metadata["uid"].(string)
	if !ok || uid == "" {
		return nil, errors.New("the object has no uid, so the capture cannot say which object it holds")
	}
	if raw, present := metadata["annotations"]; present {
		annotations, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("the object's annotations are not an object, so its last-applied copy cannot be removed")
		}
		delete(annotations, captureLastApplied)
	}
	for _, field := range []string{"managedFields", "resourceVersion"} {
		delete(metadata, field)
	}
	delete(object, "status")
	keepConfig := object["kind"] == "ConfigMap" && metadata["name"] == captureRunnerConfigMap
	for _, field := range []string{"data", "stringData", "binaryData"} {
		raw, present := object[field]
		if !present {
			continue
		}
		values, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the object's %s is not an object, so its values cannot be redacted", field)
		}
		for key := range values {
			if keepConfig && key == captureRunnerConfigKey {
				continue
			}
			values[key] = captureRedacted
		}
	}
	for _, path := range captureContainerPaths {
		if err := captureRedactEnvAt(object, path); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"_capture": map[string]any{
			"cluster": kubeContext,
			"at":      at.UTC().Format(time.RFC3339),
			"uid":     uid,
		},
		"object": object,
	}, nil
}

// captureCopy returns value with every object and list copied, so the redaction
// never changes the map the caller read from the cluster.
func captureCopy(value any) any {
	switch v := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(v))
		for key, child := range v {
			copied[key] = captureCopy(child)
		}
		return copied
	case []any:
		copied := make([]any, len(v))
		for i, child := range v {
			copied[i] = captureCopy(child)
		}
		return copied
	}
	return value
}

// captureRedactEnvAt replaces every container env value under path with
// captureRedacted. An absent path is skipped; a node along it that is present but
// the wrong shape is an error, so a value is never copied through because its
// shape hid it from the redaction.
func captureRedactEnvAt(object map[string]any, path []string) error {
	node := object
	for i, key := range path {
		raw, present := node[key]
		if !present {
			return nil
		}
		if i == len(path)-1 {
			return captureRedactContainerList(raw, strings.Join(path, "."))
		}
		child, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("the object's %s is not an object, so its containers cannot be checked", strings.Join(path[:i+1], "."))
		}
		node = child
	}
	return nil
}

// captureRedactContainerList replaces the env value of every container in raw, a
// list at the known path at. A list, a container or an env of another shape is an
// error rather than a skip: a container whose env cannot be read may hold a
// value the capture must not keep.
func captureRedactContainerList(raw any, at string) error {
	containers, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("the object's %s is not a list, so its containers cannot be checked", at)
	}
	for _, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("a container under %s is not an object, so its env cannot be checked", at)
		}
		rawEnv, present := container["env"]
		if !present {
			continue
		}
		env, ok := rawEnv.([]any)
		if !ok {
			return fmt.Errorf("an env under %s is not a list, so its values cannot be redacted", at)
		}
		for _, item := range env {
			variable, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("an env entry under %s is not an object, so its value cannot be redacted", at)
			}
			if _, present := variable["value"]; present {
				variable["value"] = captureRedacted
			}
		}
	}
	return nil
}
