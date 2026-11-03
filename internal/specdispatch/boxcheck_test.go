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
