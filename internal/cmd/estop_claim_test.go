package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/holdclaim"
	"github.com/steveyegge/gastown/internal/holdclaim/holdclaimtest"
)

// errPodDown is the failure a fake pod returns for a call it refuses.
var errPodDown = errors.New("pod unreachable")

// podClaimFor returns the claim writer for town over a fresh fake pod.
func podClaimFor(t *testing.T, town string) (*holdclaim.Writer, *holdclaimtest.Pod) {
	t.Helper()
	pod := holdclaimtest.New()
	w, err := holdclaim.New(town, holdclaim.Target{Context: "kind-gt", Namespace: "gt"}, pod.Exec)
	if err != nil {
		t.Fatalf("holdclaim.New: %v", err)
	}
	return w, pod
}

func TestActivateEstopMirrorsTheTownSentinelIntoThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pod, fake := podClaimFor(t, town)
	var out bytes.Buffer

	if err := activateEstopWith(&out, town, "", "closing laptop", pod); err != nil {
		t.Fatalf("activateEstopWith: %v", err)
	}
	local, err := os.ReadFile(estop.FilePath(town))
	if err != nil {
		t.Fatalf("the Mac sentinel was not written: %v", err)
	}
	if got := fake.Files[k8sdaemon.TownRootPath+"/"+estop.FileName]; string(got) != string(local) {
		t.Fatalf("ESTOP in the pod = %q, want the Mac's %q", got, local)
	}
	podFile := k8sdaemon.TownRootPath + "/" + estop.FileName
	want := [][]string{
		{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", "tee", podFile},
		{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", "cat", podFile},
	}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.Calls, want)
	}
}

func TestLiftEstopWithNoPodWriterRemovesOnlyTheMacSentinel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}

	if err := liftEstop(town, "", nil); err != nil {
		t.Fatalf("liftEstop: %v", err)
	}
	if estop.IsActive(town) {
		t.Fatal("the Mac ESTOP survived the Mac-only lift")
	}
}

func TestActivateEstopMirrorsTheRigSentinelIntoThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pod, fake := podClaimFor(t, town)
	var out bytes.Buffer

	if err := activateEstopWith(&out, town, "gastown", "maintenance", pod); err != nil {
		t.Fatalf("activateEstopWith: %v", err)
	}
	rigFile := estop.RigFileName("gastown")
	podFile := k8sdaemon.TownRootPath + "/" + rigFile
	if _, ok := fake.Files[podFile]; !ok {
		t.Fatalf("rig ESTOP %s is not in the pod's claim; files = %v", rigFile, keysOf(fake.Files))
	}
	want := [][]string{
		{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", "tee", podFile},
		{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", "cat", podFile},
	}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.Calls, want)
	}
}

func TestActivateEstopRefusesWhenThePodWriteFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pod, fake := podClaimFor(t, town)
	fake.Fail["tee"] = errPodDown
	var out bytes.Buffer

	err := activateEstopWith(&out, town, "", "", pod)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("activateEstopWith error = %v, want the NOT-in-the-pod refusal", err)
	}
	if _, statErr := os.Stat(estop.FilePath(town)); statErr != nil {
		t.Fatalf("the Mac sentinel must stay after a failed pod write: %v", statErr)
	}
}

func TestActivateEstopWithNoPodWriterIsMacOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer

	if err := activateEstopWith(&out, town, "", "", nil); err != nil {
		t.Fatalf("activateEstopWith: %v", err)
	}
	if _, err := os.Stat(estop.FilePath(town)); err != nil {
		t.Fatalf("the Mac sentinel was not written: %v", err)
	}
}

func TestActivateEstopAlreadyActiveStillMirrorsIntoThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "earlier"); err != nil {
		t.Fatal(err)
	}
	pod, fake := podClaimFor(t, town)
	var out bytes.Buffer

	if err := activateEstopWith(&out, town, "", "", pod); err != nil {
		t.Fatalf("activateEstopWith: %v", err)
	}
	if !strings.Contains(out.String(), "already active") {
		t.Fatalf("output = %q, want the already-active report", out.String())
	}
	if _, ok := fake.Files[k8sdaemon.TownRootPath+"/"+estop.FileName]; !ok {
		t.Fatal("an ESTOP already up on the Mac was not mirrored into the pod")
	}
}

