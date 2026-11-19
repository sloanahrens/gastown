package sbx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

const (
	// ChartVersion is the OpenShell chart release this render pins.
	ChartVersion = "0.1.4"
	// ChartDigest is the OCI manifest digest of
	// oci://ghcr.io/nvidia/openshell/helm-chart:0.1.4, recorded for whoever
	// pulls the chart: the render reads a chart directory and cannot check it.
	ChartDigest = "sha256:46c21e50abbe88c742ef46e77728e5201398706061b19871eaf3303adac274d6"
)

const (
	// helmBinary is the program the render runs; template is its only verb.
	helmBinary = "helm"
	// openshellName is both the release name helm renders and the namespace
	// the gateway runs in.
	openshellName = "openshell"
	// hookAnnotation marks a document helm runs during install or upgrade
	// rather than applies as an object.
	hookAnnotation = "helm.sh/hook"
	// gatewayStatefulSet is the gateway StatefulSet's name in the chart.
	gatewayStatefulSet = "openshell"
	// gatewayCPU and gatewayMemory are the request and limit the gateway's
	// containers run with, as up.sh's guaranteed_pod gives them (gt-cjm8fu).
	gatewayCPU    = "250m"
	gatewayMemory = "768Mi"
)

// openshellObjectScopes maps the kinds this render accepts to whether each is
// cluster-scoped. A kind missing here is refused rather than read as namespaced:
// a namespaced document with no metadata.namespace would otherwise render into
// whatever namespace the applier's context holds.
var openshellObjectScopes = map[string]bool{
	"Namespace":                true,
	"CustomResourceDefinition": true,
	"ClusterRole":              true,
	"ClusterRoleBinding":       true,
	"ServiceAccount":           false,
	"Role":                     false,
	"RoleBinding":              false,
	"ConfigMap":                false,
	"Secret":                   false,
	"Service":                  false,
	"StatefulSet":              false,
	"Deployment":               false,
	"Job":                      false,
	"PersistentVolumeClaim":    false,
}

// certgenSecretFlags are the certgen hook Job's arguments that name the
// Secrets it creates, in the order Secrets lists those names.
var certgenSecretFlags = []string{
	"--server-secret-name",
	"--client-secret-name",
	"--jwt-secret-name",
}

// Runner runs one command and returns its output. An error means the command
// did not run or exited non-zero, and it carries what the command wrote to
// stderr: the render wraps that error and never echoes the output, which can
// hold a rendered Secret.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Chart is what the OpenShell render returns: the objects to apply, the names
// of the Secrets that must never be applied, and the hook objects it dropped.
type Chart struct {
	// Objects are the chart's documents in helm output order, without hook
	// documents or Secrets, with the gateway StatefulSet's document re-encoded
	// to carry its Guaranteed resources.
	Objects []k8s.StaticObject
	// Secrets names every Secret left out of Objects in helm output order,
	// followed by the three certificate Secrets the certgen Job creates:
	// server, client and jwt. internal/secretpreflight checks that each exists.
	Secrets []string
	// Hooks names the dropped hook objects as "Kind/name", in output order.
	Hooks []string
}

// OpenShell renders the OpenShell chart directory through `helm template`,
// returning its objects with the hook documents and every Secret removed and
// named. ChartVersion and ChartDigest record the release the caller must have
// pulled; a chart directory cannot be checked against the OCI digest here.
//
// Secrets are dropped because with no cluster to look up, `helm template`
// renders the chart's credential-storage-key-encryption-key Secret with a new
// random value (the template calls randBytes unless lookup finds an existing
// one): applying it would replace the live key and make stored credentials
// unreadable. The names serve internal/secretpreflight's existence check;
// Secrets are never rendered, planned or applied.
//
// The hook documents are the certgen pre-install,pre-upgrade Job and its
// ServiceAccount, Role and RoleBinding, which this render never applies. The
// render runs no verb but `helm template` and never `helm upgrade`: an upgrade
// regenerates the CA and every certificate. The first install stays a helm
// install inside the rebuild sitting; this render is for plain apply after it.
//
// The gateway StatefulSet is sized Guaranteed on the decoded object: every
// container and init container gets requests equal to limits of gatewayCPU and
// gatewayMemory. A chart output without that StatefulSet is refused rather than
// returned unsized.
func OpenShell(ctx context.Context, run Runner, chartDir, valuesPath string) (Chart, error) {
	if run == nil {
		return Chart{}, errors.New("render the OpenShell chart: no runner, so helm cannot be run")
	}
	for _, path := range []struct{ what, value string }{
		{"chart directory", chartDir},
		{"values file", valuesPath},
	} {
		if err := checkPathArg(path.what, path.value); err != nil {
			return Chart{}, fmt.Errorf("render the OpenShell chart: %w", err)
		}
	}
	out, err := run(ctx, helmBinary,
		"template", openshellName, chartDir,
		"--namespace", openshellName,
		"-f", valuesPath,
		"--set", "agentSandbox.preflight.enabled=false")
	if err != nil {
		return Chart{}, fmt.Errorf("helm template the OpenShell %s chart: %w", ChartVersion, err)
	}
	chart, err := parseChart(out)
	if err != nil {
		return Chart{}, fmt.Errorf("OpenShell %s chart: %w", ChartVersion, err)
	}
	if err := chart.applyGuaranteed(yaml.Marshal); err != nil {
		return Chart{}, fmt.Errorf("OpenShell %s chart: %w", ChartVersion, err)
	}
	return chart, nil
}

