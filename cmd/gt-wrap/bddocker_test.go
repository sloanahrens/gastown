package main

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// bdDockerConfigFile is a complete bddocker target in the config file.
const bdDockerConfigFile = `{"target": "bddocker", "bddocker": {"container": "gt-bd", "context": "desktop-linux", "caller": "mac.sloan-shell", "timeout_ms": 60000}}`

func TestRunBuildsTheBdDockerTargetFromItsConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdDockerConfigFile)
	if exit := h.run("bd", "show", "gt-x"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.bdDockerBuilt || h.kubeBuilt || h.bdBuilt || h.targetSet {
		t.Fatalf("bddocker built = %v, kubernetes = %v, bdgateway = %v, docker = %v; want the bddocker target only",
			h.bdDockerBuilt, h.kubeBuilt, h.bdBuilt, h.targetSet)
	}
	if h.builtTown != h.town {
		t.Fatalf("built the target for town %q, want %q", h.builtTown, h.town)
	}
	want := bdDockerConfig{Container: "gt-bd", Context: "desktop-linux", Caller: "mac.sloan-shell", TimeoutMS: 60000}
	if h.bdDockerCfg != want {
		t.Fatalf("target config = %+v, want %+v", h.bdDockerCfg, want)
	}
	if got, want := h.fake.call.Argv, []string{"bd", "show", "gt-x"}; !slices.Equal(got, want) {
		t.Fatalf("launched %q, want %q", got, want)
	}
}

// TestRunRefusesAnIncompleteBdDockerConfig: a bddocker target that names no
// container refuses by naming the missing key, and builds nothing.
func TestRunRefusesAnIncompleteBdDockerConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, `{"target": "bddocker"}`)
	if exit := h.run("bd", "show", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	for _, want := range []string{"no \"container\"", "\"gt-bd\""} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", h.stderr, want)
		}
	}
	if h.bdDockerBuilt || h.fake.launched {
		t.Fatal("an incomplete bddocker config built and launched a target")
	}
}

// TestRunRefusesABdDockerCallNotNamedBd: the bddocker target serves bd only, so a
// call under another tool name is refused before any docker command.
func TestRunRefusesABdDockerCallNotNamedBd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdDockerConfigFile)
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "bddocker target runs bd") {
		t.Fatalf("stderr %q does not say the target runs bd", h.stderr)
	}
	if h.fake.launched || h.bdDockerBuilt {
		t.Fatal("a non-bd call through the bddocker target was launched")
	}
}

// TestRunRefusesAnUnwiredBdDockerTarget: a wrapper built without a bddocker
// constructor refuses rather than panicking, and launches nothing.
func TestRunRefusesAnUnwiredBdDockerTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdDockerConfigFile)
	h.surface.newBdDocker = nil
	if exit := h.run("bd", "show", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "no bddocker target is wired") {
		t.Fatalf("stderr %q does not say the target is unwired", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("an unwired bddocker target launched a call")
	}
}

// TestDoctorPrintsTheBdDockerLines: a bddocker diagnosis names the container it
// would reach rather than a URL and token file.
func TestDoctorPrintsTheBdDockerLines(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdDockerConfigFile)
	h.fake.diag = gtwrap.Diagnosis{
		Rung:      gtwrap.RungExec,
		Container: "gt-bd",
		Reason:    "the bd container gt-bd is running and healthy",
	}
	if exit := h.run("bd", doctorFlag); exit != 0 {
		t.Fatalf("run(%s) = %d, want 0; stderr: %s", doctorFlag, exit, h.stderr)
	}
	want := "rung: exec\ncontainer: gt-bd\nreason: the bd container gt-bd is running and healthy\n"
	if got := h.stdout.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
}

// TestRunFileArgRewrittenForBdDocker: the bd container cannot read a Mac path, so
// a file a bd flag names travels as stdin with the flag's value rewritten, as
// through the kubernetes and bdgateway targets.
func TestRunFileArgRewrittenForBdDocker(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdDockerConfigFile)
	path := writeMacFile(t, h.town, "body.md", "the bead text\n")
	if exit := h.run("bd", "create", "--title", "t", "--body-file", path); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	want := []string{"bd", "create", "--title", "t", "--body-file", "-"}
	if !slices.Equal(h.fake.call.Argv, want) {
		t.Errorf("target argv = %q, want %q", h.fake.call.Argv, want)
	}
	if h.fake.call.Stdin == nil {
		t.Fatal("the target got no stdin; the file was not fed")
	}
	if got, _ := io.ReadAll(h.fake.call.Stdin); string(got) != "the bead text\n" {
		t.Errorf("stdin = %q, want the file's bytes", got)
	}
}
