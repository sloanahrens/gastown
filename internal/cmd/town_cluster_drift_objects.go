package cmd

import (
	"strings"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// namespacedDriftTarget is the drift target of one namespaced object the render
// builds, named by its kind and name: the resource kubectl reads it by, its
// namespace, and the object the render produced. The read and the compare are
// driftTarget's, so the kind's fields are walked the way a Deployment's are, and
// a field the render never names, such as a Service's clusterIP or a claim's
// volumeName, is not drift (gt-03u02a).
func namespacedDriftTarget(kind, name, namespace string, object any) driftTarget {
	return driftTarget{kind: kind, resource: strings.ToLower(kind) + "/" + name, namespace: namespace, object: object}
}

// renderedNamespacedTargets is every namespaced object a render builds beside
// its Deployments: the Services, the claims, the ServiceAccount and the tracker
// backup CronJob. Build leaves an object zero unless the flags render it, so a
// cluster with no tracker, no backup and no bd gateway is never asked for their
// objects, and a host path that replaces the dolt-backup claim renders no claim
// to compare (gt-03u02a).
func renderedNamespacedTargets(m k8sdaemon.Manifests) []driftTarget {
	var targets []driftTarget
	services := []k8sdaemon.Service{
		m.ForgejoService,
		m.DashboardService,
		m.TrackerService,
		m.BdGatewayService,
		m.BdGatewayPushService,
	}
	for _, service := range services {
		if service.Kind != "" {
			targets = append(targets, namespacedDriftTarget("Service", service.Metadata.Name, service.Metadata.Namespace, service))
		}
	}
	claims := []k8sdaemon.PersistentVolumeClaim{
		m.TownStateClaim,
		m.LandingWorkClaim,
		m.TrackerBackupClaim,
	}
	if m.DoltBackupClaim != nil {
		claims = append(claims, *m.DoltBackupClaim)
	}
	claims = append(claims, m.BdGatewayClaim, m.BdGatewayLogClaim)
	for _, claim := range claims {
		if claim.Kind != "" {
			targets = append(targets, namespacedDriftTarget("PersistentVolumeClaim", claim.Metadata.Name, claim.Metadata.Namespace, claim))
		}
	}
	if m.ServiceAccount.Kind != "" {
		targets = append(targets, namespacedDriftTarget("ServiceAccount", m.ServiceAccount.Metadata.Name, m.ServiceAccount.Metadata.Namespace, m.ServiceAccount))
	}
	if m.TrackerBackupCronJob.Kind != "" {
		targets = append(targets, namespacedDriftTarget("CronJob", m.TrackerBackupCronJob.Metadata.Name, m.TrackerBackupCronJob.Metadata.Namespace, m.TrackerBackupCronJob))
	}
	return targets
}