// checkPathArg refuses an empty path and one helm would read as a flag instead
// of as the chart argument or the value of -f.
func checkPathArg(what, path string) error {
	switch {
	case path == "":
		return fmt.Errorf("no %s given", what)
	case strings.HasPrefix(path, "-"):
		return fmt.Errorf("%s %q starts with '-', so helm would read it as a flag", what, path)
	}
	return nil
}

// parseChart splits helm's output into documents, dropping the hooks and the
// Secrets and collecting the certgen Job's Secret names.
func parseChart(out []byte) (Chart, error) {
	docs := splitDocuments(out)
	if len(docs) == 0 {
		return Chart{}, errors.New("helm template printed no document")
	}
	var chart Chart
	var certgen certgenFlags
	for i, doc := range docs {
		node, err := documentNode(doc)
		if err != nil {
			return Chart{}, fmt.Errorf("document %d: %w", i+1, err)
		}
		object, err := readChartObject(doc, node)
		if err != nil {
			return Chart{}, fmt.Errorf("document %d: %w", i+1, err)
		}
		if hasHookAnnotation(node) {
			chart.Hooks = append(chart.Hooks, object.Kind+"/"+object.Name)
			if object.Kind == "Job" {
				if err := certgen.read(node); err != nil {
					return Chart{}, fmt.Errorf("document %d: hook Job %s: %w", i+1, object.Name, err)
				}
			}
			continue
		}
		if object.Kind == "Secret" {
			chart.Secrets = append(chart.Secrets, object.Name)
			continue
		}
		chart.Objects = append(chart.Objects, object)
	}
	// An output whose every document was dropped would otherwise report
	// success with nothing to apply.
	if len(chart.Objects) == 0 {
		return Chart{}, errors.New("holds no object to apply: every document was a hook or a Secret")
	}
	names, err := certgen.names()
	if err != nil {
		return Chart{}, err
	}
	for _, name := range names {
		if !slices.Contains(chart.Secrets, name) {
			chart.Secrets = append(chart.Secrets, name)
		}
	}
	return chart, nil
}

// applyGuaranteed sizes the gateway StatefulSet Guaranteed and re-encodes its
// document. A chart output without exactly one such StatefulSet is refused: an
// empty patch would otherwise report success with the gateway free to run
// Burstable or BestEffort.
func (c *Chart) applyGuaranteed(marshal func(any) ([]byte, error)) error {
	indexes := make([]int, 0, 1)
	for i, object := range c.Objects {
		if object.Kind == "StatefulSet" && object.Name == gatewayStatefulSet {
			indexes = append(indexes, i)
		}
	}
	switch len(indexes) {
	case 0:
		return fmt.Errorf("holds no StatefulSet named %s to size Guaranteed", gatewayStatefulSet)
	case 1:
	default:
		return fmt.Errorf("holds %d StatefulSets named %s, want one", len(indexes), gatewayStatefulSet)
	}
	index := indexes[0]
	node, err := documentNode(c.Objects[index].Doc)
	if err != nil {
		return fmt.Errorf("StatefulSet %s: %w", gatewayStatefulSet, err)
	}
	if err := patchGateway(node); err != nil {
		return fmt.Errorf("StatefulSet %s: %w", gatewayStatefulSet, err)
	}
	patched, err := marshal(node)
	if err != nil {
		return fmt.Errorf("re-encoding StatefulSet %s: %w", gatewayStatefulSet, err)
	}
	c.Objects[index].Doc = bytes.TrimSpace(patched)
	return nil
}

// patchGateway sizes every container and init container of the gateway's pod
// Guaranteed. A pod with no init containers is sized like any other.
func patchGateway(node *yaml.Node) error {
	podSpec, err := mappingPath(node.Content[0], "spec", "template", "spec")
	if err != nil {
		return err
	}
	if err := sizeGuaranteed(podSpec, "containers", true); err != nil {
		return err
	}
	return sizeGuaranteed(podSpec, "initContainers", false)
}

// sizeGuaranteed replaces the resources of every container in the pod spec's
// sequence under key. An empty container list is refused when required,
// because sizing nothing would otherwise report success.
func sizeGuaranteed(podSpec *yaml.Node, key string, required bool) error {
	containers := mappingValue(podSpec, key)
	if containers == nil {
		if required {
			return fmt.Errorf("spec.template.spec.%s is missing, so the pod would not run Guaranteed", key)
		}
		return nil
	}
	if containers.Kind != yaml.SequenceNode {
		return fmt.Errorf("spec.template.spec.%s is not a sequence", key)
	}
	if required && len(containers.Content) == 0 {
		return fmt.Errorf("spec.template.spec.%s is empty, so the pod would not run Guaranteed", key)
	}
	for _, container := range containers.Content {
		if container.Kind != yaml.MappingNode {
			return fmt.Errorf("spec.template.spec.%s holds an item that is not a mapping", key)
		}
		setGatewayResources(container)
	}
	return nil
}