func TestActivateEstopRefusesAHalfGivenPodSettingBeforeWriting(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := holdclaim.NamespaceEnv + "=gt\n"
	if err := os.WriteFile(filepath.Join(town, "settings", "daemon.env"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	err := activateEstop(&out, town, "", "")
	if err == nil || !strings.Contains(err.Error(), "pod setting") {
		t.Fatalf("activateEstop error = %v, want a refusal naming the pod setting", err)
	}
	if _, statErr := os.Stat(estop.FilePath(town)); !os.IsNotExist(statErr) {
		t.Fatalf("the ESTOP was written before the pod setting was read: stat err = %v", statErr)
	}
}

func TestLiftEstopRemovesThePodCopyThenTheMacSentinel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	pod, fake := podClaimFor(t, town)
	fake.Files[k8sdaemon.TownRootPath+"/"+estop.FileName] = []byte("copy")

	if err := liftEstop(town, "", pod); err != nil {
		t.Fatalf("liftEstop: %v", err)
	}
	if _, ok := fake.Files[k8sdaemon.TownRootPath+"/"+estop.FileName]; ok {
		t.Fatal("the pod kept its ESTOP copy after the lift")
	}
	if estop.IsActive(town) {
		t.Fatal("the Mac ESTOP survived the lift")
	}
}

func TestLiftEstopKeepsTheMacSentinelWhenThePodRemoveFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	pod, fake := podClaimFor(t, town)
	fake.Fail["rm"] = errPodDown

	err := liftEstop(town, "", pod)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("liftEstop error = %v, want the NOT-in-the-pod refusal", err)
	}
	if !estop.IsActive(town) {
		t.Fatal("the Mac ESTOP was removed although the pod kept its copy")
	}
}

func TestLiftEstopRigRemovesOnlyThatRigsSentinel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.ActivateRig(town, "gastown", estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	pod, fake := podClaimFor(t, town)
	fake.Files[k8sdaemon.TownRootPath+"/"+estop.RigFileName("gastown")] = []byte("copy")

	if err := liftEstop(town, "gastown", pod); err != nil {
		t.Fatalf("liftEstop rig: %v", err)
	}
	if estop.ReadRig(town, "gastown") != nil {
		t.Fatal("the Mac rig ESTOP survived the lift")
	}
	if _, ok := fake.Files[k8sdaemon.TownRootPath+"/"+estop.RigFileName("gastown")]; ok {
		t.Fatal("the pod kept its rig ESTOP copy after the lift")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestActivateEstopRigRefusesWhenThePodWriteFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pod, fake := podClaimFor(t, town)
	fake.Fail["tee"] = errPodDown
	var out bytes.Buffer

	err := activateEstopWith(&out, town, "gastown", "", pod)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("activateEstopWith rig error = %v, want the NOT-in-the-pod refusal", err)
	}
	if estop.ReadRig(town, "gastown") == nil {
		t.Fatal("the Mac rig ESTOP must stay after a failed pod write")
	}
}

func TestThawEstopRefusesAHalfGivenPodSettingBeforeLifting(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "daemon.env"), []byte(holdclaim.ContextEnv+"=kind-gt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := holdclaimtest.New()

	err := thawEstop(town, "", fake.Exec)
	if err == nil || !strings.Contains(err.Error(), "pod setting") {
		t.Fatalf("thawEstop error = %v, want a refusal naming the pod setting", err)
	}
	if !estop.IsActive(town) {
		t.Fatal("the Mac ESTOP was lifted on a refused pod setting")
	}
}

func TestThawEstopKeepsTheMacEstopWhenThePodRemoveFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	writePodEnv(t, town)
	fake := holdclaimtest.New()
	fake.Fail["rm"] = errPodDown

	err := thawEstop(town, "", fake.Exec)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("thawEstop error = %v, want the NOT-in-the-pod refusal", err)
	}
	if !estop.IsActive(town) {
		t.Fatal("the Mac ESTOP was removed although the pod kept its copy")
	}
}

// TestThawEstopWithNoMacEstopReportsAPodCopyThatCannotBeLifted covers the
// branch where the Mac has already cleared its sentinel but the pod still has
// a copy: the copy's removal failure is reported, not read as "No E-stop".
func TestThawEstopWithNoMacEstopReportsAPodCopyThatCannotBeLifted(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writePodEnv(t, town)
	fake := holdclaimtest.New()
	fake.Fail["rm"] = errPodDown

	err := thawEstop(town, "", fake.Exec)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("thawEstop error = %v, want the NOT-in-the-pod refusal", err)
	}
}

// TestLiftEstopReportsALocalSentinelThatCannotBeRemoved drives the local
// removal refusal: a non-empty directory where the sentinel should be cannot
// be removed, and the error is not "already gone".
func TestLiftEstopReportsALocalSentinelThatCannotBeRemoved(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	sentinel := estop.FilePath(town)
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sentinel, "inside"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	err := liftEstop(town, "", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to remove ESTOP file") {
		t.Fatalf("liftEstop error = %v, want the local removal failure", err)
	}
}

// writePodEnv writes a complete pod setting into the town's daemon.env.
func writePodEnv(t *testing.T, town string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := holdclaim.ContextEnv + "=kind-gt\n" + holdclaim.NamespaceEnv + "=gt\n"
	if err := os.WriteFile(filepath.Join(town, "settings", "daemon.env"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
