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

// BdGatewayMaxReplicas is the most gateway pods a render runs: two, so one serves
// while the other rolls or fails.
const BdGatewayMaxReplicas = 2

// validateBdGatewayReplicas refuses a replica count the gateway does not run: a
// negative count, or more than BdGatewayMaxReplicas. It runs with or without the
// gateway image, as the concurrency cap's refusal does, so a value is refused rather
// than ignored.
func (v Values) validateBdGatewayReplicas() error {
	if v.BdGatewayReplicas < 0 || v.BdGatewayReplicas > BdGatewayMaxReplicas {
		return fmt.Errorf("Values.BdGatewayReplicas %d: want zero or one for one gateway pod, or two, the most the gateway runs", v.BdGatewayReplicas)
	}
	return nil
}

// validateBdGatewayCPURequest refuses a CPU request cpuRE does not read: whole cores or
// millicores, positive. Empty is no request. It runs with or without the gateway
// image, as the replica count's refusal does, so a value is refused rather than ignored.
func (v Values) validateBdGatewayCPURequest() error {
	if v.BdGatewayCPURequest == "" || cpuRE.MatchString(v.BdGatewayCPURequest) {
		return nil
	}
	return fmt.Errorf("Values.BdGatewayCPURequest %q: want a positive whole number of cores or millicores, such as 4 or 500m, or empty for no request", v.BdGatewayCPURequest)
}

// bdGatewayResources is the gateway container's share: a CPU request when
// Values.BdGatewayCPURequest is set, and no limit, so the container is Burstable. Empty
// is nil, so the container renders no resources block, as it did before the request.
func bdGatewayResources(v Values) *ContainerResources {
	if v.BdGatewayCPURequest == "" {
		return nil
	}
	return &ContainerResources{Requests: map[string]string{"cpu": v.BdGatewayCPURequest}}
}

// bdGatewayReplicas is the Deployment's replica count: Values.BdGatewayReplicas, with
// zero read as one. validateBdGatewayReplicas has already bounded the value.
func (v Values) bdGatewayReplicas() int32 {
	if v.BdGatewayReplicas == 0 {
		return 1
	}
	return int32(v.BdGatewayReplicas) //nolint:gosec // bounded to 0..2 by validateBdGatewayReplicas, which Build runs first
}

// bdGatewayDeployment is the gateway's pod: an init container that copies the tokens
// to 0600, one container running bdgw serve, the emptyDir for tokens, the log claim
// and the workspace claim. It takes no ServiceAccount token, as the dashboard's pod
// does not.
//
// Its strategy is RollingUpdate with maxUnavailable 0 and maxSurge 1: the new gateway
// starts, and passes its readiness, before the old one stops, so the gateway has no
// gap in service. For the length of a roll two gateway processes share the workspace
// claim and the log claim. Replicas is Values.BdGatewayReplicas, one by default and
// two when an operator sets it; with two, the pods share the claims in steady state
// as they do during a roll, and both match the Service's selector.
//
// That sharing is safe on the control cluster as verified on 2026-11-19 by the
// overseer's commands: kubectl get nodes shows one node (colima-gt-control, k3s v1.35),
// and kubectl get storageclass shows local-path. The gateway's two claims,
// bd-gateway-workspace and bd-gateway-log, are ReadWriteOnce, which is per node, and
// local-path volumes are node-local hostPath, so the surge pod attaches while the old
// pod still holds them and the roll does not stall. This stops
// holding if the control cluster gets a second node: the claims must then become
// ReadWriteMany, or the strategy goes back to Recreate.
//
// The steady-state sharing was checked by gt-yz0s8v (seagull, 2026-11-19): two pods on
// one claim for 10 minutes served 4000 calls, and 636 of 636 acknowledged writes were
// present. One pusher at a time is safe with the workspace push's cross-process lock
// (gt-fo7rf0, landed), which serializes two pushers; the lock holds only between pods
// that run that release, so a roll from an image built before it races once. The check
// did not verify the X-Bd-Workspace hash each pod reports after the other pod swapped
// the tree, nor a real roll with pod startup. The live cluster keeps Recreate until the
// next attended render and apply, so that attended roll is the first real one.
// --log-per-pod gives each pod its own call log directory.
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
			Replicas: v.bdGatewayReplicas(),
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

// bdGatewayMaxConcurrentDefault is bdgw serve's own cap when --max-concurrent is
// absent: internal/bdgateway.DefaultMaxConcurrent. The renderer imports no command
// package (imports_test.go), so the value is repeated here and a test ties it to the
// gateway's constant.
const bdGatewayMaxConcurrentDefault = 16

// bdGatewayMaxConcurrentPerPod is the --max-concurrent one pod runs and whether the
// command carries it. Values.BdGatewayMaxConcurrent is the total across the replicas,
// so each pod takes the total divided by the replica count, rounded up; a zero total
// is the gateway's own default of 16 in total, so two pods take 8 each. The flag is
// left off only when it would repeat that default, one pod with no total set, so an
// unset cap renders what the gateway applied before replicas were configurable. A
// replica count below one reads as one so the division cannot fail; bdGatewayReplicas
// never returns zero.
func bdGatewayMaxConcurrentPerPod(total int, replicas int32) (perPod int, render bool) {
	if replicas < 1 {
		replicas = 1
	}
	if total <= 0 {
		if replicas == 1 {
			return 0, false
		}
		total = bdGatewayMaxConcurrentDefault
	}
	return (total + int(replicas) - 1) / int(replicas), true
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
	if perPod, render := bdGatewayMaxConcurrentPerPod(v.BdGatewayMaxConcurrent, v.bdGatewayReplicas()); render {
		command = append(command, "--max-concurrent", strconv.Itoa(perPod))
	}
	return Container{
		Name:    BdGatewayName,
		Image:   v.BdGatewayImage,
		Command: command,
		// The push listener is the one port the container declares, so the pod's
		// spec names the port bdGatewayPushService forwards to.
		Ports:     []ContainerPort{{Name: bdGatewayPushPortName, ContainerPort: BdGatewayPushPort, Protocol: "TCP"}},
		Resources: bdGatewayResources(v),
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
