package daemon

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The bd gateway's client side in a pod. The forwarder installed as bd reads two
// environment names: BD_GATEWAY_URL, the gateway's base URL, and
// BD_GATEWAY_TOKEN_FILE, the file that holds its bearer token
// (internal/gtwrap/forwarder.go). A pod names the file inside BdGatewayTokenDir,
// a read-only mount of one Secret that holds the token under BdGatewayTokenKey and
// nothing else. The render names each Secret and never creates it: the operator
// creates the tokens and their Secrets before the apply (bd-container design, O2).
const (
	// BdGatewayURLEnv and BdGatewayTokenFileEnv are the forwarder's two names.
	BdGatewayURLEnv       = "BD_GATEWAY_URL"
	BdGatewayTokenFileEnv = "BD_GATEWAY_TOKEN_FILE" //nolint:gosec // an environment name, not a credential
	// BdGatewayTokenDir is where the token Secret is mounted, and
	// BdGatewayTokenPath is the token file inside it.
	BdGatewayTokenDir  = "/run/gt/bd-gateway" //nolint:gosec // a directory path, not a credential
	BdGatewayTokenKey  = "token"              //nolint:gosec // a Secret key name, not a credential
	BdGatewayTokenPath = BdGatewayTokenDir + "/" + BdGatewayTokenKey
	// BdGatewayDashboardSecretName is the read-class token the dashboard mounts.
	// BdGatewayDaemonSecretName is the daemon's own token; no manifest mounts it
	// into the dashboard (TestDashboardSecretIsItsOwn).
	BdGatewayDashboardSecretName = "bd-gateway-dashboard" //nolint:gosec // a Secret name, not a credential
	BdGatewayDaemonSecretName    = "bd-gateway-daemon"    //nolint:gosec // a Secret name, not a credential
	bdGatewayVolumeName          = "bd-gateway-token"     //nolint:gosec // a volume name, not a credential
	bdGatewayTokenMode           = int32(0o400)
)

// validateBdGateway refuses the gateway values a pod cannot use. The URL is only
// read by the dashboard today, so it needs Dashboard; a daemon that reads it
// later widens this check with its own.
func (v Values) validateBdGateway() error {
	if v.BdGatewayURL == "" {
		return nil
	}
	if !v.Dashboard {
		return errors.New("Values.BdGatewayURL is set but Values.Dashboard is not: nothing in the pod reads the gateway settings, so the URL would be dropped")
	}
	return validateBdGatewayURL(v.BdGatewayURL)
}

// bdGatewayURL matches an http or https URL whose authority is a host name or an
// IPv4 address with an optional port. A path is allowed; a query, a fragment, a
// space, user information and a bracketed IPv6 literal are not. The renderer may
// not import net/url (imports_test.go), so the form is matched here.
var bdGatewayURL = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[^?#\s]*)?$`)

// validateBdGatewayURL accepts a URL of bdGatewayURL's form. The URL is not
// echoed in the error: a credential may sit in it, and the token file is the only
// credential the pod should carry.
func validateBdGatewayURL(raw string) error {
	_, rest, _ := strings.Cut(raw, "://")
	authority, _, _ := strings.Cut(rest, "/")
	if strings.Contains(authority, "@") {
		return fmt.Errorf("Values.BdGatewayURL: the URL names a user, and the bearer token in %s is the only credential the pod carries", BdGatewayTokenPath)
	}
	if !bdGatewayURL.MatchString(raw) {
		return errors.New("Values.BdGatewayURL: want an http or https URL with a host, such as http://host.docker.internal:7440")
	}
	return nil
}

// bdGatewayEnv is the forwarder's environment pair for a pod whose gateway URL is
// url: the URL, and the token file the mount puts in place.
func bdGatewayEnv(url string) []EnvVar {
	return []EnvVar{
		{Name: BdGatewayURLEnv, Value: url},
		{Name: BdGatewayTokenFileEnv, Value: BdGatewayTokenPath},
	}
}

// bdGatewayVolume is the Secret volume that carries one gateway token. Its items
// name only BdGatewayTokenKey, so a key added to the Secret later is not mounted,
// and its mode is owner-read only.
func bdGatewayVolume(secret string) Volume {
	mode := bdGatewayTokenMode
	return Volume{
		Name: bdGatewayVolumeName,
		Secret: &SecretVolumeSource{
			SecretName:  secret,
			DefaultMode: &mode,
			Items:       []KeyToPath{{Key: BdGatewayTokenKey, Path: BdGatewayTokenKey}},
		},
	}
}

// bdGatewayMount places the token directory read-only in the container.
func bdGatewayMount() VolumeMount {
	return VolumeMount{Name: bdGatewayVolumeName, MountPath: BdGatewayTokenDir, ReadOnly: true}
}
