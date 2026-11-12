//go:build integration

package gtwrap

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegrationKubernetesTargetReachesTheDaemonPod drives the kubernetes target
// through the real kubectl against the control cluster. It reads a live cluster,
// so it runs only when GT_TEST_K8S=1 and skips otherwise.
func TestIntegrationKubernetesTargetReachesTheDaemonPod(t *testing.T) {
	if os.Getenv("GT_TEST_K8S") != "1" {
		t.Skip("GT_TEST_K8S is not 1, so the control cluster is not reached")
	}
	var note bytes.Buffer
	k := Kubectl{
		Town:       kubeTown,
		Context:    kubeContext,
		Namespace:  kubeNamespace,
		Deployment: kubeDeploy,
		Container:  kubeContainer,
		Tool:       "gt",
		Shell:      OSShell{},
		LookPath:   exec.LookPath,
		Note:       &note,
	}
	diag, err := k.Doctor()
	if err != nil {
		t.Fatalf("Doctor = %v", err)
	}
	if diag.Rung != RungExec {
		t.Fatalf("the doctor reports rung %s, want exec: %s", diag.Rung, diag.Reason)
	}
	// A directory the pod lacks runs in the nearest existing ancestor, the town
	// root, and the tool is told the caller's Mac directory there.
	missing := filepath.Join(kubeTown, "gt-wrap-integration-missing")
	exit, err := k.Launch(Call{Argv: []string{"sh", "-c", `test "$GT_CALLER_CWD" = "$1"`, "sh", missing}, Dir: missing})
	if err != nil || exit != 0 {
		t.Fatalf("a call from a missing directory = %d, %v; want 0, nil with GT_CALLER_CWD naming it", exit, err)
	}
	exit, err = k.Launch(Call{Argv: []string{"gt", "status", "--line"}, Dir: kubeTown})
	if err != nil || exit != 0 {
		t.Fatalf("gt status --line through the kubernetes target = %d, %v; want 0, nil", exit, err)
	}
}
