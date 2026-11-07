package specdispatch

import (
	"strings"
	"testing"
)

// specAccepting is goodSpec carrying acceptance as its acceptance field.
func specAccepting(acceptance string) Spec {
	s := goodSpec()
	s.Acceptance = acceptance
	return s
}

// lintAcceptance lints goodSpec with acceptance as its field, so every check
// here runs the lint the dispatcher reads, not a helper beside it.
func lintAcceptance(acceptance string) LintVerdict {
	return Lint(specAccepting(acceptance), defaultTemplate())
}

// An acceptance item naming the whole gate is refused, and the reason names the
// item and the phrase so the filer can rewrite it (gt-hgt9j.3).
func TestBoxCheckRefusesMakeGate(t *testing.T) {
	t.Parallel()
	v := lintAcceptance("- [ ] make gate passes\n- [ ] a named test passes")
	if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
		t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
	}
	if !strings.Contains(v.Reason, "make gate passes") {
		t.Errorf("reason %q does not name the offending item", v.Reason)
	}
	if !strings.Contains(v.Reason, `"make gate"`) {
		t.Errorf("reason %q does not name the phrase matched", v.Reason)
	}
	if !strings.Contains(v.Reason, "Gate") || !strings.Contains(v.Reason, "overseer") {
		t.Errorf("reason %q does not say the step belongs in the Gate as the overseer's", v.Reason)
	}
}

// Each container and cluster step a polecat's sandbox cannot run is refused,
// one item at a time (gt-hgt9j.3).
func TestBoxCheckRefusesDockerKubectlColima(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		item string
	}{
		{"docker", "docker ps shows the pool"},
		{"kubectl", "kubectl get pods is empty"},
		{"colima", "colima start runs the virtual machine"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance("- [ ] " + tc.item)
			if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
				t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
			}
			if !strings.Contains(v.Reason, tc.item) {
				t.Errorf("reason %q does not name the offending item", v.Reason)
			}
			if !strings.Contains(v.Reason, `"`+tc.name+`"`) {
				t.Errorf("reason %q does not name the phrase %q", v.Reason, tc.name)
			}
		})
	}
}

// An item naming a Go test with a narrow -run filter is a check the polecat
// runs itself, so it passes (gt-hgt9j.3).
func TestBoxCheckPassesNamedTests(t *testing.T) {
	t.Parallel()
	v := lintAcceptance("- [ ] go test ./internal/specdispatch -run 'BoxCheck' -count=1 passes\n- [ ] a changed guard turns TestBoxCheckRefusesMakeGate red")
	if !v.Clean() {
		t.Fatalf("named test refused: %s", v.Line("gt-good"))
	}
}

// The container tool is matched on a word boundary: a dockerfile is a file the
// bead may change, not a run a polecat must make (gt-hgt9j.3).
func TestBoxCheckWordBoundary(t *testing.T) {
	t.Parallel()
	if v := lintAcceptance("- [ ] the dockerfile lists every image"); !v.Clean() {
		t.Fatalf("dockerfile refused as docker: %s", v.Line("gt-good"))
	}
	// The same word standing alone is still refused, so the boundary narrows
	// the match without dropping it.
	if v := lintAcceptance("- [ ] docker build succeeds"); v.Route != RouteRefuse {
		t.Fatalf("docker run passed: %+v", v)
	}
}

// With both an acceptance field and a description acceptance section, the
// field is the list checked: an offending item there is refused even when the
// section names a different step (gt-hgt9j.3).
func TestBoxCheckReadsFieldBeforeSection(t *testing.T) {
	t.Parallel()
	s := specAccepting("- [ ] make gate passes")
	s.Description += "\n\n## Acceptance\n- [ ] kubectl get pods is empty"
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
		t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
	}
	if !strings.Contains(v.Reason, "make gate passes") {
		t.Errorf("reason %q does not name the field's item", v.Reason)
	}
	if strings.Contains(v.Reason, "kubectl get pods") {
		t.Errorf("reason %q names the section's item, not the field's", v.Reason)
	}
}

