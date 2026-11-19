package townreboot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// polecatManifestAccept names both manifest types the polecat image can be
// stored as, OCI or Docker v2, so the dev tag answers with its manifest
// whichever shape the build pushed (gt-plvm2h).
const polecatManifestAccept = "application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// polecatImageFix is the repair when the daemon Deployment's annotation and the
// registry disagree: render the Deployment on the digest the registry serves
// (gt-plvm2h).
const polecatImageFix = "gt town cluster render --polecat-image on the digest the registry answers"

// bdGatewayFix is the repair when the gateway has no ready replica. A pod that
// fails readiness has no Service endpoint, so nothing reaches the gateway until
// one is ready; a push goes through the push Service once it exists, and the
// exec-pod route until then (gt-plvm2h).
const bdGatewayFix = "gt bd workspace push through the push Service once it exists " +
	"(bead: bd gateway push Service), else the exec-pod route"

// daemonDeployment is the part of the daemon Deployment the image check reads.
// The polecat image travels only on the pod template's annotations, so the
// container's image field says nothing about it.
type daemonDeployment struct {
	Spec struct {
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
}

// gatewayDeployment is the part of the bd-gateway Deployment the gateway check
// reads. A Deployment that never became ready carries no readyReplicas, which
// decodes as 0: the same no-ready-pod state the check reports.
type gatewayDeployment struct {
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

// ImageGatewayChecks returns the operations doc's checks 7 and 8 in order: the
// polecat image digest the daemon Deployment names against the digest the
// registry serves, then the bd gateway Deployment's ready replicas
// (gt-plvm2h). registryBase is the registry root the caller resolved, scheme
// included; this package never spells one.
func ImageGatewayChecks(run Runner, registryBase string) []Check {
	return []Check{polecatImageCheck(run, registryBase), bdGatewayCheck(run)}
}

// registryManifestURL is the polecat image's dev manifest under registryBase. A
// base that is not an http(s) root is refused here, before its text reaches
// curl's arguments, where a value starting with "-" would be read as an option.
// The refusal names no part of the base: a base carrying credentials must not
// put them in a row.
func registryManifestURL(registryBase string) (string, error) {
	if registryBase == "" {
		return "", errors.New("no registry base configured")
	}
	u, err := url.Parse(registryBase)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("the registry base is not an http(s) root: want a URL with a host and no path, query, credentials or fragment")
	}
	return strings.TrimSuffix(registryBase, "/") + "/v2/" + imagevalues.PolecatImage + "/manifests/dev", nil
}

// polecatImageCheck reads the daemon Deployment's polecat image annotation and
// the digest the registry answers for that image, and passes when they name the
// same digest (gt-plvm2h).
func polecatImageCheck(run Runner, registryBase string) Check {
	return Check{
		Name: "7 polecat image",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-k3bf8x).
				return Result{Detail: "no runner configured"}
			}
			manifestURL, err := registryManifestURL(registryBase)
			if err != nil {
				return Result{Detail: err.Error()}
			}
			out, err := run(ctx, "kubectl", "--context", "colima-gt-control",
				"-n", k8sdaemon.DefaultNamespace, "get", "deployment", k8sdaemon.Name, "-o", "json")
			if err != nil {
				// The Deployment is unknown, so the row says what failed rather
				// than comparing against an annotation it never read.
				return Result{Detail: fmt.Sprintf("read deployment %s: %v", k8sdaemon.Name, err)}
			}
			var d daemonDeployment
			if err := json.Unmarshal(out, &d); err != nil {
				return Result{Detail: fmt.Sprintf("parse deployment %s: %v", k8sdaemon.Name, err)}
			}
			annotation := d.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]
			if annotation == "" {
				// A Deployment rendered without the annotation runs no polecat
				// image the check could compare.
				return Result{
					Detail: "deployment " + k8sdaemon.Name + " has no " + k8sdaemon.PolecatImageAnnotation + " annotation",
					Fix:    polecatImageFix,
				}
			}
			if !imagevalues.ValidRef(annotation) {
				return Result{
					Detail: fmt.Sprintf("deployment %s names polecat image %q, which is not repo@sha256:<64 hex>", k8sdaemon.Name, annotation),
					Fix:    polecatImageFix,
				}
			}
			// ValidRef admitted one "@", so the half after it is the digest.
			_, want, _ := strings.Cut(annotation, "@")
			out, err = run(ctx, "curl", "-sI", "-H", "Accept: "+polecatManifestAccept, manifestURL)
			if err != nil {
				return Result{Detail: fmt.Sprintf("read the registry digest: %v", err)}
			}
			got, found := headerValue(string(out), "Docker-Content-Digest")
			if !found {
				return Result{Detail: "the registry answered no Docker-Content-Digest header"}
			}
			if !imagevalues.ValidDigest(got) {
				return Result{Detail: fmt.Sprintf("the registry answered digest %q, which is not sha256:<64 hex>", got)}
			}
			if got != want {
				return Result{
					Detail: fmt.Sprintf("the annotation names %s, the registry answers %s", want, got),
					Fix:    polecatImageFix,
				}
			}
			return Result{OK: true, Detail: "the annotation and the registry both name " + want}
		},
	}
}

// headerValue reads the value of the response header named want from curl -sI's
// output. The name is matched without case, as HTTP header names are, and a
// header line with no colon is skipped rather than read as a name.
func headerValue(headers, want string) (string, bool) {
	for _, line := range strings.Split(headers, "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.EqualFold(strings.TrimSpace(name), want) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// bdGatewayCheck reads the bd gateway Deployment's ready replicas and passes at
// one or more (gt-plvm2h). A town before the gateway cutover has no Deployment,
// which the check reports as not deployed rather than as a failure.
func bdGatewayCheck(run Runner) Check {
	return Check{
		Name: "8 bd gateway",
		Run: func(ctx context.Context) Result {
			if run == nil {
				return Result{Detail: "no runner configured"}
			}
			out, err := run(ctx, "kubectl", "--context", "colima-gt-control",
				"-n", k8sdaemon.DefaultNamespace, "get", "deployment", k8sdaemon.BdGatewayName,
				"-o", "json", "--ignore-not-found")
			if err != nil {
				return Result{Detail: fmt.Sprintf("read deployment %s: %v", k8sdaemon.BdGatewayName, err)}
			}
			if strings.TrimSpace(string(out)) == "" {
				return Result{OK: true, Detail: "not deployed"}
			}
			var d gatewayDeployment
			if err := json.Unmarshal(out, &d); err != nil {
				return Result{Detail: fmt.Sprintf("parse deployment %s: %v", k8sdaemon.BdGatewayName, err)}
			}
			if d.Status.ReadyReplicas < 1 {
				return Result{
					Detail: "no ready replica: a pod that fails readiness has no Service endpoint",
					Fix:    bdGatewayFix,
				}
			}
			return Result{OK: true, Detail: fmt.Sprintf("readyReplicas %d", d.Status.ReadyReplicas)}
		},
	}
}
