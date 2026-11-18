package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/update"
)

// bdGatewayReadTimeout bounds the whole in-cluster read of one data-unit run, so
// a kubectl that never answers ends the check rather than the doctor run.
const bdGatewayReadTimeout = 30 * time.Second

// bdGatewayClusterDeps is what the data unit check reads outside the town when a
// bd-gateway Deployment is in the cluster: the kubectl runner for the Deployment
// and Endpoints reads, the town copy's workspace hash, and the hash the running
// gateway reports. A test supplies fakes, so it reads no cluster and no town.
type bdGatewayClusterDeps struct {
	// kube runs kubectl, the same seam gt town cluster drift reads through.
	kube update.RunFunc
	// namespace is the namespace the gateway's Deployment and Service live in.
	namespace string
	// townHash is the manifest hash of the town copy's workspace.
	townHash func(townRoot string) (string, error)
	// reportedHash is the manifest hash the running gateway reports.
	reportedHash func(ctx context.Context) (string, error)
}

// realBdGatewayClusterDeps is the host's: kubectl, the daemon's namespace, the
// town copy's hash from the same document a push sends, and the reported hash read
// through gateway, the pusher the daemon sends its workspace with.
func realBdGatewayClusterDeps(gateway bdgateway.GatewayPusher) bdGatewayClusterDeps {
	return bdGatewayClusterDeps{
		kube:         realRunFunc(),
		namespace:    k8sdaemon.DefaultNamespace,
		townHash:     townWorkspaceHash,
		reportedHash: gateway.ReportedHash,
	}
}

// bdGatewayPusherFromEnv is the pusher the doctor reads the gateway with: the URL and
// token file gt-wrap's bdgateway target reads, the same pair the daemon pushes with.
// getenv is os.Getenv in the doctor and a fake in a test.
func bdGatewayPusherFromEnv(getenv func(string) string) bdgateway.GatewayPusher {
	return bdgateway.GatewayPusher{
		URL:       getenv(bdgateway.WorkspaceURLEnv),
		TokenFile: getenv(bdgateway.WorkspaceTokenEnv),
	}
}

// townWorkspaceHash is the manifest hash of the town copy, the hash a push of
// the town's workspace would carry.
func townWorkspaceHash(townRoot string) (string, error) {
	doc, err := bdgateway.BuildWorkspaceDocument(townRoot)
	if err != nil {
		return "", err
	}
	return doc.ManifestSHA, nil
}

// bdGatewayDataUnitCheck is the data unit check with the in-cluster bd gateway in
// front of it. With no bd-gateway Deployment in the cluster it is the host-container
// check, run unchanged. With one, it checks the gateway in the cluster. Its name,
// its opt-in and its SKIPPED line are the data unit check's own.
type bdGatewayDataUnitCheck struct {
	doctor.Check

	// named is whether --check named the data unit check. An unnamed run reads
	// nothing from the cluster.
	named bool

	cluster bdGatewayClusterDeps
}

