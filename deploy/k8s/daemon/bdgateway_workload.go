package daemon

import (
	"fmt"
	"strconv"
)

// The bd gateway's pod: one replica of bdgw serve, reading its tokens from a
// directory an init container fills from the token Secret the operator creates, and
// keeping its workspace on a claim. It runs as user and group 65532 and may not gain
// privilege.
//
// fsGroup is 65532 as well, and the token Secret volume is group-readable (0440): the
// Secret is root-owned, so the init container, which runs as 65532, reads the tokens
// through the group, not the owner. A 0400 volume would leave that copy unable to
// read any token.
//
// The gateway's token loader refuses any token file whose mode is not exactly 0600,
// and the 0440 Secret mode is the one fsGroup needs. So the init container copies
// each token into an emptyDir at 0600, and the gateway reads that copy.
const (
	// bdGatewayIdentity is the uid and gid the gateway runs as.
	bdGatewayIdentity = int64(65532)
	// bdGatewayTokensDir is where the gateway reads its tokens: the emptyDir the init
	// container fills, mounted read-only.
	bdGatewayTokensDir = "/run/bdgw/tokens" //nolint:gosec // a directory path, not a credential
	// bdGatewayTokenSecretDir is where the token Secret is mounted, read-only, in the
	// init container only; bdGatewayTokenCopyDir is the emptyDir it copies into.
	bdGatewayTokenSecretDir = "/secrets"
	bdGatewayTokenCopyDir   = "/tokens"
	// bdGatewayTokenInit names the init container that copies the tokens at 0600.
	bdGatewayTokenInit = "tokens-0600" //nolint:gosec // a container name, not a credential
	// bdGatewayLogDir is where the gateway writes its call log, on the log claim.
	bdGatewayLogDir = "/var/log/bdgw"
	// bdGatewayWorkspaceMount is the claim's mount; the gateway's workspace is the
	// ws directory under it.
	bdGatewayWorkspaceMount = "/var/lib/bdgw"
	bdGatewayWorkspaceDir   = bdGatewayWorkspaceMount + "/ws"
	// bdGatewayVerbs is the verb table the command reads.
	bdGatewayVerbs = "/etc/bdgw/verbs.json"
	// bdGatewayTokenSecretVolume is the Secret the init container reads, and
	// bdGatewayTokensVolume the emptyDir it fills and the gateway mounts; bdGatewayLogVolume
	// is the log claim's volume, and the workspace claim is named by BdGatewayWorkspaceClaim.
	bdGatewayTokenSecretVolume = "bd-gateway-token-secret" //nolint:gosec // a volume name, not a credential
	bdGatewayTokensVolume      = "bd-gateway-tokens"       //nolint:gosec // a volume name, not a credential
	bdGatewayLogVolume         = "bd-gateway-log"
	// bdGatewayTokenSecretMode is owner and group read, so fsGroup lets the init
	// container read the Secret.
	bdGatewayTokenSecretMode = int32(0o440)
	// bdGatewayDoltPort is the Dolt port the gateway dials at DoltHostName.
	bdGatewayDoltPort = 3307
	// bdGatewayHealthPath is the gateway's /healthz path, which answers with no token.
	bdGatewayHealthPath = "/healthz"
	// bdGatewayPushPortName names the push listener's port on the container and its
	// Service, which publishes the pod before it is ready.
	bdGatewayPushPortName = "push"
)