// setGatewayResources replaces container's resources with requests equal to
// limits of the gateway's pair. The whole field is replaced, as up.sh's JSON
// patch "add" does: a container keeping an extra request would fall out of the
// Guaranteed class (gt-cjm8fu).
func setGatewayResources(container *yaml.Node) {
	resources := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, field := range []string{"requests", "limits"} {
		resources.Content = append(resources.Content,
			scalarNode(field),
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				scalarNode("cpu"), scalarNode(gatewayCPU),
				scalarNode("memory"), scalarNode(gatewayMemory),
			}})
	}
	setMappingNode(container, "resources", resources)
}

// readChartObject reads one document's object and checks the fields the render
// needs to place it.
func readChartObject(doc []byte, node *yaml.Node) (k8s.StaticObject, error) {
	root := node.Content[0]
	apiVersion := scalarValue(root, "apiVersion")
	kind := scalarValue(root, "kind")
	metadata := mappingValue(root, "metadata")
	name := scalarValue(metadata, "name")
	namespace := scalarValue(metadata, "namespace")
	switch {
	case apiVersion == "":
		return k8s.StaticObject{}, errors.New("no apiVersion")
	case kind == "":
		return k8s.StaticObject{}, errors.New("no kind")
	case name == "":
		return k8s.StaticObject{}, errors.New("no metadata.name")
	}
	cluster, known := openshellObjectScopes[kind]
	if !known {
		return k8s.StaticObject{}, fmt.Errorf("kind %s is not one the OpenShell %s render places", kind, ChartVersion)
	}
	if !cluster && namespace == "" {
		return k8s.StaticObject{}, fmt.Errorf("namespaced %s %s has no metadata.namespace", kind, name)
	}
	object := k8s.StaticObject{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Namespace:  namespace,
		Cluster:    cluster,
		Doc:        doc,
	}
	if cluster {
		object.Namespace = ""
	}
	return object, nil
}

// hasHookAnnotation reports whether the document is a helm hook.
func hasHookAnnotation(node *yaml.Node) bool {
	metadata := mappingValue(node.Content[0], "metadata")
	annotations := mappingValue(metadata, "annotations")
	return scalarValue(annotations, hookAnnotation) != ""
}

// certgenFlags collects the Secret names the certgen hook Job creates.
type certgenFlags struct {
	values map[string]string
}

// read takes the flag values from the Job's container arguments. Both
// `--flag=value` and `--flag value` are helm chart spellings.
func (f *certgenFlags) read(node *yaml.Node) error {
	podSpec, err := mappingPath(node.Content[0], "spec", "template", "spec")
	if err != nil {
		return err
	}
	containers := mappingValue(podSpec, "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode || len(containers.Content) == 0 {
		return errors.New("spec.template.spec.containers is missing or empty, so the Job creates no named Secret this render can see")
	}
	if f.values == nil {
		f.values = make(map[string]string)
	}
	for _, container := range containers.Content {
		args := mappingValue(container, "args")
		if args == nil {
			continue
		}
		if args.Kind != yaml.SequenceNode {
			return errors.New("a container's args is not a sequence")
		}
		values := make([]string, 0, len(args.Content))
		for _, arg := range args.Content {
			if arg.Kind != yaml.ScalarNode {
				return errors.New("a container arg is not a scalar")
			}
			values = append(values, arg.Value)
		}
		if err := f.add(values); err != nil {
			return err
		}
	}
	return nil
}

// add takes every certgen flag from one container's argument list, refusing an
// empty value or a second value for the same flag.
func (f *certgenFlags) add(args []string) error {
	for i := 0; i < len(args); i++ {
		var flag, value string
		for _, want := range certgenSecretFlags {
			switch {
			case args[i] == want:
				if i+1 >= len(args) {
					return fmt.Errorf("%s names no value", want)
				}
				i++
				flag, value = want, args[i]
			case strings.HasPrefix(args[i], want+"="):
				flag, value = want, strings.TrimPrefix(args[i], want+"=")
			default:
				continue
			}
			break
		}
		if flag == "" {
			continue
		}
		if value == "" {
			return fmt.Errorf("%s names an empty Secret", flag)
		}
		if seen, ok := f.values[flag]; ok && seen != value {
			return fmt.Errorf("%s names both %s and %s", flag, seen, value)
		}
		f.values[flag] = value
	}
	return nil
}

// names returns the three Secret names in flag order, refusing a set the Job
// never gave: an unnamed Secret would be absent from the existence check.
func (f *certgenFlags) names() ([]string, error) {
	names := make([]string, 0, len(certgenSecretFlags))
	for _, flag := range certgenSecretFlags {
		name, ok := f.values[flag]
		if !ok {
			return nil, fmt.Errorf("the certgen hook Job gives no %s argument, so the Secret it creates cannot be named for the existence check", flag)
		}
		names = append(names, name)
	}
	return names, nil
}
