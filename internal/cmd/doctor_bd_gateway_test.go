package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

// bdGatewayKubeAnswer is what the fake kubectl answers for one object: its output,
// exit status and error.
type bdGatewayKubeAnswer struct {
	out  string
	exit int
	err  error
}

const (
	bdGatewayAvailableDeployment = `{"status":{"conditions":[{"type":"Available","status":"True","reason":"MinimumReplicasAvailable"}]}}`
	bdGatewayUnavailableDeploy   = `{"status":{"conditions":[{"type":"Available","status":"False","reason":"MinimumReplicasUnavailable"}]}}`
	bdGatewayServedEndpoints     = `{"subsets":[{"addresses":[{"ip":"10.0.0.5"}]}]}`
	bdGatewayNoEndpoints         = `{"subsets":[]}`
)

// bdGatewayFake is the cluster and the town a gateway check reads. Each read is a
// field, so one test sets the answer it needs and leaves the others healthy.
type bdGatewayFake struct {
	deployment, endpoints bdGatewayKubeAnswer
	townHash              string
	townErr               error
	reported              string
	reportedErr           error

	kubeCalls     []string
	townCalls     int
	reportedCalls int
}

// healthyBdGatewayFake is a gateway that passes all three checks: the Deployment is
// Available, the Service has an address, and the reported hash is the town's.
func healthyBdGatewayFake() *bdGatewayFake {
	return &bdGatewayFake{
		deployment: bdGatewayKubeAnswer{out: bdGatewayAvailableDeployment},
		endpoints:  bdGatewayKubeAnswer{out: bdGatewayServedEndpoints},
		townHash:   "hash-of-town",
		reported:   "hash-of-town",
	}
}

// deps is the fake as the check's seams. The kubectl fake answers the two reads by
// kind and records each one.
func (f *bdGatewayFake) deps() bdGatewayClusterDeps {
	return bdGatewayClusterDeps{
		kube: func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
			if name != "kubectl" || len(args) < 4 {
				return "", 1, errors.New("unexpected command")
			}
			kind := args[3]
			f.kubeCalls = append(f.kubeCalls, kind)
			answer := f.deployment
			if kind == "endpoints" {
				answer = f.endpoints
			}
			return answer.out, answer.exit, answer.err
		},
		namespace: "gt",
		townHash: func(string) (string, error) {
			f.townCalls++
			return f.townHash, f.townErr
		},
		reportedHash: func(context.Context) (string, error) {
			f.reportedCalls++
			return f.reported, f.reportedErr
		},
	}
}

// absentBdGatewayCluster is a cluster with no bd-gateway Deployment. It is what the
// existing data-unit registration test runs against: the check falls back to the
// host-container check, which never reaches the town or the gateway's hash.
func absentBdGatewayCluster() bdGatewayClusterDeps {
	return bdGatewayClusterDeps{
		kube: func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 0, nil
		},
		namespace: "gt",
		townHash: func(string) (string, error) {
			return "", errors.New("the town hash should not be read when no Deployment exists")
		},
		reportedHash: func(context.Context) (string, error) {
			return "", errors.New("the reported hash should not be read when no Deployment exists")
		},
	}
}

// runBdGatewayCheck registers the data unit check with names and cluster, runs it
// against a town whose compose config is in place, and returns the result and the
// docker calls it made. Docker answers nothing, so the host-container check fails
// when it runs, which is how a test sees that it ran.
func runBdGatewayCheck(t *testing.T, names []string, cluster bdGatewayClusterDeps) (*doctor.CheckResult, []composeCall) {
	t.Helper()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	d := doctor.NewDoctor()
	registerDataUnitCheck(d, names, fakeComposeDeps(townRoot, &calls, nil, nil), cluster)
	res := d.Checks()[0].Run(&doctor.CheckContext{TownRoot: townRoot})
	return res, calls
}

// TestDoctorBdGatewayHealthyPassesAllThree: a Deployment that is Available, a
// Service with an endpoint, and a reported hash equal to the town copy's pass all
// three checks.
func TestDoctorBdGatewayHealthyPassesAllThree(t *testing.T) {
	t.Parallel()
	f := healthyBdGatewayFake()
	res, docker := runBdGatewayCheck(t, []string{doctor.DataUnitCheckName}, f.deps())
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; message %q", res.Status, res.Message)
	}
	if len(res.Details) != 3 {
		t.Errorf("passed rungs = %d, want all three named in details: %v", len(res.Details), res.Details)
	}
	if len(docker) != 0 {
		t.Errorf("docker calls = %d, want none: the in-cluster gateway replaces the compose unit", len(docker))
	}
}

// TestDoctorBdGatewayFailsWithItsOwnMessage: an unavailable Deployment, a Service
// with no endpoint and a hash mismatch each fail with a message of their own, at
// the rung they belong to. Each row's Details count shows the rungs that passed
// before it.
func TestDoctorBdGatewayFailsWithItsOwnMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mutate     func(f *bdGatewayFake)
		wantText   string
		wantPassed int
	}{
		{
			name: "unavailable Deployment",
			mutate: func(f *bdGatewayFake) {
				f.deployment.out = bdGatewayUnavailableDeploy
			},
			wantText:   "the bd-gateway Deployment is not Available",
			wantPassed: 0,
		},
		{
			name: "Service with no endpoint",
			mutate: func(f *bdGatewayFake) {
				f.endpoints.out = bdGatewayNoEndpoints
			},
			wantText:   "the bd-gateway Service has no endpoint",
			wantPassed: 1,
		},
		{
			name: "hash mismatch",
			mutate: func(f *bdGatewayFake) {
				f.reported = "hash-of-something-else"
			},
			wantText:   "serves a workspace that differs from the town",
			wantPassed: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyBdGatewayFake()
			tt.mutate(f)
			res, _ := runBdGatewayCheck(t, []string{doctor.DataUnitCheckName}, f.deps())
			if res.Status != doctor.StatusError {
				t.Fatalf("status = %v, want error; message %q", res.Status, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantText) {
				t.Errorf("message = %q, want it to name %q", res.Message, tt.wantText)
			}
			if len(res.Details) != tt.wantPassed {
				t.Errorf("passed rungs = %d, want %d: %v", len(res.Details), tt.wantPassed, res.Details)
			}
		})
	}
}

