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
// (internal/gtwrap/forwarder.go). The pusher reads a third, BD_GATEWAY_PUSH_URL,
// the URL of the Service that publishes the gateway's push listener
// (internal/daemon/workspace_reconcile.go). A pod names the file inside
// BdGatewayTokenDir, a read-only mount of one Secret that holds the token under
// BdGatewayTokenKey and nothing else. The render names each Secret and never
// creates it: the operator creates the tokens and their Secrets before the apply
// (bd-container design, O2).
const (
	// BdGatewayURLEnv and BdGatewayTokenFileEnv are the forwarder's two names, and
	// BdGatewayPushURLEnv is the pusher's.
	BdGatewayURLEnv       = "BD_GATEWAY_URL"
	BdGatewayTokenFileEnv = "BD_GATEWAY_TOKEN_FILE" //nolint:gosec // an environment name, not a credential
	BdGatewayPushURLEnv   = "BD_GATEWAY_PUSH_URL"
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
	// BdGatewayServicePolicyFile is the Service admission policy that admits the
	// push Service's name on its one port and refuses every other Service that
	// selects the gateway pods. A push URL needs it applied, or the name the
	// plain-http bearer travels to is bounded by nothing.
	BdGatewayServicePolicyFile = "deploy/k8s/bd-gateway-service-admission.yaml"
)

