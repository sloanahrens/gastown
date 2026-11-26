package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/sbxcreds"
)

// sbxControlPlaneNode is the kind node that runs the gt-sbx control plane.
const sbxControlPlaneNode = "gt-sbx-control-plane"

// deployerRole is one gt-sbx deployer role and the most its certificate may hold: apply 30 days and
// policy one day, the limits gt town deployer csr enforces, and read one day.
type deployerRole struct{ name, validity string }

// deployerRoles lists the roles in the order the refusals name them.
var deployerRoles = []deployerRole{{"read", "24h"}, {"policy", "24h"}, {"apply", "720h"}}

// credentialRequest is the argument rule of gt town cluster credentials: the request for one deployer role
// on gt-sbx, or a refusal naming the allowed values. out is the kubeconfig path the command writes; the
// request does not carry it, so only its presence is checked.
func credentialRequest(cluster, role, out string) (sbxcreds.Request, error) {
	var names []string
	validity := ""
	for _, r := range deployerRoles {
		names = append(names, r.name)
		if r.name == role {
			validity = r.validity
		}
	}
	allowed := strings.Join(names, ", ")
	switch {
	case cluster != clusterSbx:
		return sbxcreds.Request{}, fmt.Errorf("cluster %q is not allowed: the only cluster is %s", cluster, clusterSbx)
	case role == "":
		return sbxcreds.Request{}, fmt.Errorf("a missing role: the roles are %s", allowed)
	case validity == "":
		return sbxcreds.Request{}, fmt.Errorf("unknown role %q: the roles are %s", role, allowed)
	case out == "":
		return sbxcreds.Request{}, errors.New("a missing out: the kubeconfig path to write is required")
	}
	name := "gt-deployer-" + role
	return sbxcreds.Request{Container: sbxControlPlaneNode, ClientName: name, Group: name, Validity: validity}, nil
}