// bdGatewayDeployment is the gateway's pod: an init container that copies the tokens
// to 0600, one container running bdgw serve, the emptyDir for tokens, the log claim
// and the workspace claim. It takes no ServiceAccount token, as the dashboard's pod
// does not.
//
// Its strategy is RollingUpdate with maxUnavailable 0 and maxSurge 1: the new gateway
// starts, and passes its readiness, before the old one stops, so the gateway has no
// gap in service. For the length of a roll two gateway processes share the workspace
// claim and the log claim. That is safe because the workspace push takes a
// cross-process lock (gt-fo7rf0), the overseer checked bd's runtime files under two
// pods (gt-yz0s8v), and --log-per-pod gives each pod its own call log directory.
// Replicas stay at one.
func bdGatewayDeployment(v Values, namespace string) Deployment {
	automount := false
	user := bdGatewayIdentity
	group := bdGatewayIdentity
	fsGroup := bdGatewayIdentity
	maxUnavailable := int32(0)
	maxSurge := int32(1)
	return Deployment{
		TypeMeta: TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		Metadata: ObjectMeta{Name: BdGatewayName, Namespace: namespace, Labels: bdGatewayLabels()},
		Spec: DeploymentSpec{
			Replicas: 1,
			Strategy: DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &RollingUpdateStrategy{MaxUnavailable: &maxUnavailable, MaxSurge: &maxSurge},
			},
			Selector: LabelSelector{MatchLabels: bdGatewaySelectorLabels()},
			Template: PodTemplate{
				Metadata: PodTemplateMeta{Labels: bdGatewayLabels()},
				Spec: PodSpec{
					AutomountServiceAccountToken: &automount,
					SecurityContext: &PodSecurityContext{
						RunAsUser:  &user,
						RunAsGroup: &group,
						FSGroup:    &fsGroup,
					},
					HostAliases:    hostAliases(v),
					InitContainers: []Container{bdGatewayTokenCopy(v)},
					Containers:     []Container{bdGatewayContainer(v, DoltHostName, bdGatewayDoltPort)},
					Volumes:        bdGatewayVolumes(v),
				},
			},
		},
	}
}

// bdGatewayTokenCopy is the init container that copies each token from the Secret
// into the emptyDir at mode 0600, the mode the gateway's token loader requires. It
// runs Values.ProxyImage, the image the daemon's relays already run, and as the pod's
// user, so it reads the Secret through its group.
func bdGatewayTokenCopy(v Values) Container {
	allowEscalation := false
	return Container{
		Name:    bdGatewayTokenInit,
		Image:   v.ProxyImage,
		Command: []string{"sh", "-c", fmt.Sprintf("cp %s/* %s/ && chmod 0600 %s/*", bdGatewayTokenSecretDir, bdGatewayTokenCopyDir, bdGatewayTokenCopyDir)},
		SecurityContext: &ContainerSecurityContext{
			AllowPrivilegeEscalation: &allowEscalation,
		},
		VolumeMounts: []VolumeMount{
			{Name: bdGatewayTokenSecretVolume, MountPath: bdGatewayTokenSecretDir, ReadOnly: true},
			{Name: bdGatewayTokensVolume, MountPath: bdGatewayTokenCopyDir},
		},
	}
}

