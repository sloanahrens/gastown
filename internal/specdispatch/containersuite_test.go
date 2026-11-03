package specdispatch

import (
	"slices"
	"strings"
	"testing"
)

// specWithGate is a clean work bead whose Gate section is gate.
func specWithGate(gate string) Spec {
	s := goodSpec()
	s.Description = strings.Replace(goodDescription, "make gate", gate, 1)
	return s
}

// The runtime guard in internal/cmd/tap_guard_container_suite.go builds its
// containerSuitePackages from ContainerSuitePackages, so this pins the one
// source the guard and the Gate lint share. A package dropped or misspelled
// here would otherwise leave the guard quietly (gt-hgt9j.4).
func TestContainerSuitePackagesMatchesGuard(t *testing.T) {
	t.Parallel()
	want := []string{
		"internal/beads",
		"internal/cmd",
		"internal/crew",
		"internal/daemon",
		"internal/deps",
		"internal/doctor",
		"internal/doltserver",
		"internal/git",
		"internal/mail",
		"internal/plugin",
		"internal/polecat",
		"internal/protocol",
		"internal/rig",
		"internal/testutil",
	}
	if got := ContainerSuitePackages(); !slices.Equal(got, want) {
		t.Fatalf("ContainerSuitePackages() = %v, want %v", got, want)
	}
}

func TestContainerSuiteGateRefusesBarePackage(t *testing.T) {
	t.Parallel()
	v := Lint(specWithGate("go test ./internal/cmd -count=1"), defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("verdict = %s, want a refusal", v.Line("gt-good"))
	}
	if v.Field != "## Gate" {
		t.Errorf("Field = %q, want %q", v.Field, "## Gate")
	}
	for _, want := range []string{"internal/cmd", "-run '<TestName>'"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want it to name %q", v.Reason, want)
		}
	}
}

func TestContainerSuiteGateAcceptsRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gate string
	}{
		{"separate value", "go test ./internal/cmd -run 'TestX' -count=1"},
		{"equals form", "go test ./internal/cmd -run=TestX -count=1"},
		{"backtick-wrapped command", "`go test ./internal/cmd -run 'TestX' -count=1`"},
		{"quoted package argument", "go test './internal/cmd' -run 'TestX' -count=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if v := Lint(specWithGate(tt.gate), defaultTemplate()); !v.Clean() {
				t.Fatalf("verdict = %s, want a clean spec", v.Line("gt-good"))
			}
		})
	}
}

// A Gate line may wrap the command in backticks or quotes, and it may chain
// commands with shell operators. Wrapping must not hide the invocation, and a
// -run on a later command must not excuse an earlier one (gt-hgt9j.4).
func TestContainerSuiteGateStripsWrapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gate string
		pkg  string
	}{
		{"backtick-wrapped command", "`go test ./internal/cmd -count=1`", "internal/cmd"},
		{"double-quoted command", `"go test ./internal/cmd -count=1"`, "internal/cmd"},
		{"single-quoted command", "'go test ./internal/cmd -count=1'", "internal/cmd"},
		{"quoted package argument", "go test './internal/cmd' -count=1", "internal/cmd"},
		{"run on a later && command", "go test ./internal/cmd -count=1 && go test ./internal/specdispatch -run TestX", "internal/cmd"},
		{"run on a later || command", "go test ./internal/daemon -count=1 || go test ./internal/specdispatch -run TestX", "internal/daemon"},
		{"run on a later ; command", "go test ./internal/cmd -count=1; go test ./internal/specdispatch -run TestX", "internal/cmd"},
		{"run on a later | command", "go test ./internal/daemon -count=1 | tee out -run TestX", "internal/daemon"},
		{"bare command after a narrowed one", "go test ./internal/specdispatch -run TestX && go test ./internal/cmd -count=1", "internal/cmd"},
		{"unbalanced quote before a later chain", "go test ./internal/cmd -count=1 ' && go test ./internal/specdispatch -run TestX", "internal/cmd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := Lint(specWithGate(tt.gate), defaultTemplate())
			if v.Route != RouteRefuse {
				t.Fatalf("verdict = %s, want a refusal", v.Line("gt-good"))
			}
			if v.Field != "## Gate" {
				t.Errorf("Field = %q, want %q", v.Field, "## Gate")
			}
			if !strings.Contains(v.Reason, tt.pkg) {
				t.Errorf("Reason = %q, want it to name %s", v.Reason, tt.pkg)
			}
		})
	}
}