// validateBdGateway refuses the dashboard's gateway values a pod cannot use. The
// dashboard reads both URLs, so they need Dashboard. A push URL also needs the
// gateway workload: without it the render writes no push Service, so the address
// names no pod.
func (v Values) validateBdGateway() error {
	if v.BdGatewayURL == "" && v.BdGatewayPushURL == "" {
		return nil
	}
	if !v.Dashboard {
		return errors.New("Values.BdGatewayURL or Values.BdGatewayPushURL is set but Values.Dashboard is not: nothing in the pod reads the gateway settings, so the URL would be dropped")
	}
	if v.BdGatewayPushURL != "" && v.BdGatewayImage == "" {
		return errors.New("Values.BdGatewayPushURL is set while Values.BdGatewayImage is empty: the push Service renders only with the gateway workload, so the URL would name no pod")
	}
	if v.BdGatewayPushURL != "" {
		if err := v.pushPolicyRefusal("Values.BdGatewayPushURL"); err != nil {
			return err
		}
	}
	for _, field := range []struct{ name, value string }{
		{"Values.BdGatewayURL", v.BdGatewayURL},
		{"Values.BdGatewayPushURL", v.BdGatewayPushURL},
	} {
		if field.value == "" {
			continue
		}
		if err := ValidateBdGatewayURL(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

// validateDaemonBdGateway refuses the daemon's gateway values a pod cannot use. A
// rehearsal mounts no token, so it refuses the URL rather than render a gateway
// the pod cannot read. A push URL also needs the gateway workload, which renders
// the push Service.
func (v Values) validateDaemonBdGateway() error {
	if v.DaemonBdGatewayURL == "" && v.DaemonBdGatewayPushURL == "" {
		return nil
	}
	if v.Rehearsal {
		return errors.New("Values.DaemonBdGatewayURL or Values.DaemonBdGatewayPushURL is set with Values.Rehearsal: the gateway token is a live-pod credential, and a rehearsal mounts none")
	}
	if v.DaemonBdGatewayPushURL != "" && v.BdGatewayImage == "" {
		return errors.New("Values.DaemonBdGatewayPushURL is set while Values.BdGatewayImage is empty: the push Service renders only with the gateway workload, so the URL would name no pod")
	}
	if v.DaemonBdGatewayPushURL != "" {
		if err := v.pushPolicyRefusal("Values.DaemonBdGatewayPushURL"); err != nil {
			return err
		}
	}
	for _, field := range []struct{ name, value string }{
		{"Values.DaemonBdGatewayURL", v.DaemonBdGatewayURL},
		{"Values.DaemonBdGatewayPushURL", v.DaemonBdGatewayPushURL},
	} {
		if field.value == "" {
			continue
		}
		if err := ValidateBdGatewayURL(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

// pushPolicyRefusal refuses a push URL unless the Service admission policy that
// bounds the push Service's name is in this render (Values.IncludeStatic) or the
// operator attests it is applied (Values.BdGatewayServicePolicyApplied). The
// render cannot verify the cluster, so the message names both ways out and the
// policy file; a push URL with neither would carry the write-class bearer over
// plain http to a Service name trusted by label alone.
func (v Values) pushPolicyRefusal(field string) error {
	if v.IncludeStatic || v.BdGatewayServicePolicyApplied {
		return nil
	}
	return fmt.Errorf("%s is set without Values.IncludeStatic or Values.BdGatewayServicePolicyApplied: the push URL sends the gateway's bearer token over plain http to a Service name that %s guards, so the render refuses it unless that policy is in the same render (Values.IncludeStatic) or the operator attests it is applied (Values.BdGatewayServicePolicyApplied)", field, BdGatewayServicePolicyFile)
}

// The gateway's own workload, rendered when Values.BdGatewayImage is set
// (bdgateway_workload.go). Its tokens are one Secret the operator creates, named by
// Values.BdGatewayTokenSecret; the render names it and never creates it.
const (
	// BdGatewayName names the gateway's Deployment, its container and its Service.
	BdGatewayName = "bd-gateway"
	// BdGatewayPort is the port the gateway's Service publishes in the cluster.
	BdGatewayPort = 7440
	// BdGatewayPushName names the Service that publishes the gateway's push
	// listener, and BdGatewayPushPort is its port.
	BdGatewayPushName = "bd-gateway-push"
	BdGatewayPushPort = 7441
	// BdGatewayWorkspaceClaim is the claim the gateway's workspace lives on, and
	// BdGatewayWorkspaceSize is its request.
	BdGatewayWorkspaceClaim = "bd-gateway-workspace"
	BdGatewayWorkspaceSize  = "1Gi"
	// BdGatewayLogClaim is the claim the gateway's call log lives on, and
	// BdGatewayLogSize is its request. It is its own claim so a full log cannot
	// make the workspace claim, and the gateway's readiness, fail.
	BdGatewayLogClaim = "bd-gateway-log"
	BdGatewayLogSize  = "512Mi"
)

// validateBdGatewayWorkload refuses the gateway workload values the cluster cannot
// run. The image is a digest, as the daemon's is, so a tag cannot roll a build the
// values file does not name. The token Secret is required with the image: without
// it the gateway starts with no tokens and refuses every client. The gateway also
// needs Values.HostGatewayIP, since its Dolt host resolves only through that
// address's hostAliases entry. The token-copy init container runs Values.ProxyImage,
// so the gateway needs that too. A negative concurrency cap is refused even with no
// image. With no image the workload is not rendered, and the gateway's token Secret
// name is not read.
func (v Values) validateBdGatewayWorkload() error {
	if v.BdGatewayMaxConcurrent < 0 {
		return fmt.Errorf("Values.BdGatewayMaxConcurrent %d: want zero, which leaves --max-concurrent off so the gateway's own default applies, or a positive cap", v.BdGatewayMaxConcurrent)
	}
	if v.BdGatewayImage == "" {
		return nil
	}
	if !imageRefRE.MatchString(v.BdGatewayImage) {
		return fmt.Errorf("Values.BdGatewayImage %q: want the bd gateway image by digest (repo@sha256:<64 lowercase hex>), as Values.Image is; a tag is refused so the gateway runs the build the values file records", v.BdGatewayImage)
	}
	if v.BdGatewayTokenSecret == "" {
		return errors.New("Values.BdGatewayTokenSecret is empty while Values.BdGatewayImage is set: the gateway reads its tokens from that Secret, and with none it refuses every client")
	}
	if !validDNS1123Subdomain(v.BdGatewayTokenSecret) {
		return fmt.Errorf("Values.BdGatewayTokenSecret %q: want a Secret name, a DNS-1123 subdomain", v.BdGatewayTokenSecret)
	}
	// The gateway dials DoltHostName, which resolves only through the hostAliases
	// entry hostAliases builds from Values.HostGatewayIP. A rehearsal builds no
	// entry, so a gateway there would dial a name it cannot resolve.
	if v.HostGatewayIP == "" {
		return fmt.Errorf("Values.HostGatewayIP is empty while Values.BdGatewayImage is set: the gateway dials %s, which resolves only through the hostAliases entry that address builds", DoltHostName)
	}
	if v.Rehearsal {
		return fmt.Errorf("Values.BdGatewayImage is set with Values.Rehearsal: a rehearsal builds no hostAliases entry, so the gateway's dial to %s would not resolve", DoltHostName)
	}
	if v.ProxyImage == "" {
		return errors.New("Values.ProxyImage is empty while Values.BdGatewayImage is set: the gateway's tokens-0600 init container copies its tokens with the proxy image, and an empty one has nothing to run")
	}
	return nil
}

// bdGatewayURL matches an http or https URL whose authority is a host name or an
// IPv4 address with an optional port in 1 to 65535. A path is allowed; a query, a
// fragment, a space, user information and a bracketed IPv6 literal are not. The
// renderer may not import net/url (imports_test.go), so the form is matched here.
var bdGatewayURL = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:(6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3}))?(/[^?#\s]*)?$`)

// ValidateBdGatewayURL accepts a gateway URL of the forwarder's form for the named
// field, so a render that carries the gateway refuses one the pod cannot use. The
// URL is not echoed in the error: a credential may sit in it, and the token file
// is the only credential the pod should carry.
func ValidateBdGatewayURL(field, raw string) error {
	_, rest, _ := strings.Cut(raw, "://")
	authority, _, _ := strings.Cut(rest, "/")
	if strings.Contains(authority, "@") {
		return fmt.Errorf("%s: the URL names a user, and the bearer token in %s is the only credential the pod carries", field, BdGatewayTokenPath)
	}
	if !bdGatewayURL.MatchString(raw) {
		return fmt.Errorf("%s: want an http or https URL with a host, such as http://host.docker.internal:7440", field)
	}
	return nil
}

// BdGatewayEnv is the forwarder's environment pair for a pod whose gateway URL is
// url: the URL, and the token file BdGatewayMount puts in place.
func BdGatewayEnv(url string) []EnvVar {
	return []EnvVar{
		{Name: BdGatewayURLEnv, Value: url},
		{Name: BdGatewayTokenFileEnv, Value: BdGatewayTokenPath},
	}
}

// BdGatewayPushEnv is the pusher's environment entry for a pod whose push Service
// URL is url. The value is a URL the render validated, never a credential.
func BdGatewayPushEnv(url string) []EnvVar {
	return []EnvVar{{Name: BdGatewayPushURLEnv, Value: url}}
}

// BdGatewayVolume is the Secret volume that carries one gateway token. Its items
// name only BdGatewayTokenKey, so a key added to the Secret later is not mounted,
// and its mode is owner-read only.
func BdGatewayVolume(secret string) Volume {
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

// BdGatewayMount places the token directory read-only in the container.
func BdGatewayMount() VolumeMount {
	return VolumeMount{Name: bdGatewayVolumeName, MountPath: BdGatewayTokenDir, ReadOnly: true}
}