// bdGatewayContainer runs bdgw serve with the command the gateway's image expects.
// Its Dolt endpoint is doltHost and doltPort, which bdGatewayDeployment passes as
// DoltHostName and bdGatewayDoltPort; the serve command and the readiness probe both
// take that one pair, so they cannot name different Dolt endpoints. The host resolves
// through the pod's hostAliases entry when the render has a host gateway address.
// Its readiness is bdgw ready on the tokens, the verb table and the workspace, the
// three flags bdgw ready requires, plus the same Dolt endpoint, so a gateway whose
// workspace is not ready, or whose Dolt does not answer at that endpoint, stays out
// of bdGatewayService. The push Service publishes it either way, since an unready
// gateway can still answer a push.
func bdGatewayContainer(v Values, doltHost string, doltPort int) Container {
	allowEscalation := false
	dolt := []string{"--dolt-host", doltHost, "--dolt-port", strconv.Itoa(doltPort)}
	command := append([]string{"bdgw", "serve", "--tokens-dir", bdGatewayTokensDir, "--verbs", bdGatewayVerbs, "--log-dir", bdGatewayLogDir, "--log-per-pod", "--workspace", bdGatewayWorkspaceDir}, dolt...)
	command = append(command, "--push-addr", ":"+strconv.Itoa(BdGatewayPushPort))
	if v.BdGatewayMaxConcurrent > 0 {
		command = append(command, "--max-concurrent", strconv.Itoa(v.BdGatewayMaxConcurrent))
	}
	return Container{
		Name:    BdGatewayName,
		Image:   v.BdGatewayImage,
		Command: command,
		// The push listener is the one port the container declares, so the pod's
		// spec names the port bdGatewayPushService forwards to.
		Ports: []ContainerPort{{Name: bdGatewayPushPortName, ContainerPort: BdGatewayPushPort, Protocol: "TCP"}},
		SecurityContext: &ContainerSecurityContext{
			AllowPrivilegeEscalation: &allowEscalation,
		},
		VolumeMounts: []VolumeMount{
			{Name: bdGatewayTokensVolume, MountPath: bdGatewayTokensDir, ReadOnly: true},
			{Name: bdGatewayLogVolume, MountPath: bdGatewayLogDir},
			{Name: BdGatewayWorkspaceClaim, MountPath: bdGatewayWorkspaceMount},
		},
		// The readiness probe does not run until the startup probe passes. /healthz needs
		// no token, so the startup probe passes as soon as the server answers on its port.
		// Its window is 60 checks 2 s apart: a start slower than that restarts the container.
		StartupProbe: &Probe{
			HTTPGet:          &HTTPGetAction{Path: bdGatewayHealthPath, Port: BdGatewayPort},
			PeriodSeconds:    2,
			FailureThreshold: 60,
		},
		ReadinessProbe: &Probe{
			Exec:                &ExecAction{Command: append([]string{"bdgw", "ready", "--tokens-dir", bdGatewayTokensDir, "--verbs", bdGatewayVerbs, "--workspace", bdGatewayWorkspaceDir}, dolt...)},
			InitialDelaySeconds: 2,
			PeriodSeconds:       3,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
	}
}

// bdGatewayVolumes are the gateway's volumes: the token Secret at the group-readable
// mode, the emptyDir the init container fills with the 0600 copies, the log claim,
// and the workspace claim. The Secret is mounted whole, since the init container
// copies a directory of tokens.
func bdGatewayVolumes(v Values) []Volume {
	mode := bdGatewayTokenSecretMode
	return []Volume{
		{Name: bdGatewayTokenSecretVolume, Secret: &SecretVolumeSource{SecretName: v.BdGatewayTokenSecret, DefaultMode: &mode}},
		{Name: bdGatewayTokensVolume, EmptyDir: &EmptyDirVolumeSource{}},
		{Name: bdGatewayLogVolume, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: BdGatewayLogClaim}},
		{Name: BdGatewayWorkspaceClaim, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: BdGatewayWorkspaceClaim}},
	}
}

// bdGatewayService publishes the gateway's port inside the cluster. Its selector is
// the gateway pod's own label, so the other pods are not published.
func bdGatewayService(namespace string) Service {
	return Service{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Service"},
		Metadata: ObjectMeta{Name: BdGatewayName, Namespace: namespace, Labels: bdGatewayLabels()},
		Spec: ServiceSpec{
			Type:     "ClusterIP",
			Selector: bdGatewaySelectorLabels(),
			Ports:    []ServicePort{{Name: "http", Port: BdGatewayPort, Protocol: "TCP"}},
		},
	}
}

// bdGatewayPushService publishes the gateway's push listener. It selects the same
// pods as bdGatewayService, and it publishes them before they are ready, because an
// unready gateway can still answer a push; bdGatewayService keeps publishing ready
// pods only, so a bd exec never reaches a gateway whose workspace is not ready.
func bdGatewayPushService(namespace string) Service {
	return Service{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Service"},
		Metadata: ObjectMeta{Name: BdGatewayPushName, Namespace: namespace, Labels: bdGatewayLabels()},
		Spec: ServiceSpec{
			Type:                     "ClusterIP",
			Selector:                 bdGatewaySelectorLabels(),
			Ports:                    []ServicePort{{Name: bdGatewayPushPortName, Port: BdGatewayPushPort, Protocol: "TCP"}},
			PublishNotReadyAddresses: true,
		},
	}
}

// bdGatewaySelectorLabels are what the gateway's Deployment and Service select its
// pods by: its own name, which no other pod in the render carries.
func bdGatewaySelectorLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": BdGatewayName}
}

// bdGatewayLabels are the labels on every gateway object and its pod.
func bdGatewayLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       BdGatewayName,
		"app.kubernetes.io/managed-by": "gt",
	}
}