// TestDoctorBdGatewayNoDeploymentFallsBack: with no bd-gateway Deployment the
// host-container check runs unchanged: it reads the compose unit through docker, and
// the gateway's hashes are never read.
func TestDoctorBdGatewayNoDeploymentFallsBack(t *testing.T) {
	t.Parallel()
	f := healthyBdGatewayFake()
	f.deployment = bdGatewayKubeAnswer{}
	res, docker := runBdGatewayCheck(t, []string{doctor.DataUnitCheckName}, f.deps())
	if len(docker) != 1 {
		t.Errorf("docker calls = %d, want 1: the host-container check runs when no Deployment exists", len(docker))
	}
	if !strings.HasPrefix(res.Message, "data unit rung ") {
		t.Errorf("message = %q, want the host-container check's own rung message", res.Message)
	}
	if f.townCalls != 0 || f.reportedCalls != 0 {
		t.Errorf("town hash reads = %d, reported hash reads = %d, want none without a Deployment", f.townCalls, f.reportedCalls)
	}
	for _, kind := range f.kubeCalls {
		if kind == "endpoints" {
			t.Errorf("endpoints were read with no Deployment: %v", f.kubeCalls)
		}
	}
}

// TestDoctorBdGatewayUnreadableIsNotAbsent: a read that fails is an error that names
// what it could not read. It never falls back to the host check, since an unreadable
// Deployment is not an absent one.
func TestDoctorBdGatewayUnreadableIsNotAbsent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(f *bdGatewayFake)
	}{
		{
			name:   "kubectl exits nonzero on the Deployment read",
			mutate: func(f *bdGatewayFake) { f.deployment = bdGatewayKubeAnswer{exit: 1} },
		},
		{
			name:   "kubectl cannot run",
			mutate: func(f *bdGatewayFake) { f.deployment = bdGatewayKubeAnswer{err: errors.New("no kubectl")} },
		},
		{
			name:   "the Deployment answer is not JSON",
			mutate: func(f *bdGatewayFake) { f.deployment = bdGatewayKubeAnswer{out: "not json"} },
		},
		{
			name:   "kubectl exits nonzero on the Endpoints read",
			mutate: func(f *bdGatewayFake) { f.endpoints = bdGatewayKubeAnswer{exit: 1} },
		},
		{
			name:   "the Endpoints answer is not JSON",
			mutate: func(f *bdGatewayFake) { f.endpoints = bdGatewayKubeAnswer{out: "not json"} },
		},
		{
			name:   "the town copy cannot be hashed",
			mutate: func(f *bdGatewayFake) { f.townErr = errors.New("no town") },
		},
		{
			name:   "the gateway's reported hash cannot be read",
			mutate: func(f *bdGatewayFake) { f.reportedErr = errors.New("no hash") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyBdGatewayFake()
			tt.mutate(f)
			res, docker := runBdGatewayCheck(t, []string{doctor.DataUnitCheckName}, f.deps())
			if res.Status != doctor.StatusError {
				t.Fatalf("status = %v, want error; message %q", res.Status, res.Message)
			}
			if !strings.HasPrefix(res.Message, "the in-cluster bd gateway could not be read") {
				t.Errorf("message = %q, want it to name the unreadable gateway", res.Message)
			}
			if len(docker) != 0 {
				t.Errorf("docker calls = %d, want none: an unreadable cluster is not an absent Deployment", len(docker))
			}
		})
	}
}

// TestDoctorBdGatewayUnwiredSeamRefuses: a check built with no kubectl or hash seam
// refuses by name, rather than passing an unread gateway.
func TestDoctorBdGatewayUnwiredSeamRefuses(t *testing.T) {
	t.Parallel()
	f := healthyBdGatewayFake()
	cluster := f.deps()
	cluster.kube = nil
	res, _ := runBdGatewayCheck(t, []string{doctor.DataUnitCheckName}, cluster)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message %q", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "kubectl is not wired") {
		t.Errorf("message = %q, want it to name the unwired kubectl", res.Message)
	}
}

// TestDoctorBdGatewayNotNamedReadsNothing: an unnamed data unit check is skipped and
// reads neither the cluster nor the compose unit.
func TestDoctorBdGatewayNotNamedReadsNothing(t *testing.T) {
	t.Parallel()
	f := healthyBdGatewayFake()
	res, docker := runBdGatewayCheck(t, nil, f.deps())
	if res.Status != doctor.StatusSkipped {
		t.Fatalf("status = %v, want skipped; message %q", res.Status, res.Message)
	}
	if len(f.kubeCalls) != 0 || len(docker) != 0 {
		t.Errorf("kubectl reads = %v, docker calls = %d, want none when unnamed", f.kubeCalls, len(docker))
	}
}
