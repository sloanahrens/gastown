package daemon

import "strconv"

// The bd gateway's pod: one replica of bdgw serve, reading its tokens from the
// token Secret the operator creates and keeping its workspace on a claim. It runs
// as user and group 65532 and may not gain privilege.
//
// fsGroup is 65532 as well, and the token volume is group-readable (0440): the
// Secret is root-owned, so the non-root gateway reads its tokens through the group,
// not the owner. A 0400 volume would leave the gateway unable to read any token.
const (
	// bdGatewayIdentity is the uid and gid the gateway runs as.
	bdGatewayIdentity = int64(65532)
	// bdGatewayTokensDir is where the token Secret is mounted, read-only.
	bdGatewayTokensDir = "/run/bdgw/tokens" //nolint:gosec // a directory path, not a credential
	// bdGatewayLogDir is an emptyDir for the gateway's logs.
	bdGatewayLogDir = "/var/log/bdgw"
	// bdGatewayWorkspaceMount is the claim's mount; the gateway's workspace is the
	// ws directory under it.
	bdGatewayWorkspaceMount = "/var/lib/bdgw"
	bdGatewayWorkspaceDir   = bdGatewayWorkspaceMount + "/ws"
	// bdGatewayVerbs is the verb table the command reads.
	bdGatewayVerbs = "/etc/bdgw/verbs.json"
	// bdGatewayTokensVolume and bdGatewayLogVolume name the two volumes the
	// container mounts; the claim is named by BdGatewayWorkspaceClaim.
	bdGatewayTokensVolume = "bd-gateway-tokens" //nolint:gosec // a volume name, not a credential
	bdGatewayLogVolume    = "bd-gateway-log"
	// bdGatewayTokensMode is owner and group read, so fsGroup lets the gateway read.
	bdGatewayTokensMode = int32(0o440)
	// bdGatewayDoltPort is the Dolt port the gateway dials at DoltHostName.
	bdGatewayDoltPort = 3307
)

// bdGatewayDeployment is the gateway's pod: one container running bdgw serve, the
// token Secret read-only, the emptyDir for logs, and the workspace claim. It takes
// no ServiceAccount token, as the dashboard's pod does not.
//
// Its strategy is Recreate: the old gateway stops before the new one starts, so the
// two never write the workspace together.
func bdGatewayDeployment(v Values, namespace string) Deployment {
	automount := false
	user := bdGatewayIdentity
	group := bdGatewayIdentity
	fsGroup := bdGatewayIdentity
	return Deployment{
		TypeMeta: TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		Metadata: ObjectMeta{Name: BdGatewayName, Namespace: namespace, Labels: bdGatewayLabels()},
		Spec: DeploymentSpec{
			Replicas: 1,
			Strategy: DeploymentStrategy{Type: "Recreate"},
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
					HostAliases: hostAliases(v),
					Containers:  []Container{bdGatewayContainer(v)},
					Volumes:     bdGatewayVolumes(v),
				},
			},
		},
	}
}

// bdGatewayContainer runs bdgw serve with the command the gateway's image expects.
// Its Dolt endpoint is DoltHostName, which the pod's hostAliases entry resolves when
// the render has a host gateway address. Its readiness is bdgw ready on the
// workspace, so a gateway whose workspace is not ready stays out of the Service.
func bdGatewayContainer(v Values) Container {
	allowEscalation := false
	return Container{
		Name:    BdGatewayName,
		Image:   v.BdGatewayImage,
		Command: []string{"bdgw", "serve", "--tokens-dir", bdGatewayTokensDir, "--verbs", bdGatewayVerbs, "--log-dir", bdGatewayLogDir, "--workspace", bdGatewayWorkspaceDir, "--dolt-host", DoltHostName, "--dolt-port", strconv.Itoa(bdGatewayDoltPort)},
		SecurityContext: &ContainerSecurityContext{
			AllowPrivilegeEscalation: &allowEscalation,
		},
		VolumeMounts: []VolumeMount{
			{Name: bdGatewayTokensVolume, MountPath: bdGatewayTokensDir, ReadOnly: true},
			{Name: bdGatewayLogVolume, MountPath: bdGatewayLogDir},
			{Name: BdGatewayWorkspaceClaim, MountPath: bdGatewayWorkspaceMount},
		},
		ReadinessProbe: &Probe{
			Exec:                &ExecAction{Command: []string{"bdgw", "ready", "--workspace", bdGatewayWorkspaceDir}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       15,
			TimeoutSeconds:      10,
			FailureThreshold:    3,
		},
	}
}

// bdGatewayVolumes are the gateway's volumes: the token Secret at the group-readable
// mode, the log emptyDir, and the workspace claim. The token Secret is mounted
// whole, since the gateway reads a directory of tokens.
func bdGatewayVolumes(v Values) []Volume {
	mode := bdGatewayTokensMode
	return []Volume{
		{Name: bdGatewayTokensVolume, Secret: &SecretVolumeSource{SecretName: v.BdGatewayTokenSecret, DefaultMode: &mode}},
		{Name: bdGatewayLogVolume, EmptyDir: &EmptyDirVolumeSource{}},
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
