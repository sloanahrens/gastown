// Package sbxcreds builds the gt-sbx deployer credential request and the kubeadm argv that issues it (gt-16pu5p).
package sbxcreds

import (
	"fmt"
	"regexp"
	"slices"
	"time"
)

// The validity window a request may ask for; kubeadm issues any period.
const minValidity, maxValidity = time.Hour, 8760 * time.Hour // one year

// namePattern is the shape the container, client name and group must have.
var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// dockerExec starts every argv this package builds; it runs no process itself (docs/design/pod-engine.md).
var dockerExec = []string{"docker", "exec"}

// Request is the certificate to issue: the container, the client name, the organization (group) and the validity.
type Request struct {
	Container  string
	ClientName string
	Group      string
	Validity   string
}

// Argv returns the docker exec argv for the request's client, or a refusal; every rule is checked
// before any argv is built and the validity reaches the flag as its parsed duration.
func (r Request) Argv() ([]string, error) {
	for _, f := range []struct{ name, value string }{{"container", r.Container}, {"client name", r.ClientName}, {"group", r.Group}} {
		if !namePattern.MatchString(f.value) {
			return nil, fmt.Errorf("%s %q is not lowercase letters, digits and dashes", f.name, f.value)
		}
	}
	validity, err := time.ParseDuration(r.Validity)
	if err != nil {
		return nil, fmt.Errorf("validity %q is not a duration: %w", r.Validity, err)
	}
	if validity < minValidity || validity > maxValidity {
		return nil, fmt.Errorf("validity %q is outside %s to %s", r.Validity, minValidity, maxValidity)
	}
	return append(slices.Clone(dockerExec), r.Container, "kubeadm", "kubeconfig", "user", "--client-name", r.ClientName,
		"--org", r.Group, "--validity-period", validity.String()), nil
}