// bdGatewayDeployment is the part of the Deployment the check reads: its
// Available condition.
type bdGatewayDeployment struct {
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

// bdGatewayEndpoints is the part of the Endpoints object the check reads: the
// addresses that back the Service.
type bdGatewayEndpoints struct {
	Subsets []struct {
		Addresses []json.RawMessage `json:"addresses"`
	} `json:"subsets"`
}

// Run checks the in-cluster gateway when the cluster has its Deployment, and runs
// the host-container check when it does not. An unreadable cluster is an error,
// never the host check: a Deployment kubectl could not read is not an absent one.
func (c bdGatewayDataUnitCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	if !c.named {
		return c.Check.Run(ctx)
	}
	res := &doctor.CheckResult{Name: c.Name(), Category: doctor.CategoryInfrastructure}
	kctx, cancel := context.WithTimeout(context.Background(), bdGatewayReadTimeout)
	defer cancel()

	deployment, found, err := c.readDeployment(kctx)
	if err != nil {
		return bdGatewayUnreadable(res, err)
	}
	if !found {
		return c.Check.Run(ctx)
	}

	var passed []string
	fail := func(msg string) *doctor.CheckResult {
		res.Status = doctor.StatusError
		res.Message = msg
		res.Details = passed
		return res
	}

	if reason, ok := bdGatewayUnavailable(deployment); ok {
		return fail("the bd-gateway Deployment is not Available: " + reason)
	}
	passed = append(passed, "Deployment bd-gateway is Available")

	ready, err := c.readyEndpoints(kctx)
	if err != nil {
		return bdGatewayUnreadable(res, err)
	}
	if ready == 0 {
		return fail("the bd-gateway Service has no endpoint, so no gateway pod answers it")
	}
	passed = append(passed, fmt.Sprintf("Service bd-gateway has %d endpoint(s)", ready))

	want, err := c.townHashFor(ctx.TownRoot)
	if err != nil {
		return bdGatewayUnreadable(res, err)
	}
	got, err := c.reportedHashFor(kctx)
	if err != nil {
		return bdGatewayUnreadable(res, err)
	}
	if got != want {
		return fail(fmt.Sprintf("the gateway reports workspace hash %s, but the town copy hashes to %s: the gateway serves a workspace that differs from the town", got, want))
	}
	passed = append(passed, "the gateway's workspace hash matches the town copy")

	res.Status = doctor.StatusOK
	res.Message = "the in-cluster bd gateway is healthy: its Deployment is Available, its Service has an endpoint, and its workspace hash matches the town copy"
	res.Details = passed
	return res
}

// bdGatewayUnreadable is the result of a run that could not read the gateway. It is
// an error, so an unread input never passes as a clean check.
func bdGatewayUnreadable(res *doctor.CheckResult, err error) *doctor.CheckResult {
	res.Status = doctor.StatusError
	res.Message = "the in-cluster bd gateway could not be read: " + err.Error()
	return res
}

// readDeployment reads the bd-gateway Deployment. found is false when the cluster has
// none.
func (c bdGatewayDataUnitCheck) readDeployment(ctx context.Context) (bdGatewayDeployment, bool, error) {
	var dep bdGatewayDeployment
	out, found, err := c.cluster.get(ctx, "deployment", k8sdaemon.BdGatewayName)
	if err != nil || !found {
		return dep, false, err
	}
	if err := json.Unmarshal(out, &dep); err != nil {
		return dep, false, errors.New("the Deployment bd-gateway answer is not a JSON object")
	}
	return dep, true, nil
}

// readyEndpoints counts the addresses behind the bd-gateway Service. A Service with no
// Endpoints object has none.
func (c bdGatewayDataUnitCheck) readyEndpoints(ctx context.Context) (int, error) {
	out, found, err := c.cluster.get(ctx, "endpoints", k8sdaemon.BdGatewayName)
	if err != nil || !found {
		return 0, err
	}
	var ep bdGatewayEndpoints
	if err := json.Unmarshal(out, &ep); err != nil {
		return 0, errors.New("the Endpoints bd-gateway answer is not a JSON object")
	}
	n := 0
	for _, s := range ep.Subsets {
		n += len(s.Addresses)
	}
	return n, nil
}

// townHashFor is the town copy's hash, or an error when the seam is not wired.
func (c bdGatewayDataUnitCheck) townHashFor(townRoot string) (string, error) {
	if c.cluster.townHash == nil {
		return "", errors.New("the town copy's workspace hash is not wired")
	}
	return c.cluster.townHash(townRoot)
}

// reportedHashFor is the gateway's reported hash, or an error when the seam is not
// wired.
func (c bdGatewayDataUnitCheck) reportedHashFor(ctx context.Context) (string, error) {
	if c.cluster.reportedHash == nil {
		return "", errors.New("the gateway's reported workspace hash is not wired")
	}
	return c.cluster.reportedHash(ctx)
}

// get reads one object by kind and name through kubectl. --ignore-not-found makes an
// object that is not there an empty answer with exit 0, so absence is never read
// out of an error string. A nonzero exit is an error that names the object and not
// the output, which can carry a secret.
func (c bdGatewayClusterDeps) get(ctx context.Context, kind, name string) ([]byte, bool, error) {
	if c.kube == nil {
		return nil, false, errors.New("kubectl is not wired")
	}
	out, exit, err := c.kube(ctx, "", nil, "kubectl", "-n", c.namespace, "get", kind, name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, false, fmt.Errorf("reading %s/%s: %w", kind, name, err)
	}
	if exit != 0 {
		return nil, false, fmt.Errorf("reading %s/%s: kubectl exited %d", kind, name, exit)
	}
	if strings.TrimSpace(out) == "" {
		return nil, false, nil
	}
	return []byte(out), true, nil
}

// bdGatewayUnavailable reports why the Deployment is not Available, and whether it is
// not. A Deployment with no Available condition yet is not Available.
func bdGatewayUnavailable(dep bdGatewayDeployment) (string, bool) {
	for _, cond := range dep.Status.Conditions {
		if cond.Type != "Available" {
			continue
		}
		if cond.Status == "True" {
			return "", false
		}
		return fmt.Sprintf("its Available condition is %s (%s)", cond.Status, cond.Reason), true
	}
	return "it has no Available condition yet", true
}