// A whole Gate line wrapped in backticks is one quoted region to a shell
// parser, but the lint deletes the wrapping and still splits the chain: the
// first command has no -run, so it is refused (gt-hgt9j.4).
func TestContainerSuiteGateWholeLineWrappedChain(t *testing.T) {
	t.Parallel()
	v := Lint(specWithGate("`go test ./internal/cmd -count=1 && go test ./internal/specdispatch -run TestX`"), defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("verdict = %s, want a refusal", v.Line("gt-good"))
	}
	if v.Field != "## Gate" {
		t.Errorf("Field = %q, want %q", v.Field, "## Gate")
	}
	if !strings.Contains(v.Reason, "internal/cmd") {
		t.Errorf("Reason = %q, want it to name internal/cmd", v.Reason)
	}
}

// -tags run carries the word run as a flag value, not a test filter, so the
// bare package is still refused (gt-hgt9j.4).
func TestContainerSuiteGateTagsValueRunIsNotAFilter(t *testing.T) {
	t.Parallel()
	v := Lint(specWithGate("go test ./internal/cmd -tags run"), defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("verdict = %s, want a refusal", v.Line("gt-good"))
	}
	if v.Field != "## Gate" {
		t.Errorf("Field = %q, want %q", v.Field, "## Gate")
	}
	if !strings.Contains(v.Reason, "internal/cmd") {
		t.Errorf("Reason = %q, want it to name internal/cmd", v.Reason)
	}
}

func TestContainerSuiteGateRefusesEllipsis(t *testing.T) {
	t.Parallel()
	v := Lint(specWithGate("go test ./internal/daemon/... -count=1"), defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("verdict = %s, want a refusal", v.Line("gt-good"))
	}
	if v.Field != "## Gate" {
		t.Errorf("Field = %q, want %q", v.Field, "## Gate")
	}
	if !strings.Contains(v.Reason, "internal/daemon") {
		t.Errorf("Reason = %q, want it to name internal/daemon", v.Reason)
	}
}

func TestContainerSuiteGateIgnoresOtherPackages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gate string
	}{
		{"package outside the list", "go test ./internal/specdispatch -count=1"},
		{"package whose name starts with a listed one", "go test ./internal/cmdfoo -count=1"},
		{"go command that is not test", "go build ./internal/cmd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if v := Lint(specWithGate(tt.gate), defaultTemplate()); !v.Clean() {
				t.Fatalf("verdict = %s, want a clean spec", v.Line("gt-good"))
			}
		})
	}
}

// Every shell separator ends a command, so a -run on a later command never
// narrows an earlier one, and a narrowed command is not refused for its own
// spelling of the flag (gt-hgt9j.4).
func TestContainerSuiteGateSeparatorsAndSpellings(t *testing.T) {
	t.Parallel()
	refused := []string{
		"go test ./internal/cmd -count=1 & go test ./internal/specdispatch -run TestX",
		"(go test ./internal/cmd -count=1) && go test ./internal/specdispatch -run TestX",
		"{ go test ./internal/daemon; } ; go test ./internal/specdispatch -run TestX",
		"go test ./internal/cmd/ -count=1",
	}
	for _, gate := range refused {
		v := Lint(specWithGate(gate), defaultTemplate())
		if v.Route != RouteRefuse || v.Field != "## Gate" {
			t.Errorf("gate %q: verdict = %s, want a ## Gate refusal", gate, v.Line("gt-good"))
		}
	}
	passed := []string{
		"go test ./internal/cmd --run TestX",
		"go test ./internal/cmd --run=TestX",
		"go test ./internal/specdispatch -count=1 & go test ./internal/cmd -run TestY",
	}
	for _, gate := range passed {
		if v := Lint(specWithGate(gate), defaultTemplate()); v.Route != RouteDispatch {
			t.Errorf("gate %q: verdict = %s, want it clean", gate, v.Line("gt-good"))
		}
	}
}