// The live town and the live daemon are steps only the overseer runs, so each
// phrase the list holds has a row that names it in the refusal (gt-hgt9j.3).
func TestBoxCheckRefusesLiveTown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		item string
	}{
		{"live town", "live town runs the daemon"},
		{"the live", "the live daemon restarts the polecat"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance("- [ ] " + tc.item)
			if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
				t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
			}
			if !strings.Contains(v.Reason, tc.item) {
				t.Errorf("reason %q does not name the offending item", v.Reason)
			}
			if !strings.Contains(v.Reason, `"`+tc.name+`"`) {
				t.Errorf("reason %q does not name the phrase %q", v.Reason, tc.name)
			}
		})
	}
}

// A nested acceptance item is a step under a criterion the polecat still runs,
// so an offending nested item is refused as a top-level one is (gt-hgt9j.3).
func TestBoxCheckRefusesNestedItem(t *testing.T) {
	t.Parallel()
	v := lintAcceptance("- [ ] a named test passes\n  - [ ] kubectl get pods is empty")
	if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
		t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
	}
	if !strings.Contains(v.Reason, "kubectl get pods is empty") {
		t.Errorf("reason %q does not name the nested item", v.Reason)
	}
	if !strings.Contains(v.Reason, `"kubectl"`) {
		t.Errorf("reason %q does not name the phrase matched", v.Reason)
	}
}

// A nested item naming no phrase passes, and a phrase inside a fenced example
// is not an item at all, so neither is refused (gt-hgt9j.3).
func TestBoxCheckPassesNestedItemsAndFencedExamples(t *testing.T) {
	t.Parallel()
	v := lintAcceptance("- [ ] a named test passes\n  - [ ] the sub check passes\n```\n  - [ ] kubectl get pods\n```")
	if !v.Clean() {
		t.Fatalf("nested or fenced item refused: %s", v.Line("gt-good"))
	}
}

// A finding's title and a source path are not steps the polecat runs, so a tool
// name inside either lints OK: the item is the finding's quotation, not an
// instruction (gt-7c84xe).
func TestBoxCheckPassesFindingTitleAndPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		item string
	}{
		{"title names kubectl", "Finding resolved: Claim route now blocks on a kubectl wait (internal/land/claim.go:88)"},
		{"path names docker", "Finding resolved: name the error (internal/gtwrap/docker.go:267)"},
		{"title and path name tools", "Finding resolved: the docker and kubectl error (internal/gtwrap/docker.go)"},
		{"title names a tool before a run verb", "Finding resolved: the kubectl wait blocks; the run stays green (internal/land/claim.go:88)"},
		{"plain path names docker", "the fix in internal/gtwrap/docker.go is covered by a test"},
		{"directory path names docker", "the fix in internal/gtwrap/docker/ is covered by a test"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance("- [ ] " + tc.item + "\n- [ ] a named test passes")
			if !v.Clean() {
				t.Fatalf("quoted title or path refused: %s", v.Line("gt-good"))
			}
		})
	}
}

// An item that tells the worker to run a tool is still refused, even when a
// source path sits beside the command (gt-7c84xe).
func TestBoxCheckRefusesRunInstruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		item string
		want string
	}{
		{"run kubectl", "Run: kubectl apply -f deploy.yaml", "kubectl"},
		{"run docker beside a path", "Run: docker build internal/gtwrap/docker.go", "docker"},
		{"run kubectl after a finding title", "Finding resolved: Claim route blocks on a wait. Run: kubectl apply -f deploy.yaml", "kubectl"},
		{"shell marker after a finding title", "Finding resolved: the claim route blocks (internal/land/claim.go:88). $ kubectl get pods", "kubectl"},
		{"run an absolute tool path", "Run: /usr/bin/kubectl apply -f deploy.yaml", "kubectl"},
		{"run an absolute docker path beside a source path", "Run: /usr/local/bin/docker build internal/gtwrap/docker.go", "docker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance("- [ ] " + tc.item)
			if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
				t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
			}
			if !strings.Contains(v.Reason, `"`+tc.want+`"`) {
				t.Errorf("reason %q does not name the phrase %q", v.Reason, tc.want)
			}
		})
	}
}
