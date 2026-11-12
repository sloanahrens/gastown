package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

const buildCheckoutTestSHA = "0123456789abcdef0123456789abcdef01234567"

// buildCheckoutEnsureRecord is what a fake Ensure was called with.
type buildCheckoutEnsureRecord struct {
	calls     int
	townRoot  string
	rig, rev  string
	checkout  string
	returnErr error
}

// buildCheckoutTestDaemon returns a pod's daemon over townRoot whose build
// commit is sha (or shaErr) and whose Ensure records the call and answers
// checkout. Container mode is set on the daemon, as New settles it, so the
// environment the tests run under does not decide.
func buildCheckoutTestDaemon(t *testing.T, townRoot, sha string, shaErr error, rec *buildCheckoutEnsureRecord) *Daemon {
	t.Helper()
	d, _ := newTestDaemon(townRoot)
	d.containerMode = true
	d.fullBuildCommitFn = func() (string, error) { return sha, shaErr }
	d.rebuildGTEnsureFn = func(_ context.Context, townRoot, rig, rev string) (string, error) {
		rec.calls++
		rec.townRoot, rec.rig, rec.rev = townRoot, rig, rev
		return rec.checkout, rec.returnErr
	}
	return d
}

// buildCheckoutTown returns a town root and a checkout directory under
// t.TempDir(), so no test reads the host.
func buildCheckoutTown(t *testing.T) (townRoot, checkout string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

// writeBuildCheckoutSettings writes the town's settings file with raw as its body.
func writeBuildCheckoutSettings(t *testing.T, townRoot, raw string) {
	t.Helper()
	path := agentconfig.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCheckoutStartEnsuresTheOwnFullSHA(t *testing.T) {
	t.Parallel()
	townRoot, checkout := buildCheckoutTown(t)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)

	if err := d.startBuildCheckout(context.Background()); err != nil {
		t.Fatalf("startBuildCheckout: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("Ensure called %d times, want once", rec.calls)
	}
	if rec.rev != buildCheckoutTestSHA {
		t.Fatalf("Ensure rev = %q, want the daemon's own full sha %q", rec.rev, buildCheckoutTestSHA)
	}
	if rec.rig != rebuildGTSourceRig || rec.townRoot != townRoot {
		t.Fatalf("Ensure called for rig %q under %q, want rig %q under %q", rec.rig, rec.townRoot, rebuildGTSourceRig, townRoot)
	}
}

func TestBuildCheckoutStartRefusesABuildCommitNotInFull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		sha    string
		shaErr error
	}{
		{name: "the version package reports no full commit", shaErr: errors.New("the binary records no vcs.revision")},
		{name: "an empty commit", sha: ""},
		{name: "a short commit, as the ldflag stamp is", sha: buildCheckoutTestSHA[:9]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			townRoot, checkout := buildCheckoutTown(t)
			rec := &buildCheckoutEnsureRecord{checkout: checkout}
			d := buildCheckoutTestDaemon(t, townRoot, c.sha, c.shaErr, rec)

			if err := d.startBuildCheckout(context.Background()); err == nil {
				t.Fatal("startBuildCheckout passed, want a refusal")
			}
			if rec.calls != 0 {
				t.Fatalf("Ensure called with rev %q, want no call: a refused start must not reach the checkout", rec.rev)
			}
		})
	}
}

func TestBuildCheckoutStartRefusesAnEnsureFailure(t *testing.T) {
	t.Parallel()
	townRoot, _ := buildCheckoutTown(t)
	rec := &buildCheckoutEnsureRecord{returnErr: errors.New("fetching commit from the bare repo")}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)

	err := d.startBuildCheckout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fetching commit from the bare repo") {
		t.Fatalf("startBuildCheckout = %v, want the Ensure failure", err)
	}
}

func TestBuildCheckoutStartRefusesAMissingPolicyByProfileAndPath(t *testing.T) {
	t.Parallel()
	townRoot, checkout := buildCheckoutTown(t)
	writeBuildCheckoutSettings(t, townRoot, `{"sandboxes":{"polecat":{"gateway":"k8s","image":"img","policy":"deploy/polecat/policy.yaml"}}}`)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)

	err := d.startBuildCheckout(context.Background())
	if err == nil {
		t.Fatal("startBuildCheckout passed with the policy missing, want a refusal")
	}
	for _, want := range []string{"sandbox profile polecat", filepath.Join(checkout, "deploy", "polecat", "policy.yaml")} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

func TestBuildCheckoutStartPassesWithThePolicyPresent(t *testing.T) {
	t.Parallel()
	townRoot, checkout := buildCheckoutTown(t)
	policy := filepath.Join(checkout, "deploy", "polecat", "policy.yaml")
	if err := os.MkdirAll(filepath.Dir(policy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte("policy"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuildCheckoutSettings(t, townRoot, `{"sandboxes":{"polecat":{"gateway":"k8s","image":"img","policy":"deploy/polecat/policy.yaml"}}}`)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)

	if err := d.startBuildCheckout(context.Background()); err != nil {
		t.Fatalf("startBuildCheckout: %v", err)
	}
}

func TestBuildCheckoutStartSkipsThePolicyPartForATownWithNoProfiles(t *testing.T) {
	t.Parallel()
	townRoot, checkout := buildCheckoutTown(t)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)

	if err := d.startBuildCheckout(context.Background()); err != nil {
		t.Fatalf("startBuildCheckout with no settings file: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("Ensure called %d times, want once: the checkout is ensured even with no profiles", rec.calls)
	}
}

func TestBuildCheckoutStartRefusesUnreadableSettings(t *testing.T) {
	t.Parallel()
	townRoot, checkout := buildCheckoutTown(t)
	writeBuildCheckoutSettings(t, townRoot, `{"sandboxes":`)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)

	if err := d.startBuildCheckout(context.Background()); err == nil {
		t.Fatal("startBuildCheckout passed with unreadable settings, want a refusal: the policies cannot be checked")
	}
}

func TestBuildCheckoutStartLeavesAHostDaemonsCheckoutAlone(t *testing.T) {
	t.Parallel()
	townRoot, checkout := buildCheckoutTown(t)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	d := buildCheckoutTestDaemon(t, townRoot, buildCheckoutTestSHA, nil, rec)
	d.containerMode = false

	if err := d.startBuildCheckout(context.Background()); err != nil {
		t.Fatalf("startBuildCheckout on a host daemon: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("Ensure called %d times on a host daemon, want none", rec.calls)
	}
}
