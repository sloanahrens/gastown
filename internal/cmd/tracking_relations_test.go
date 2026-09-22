package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTrackingDependsOnID_CrossRigWrapsExternal(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte("{\"prefix\":\"ag-\",\"path\":\"agentcompany/.beads\"}\n"), 0o644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	got := trackingDependsOnID(townRoot, "ag-95s.1")
	want := "external:ag:ag-95s.1"
	if got != want {
		t.Fatalf("trackingDependsOnID() = %q, want %q", got, want)
	}
}

func TestTrackingDependsOnID_HQStaysLocal(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	got := trackingDependsOnID(townRoot, "hq-cv-test")
	if got != "hq-cv-test" {
		t.Fatalf("trackingDependsOnID() = %q, want %q", got, "hq-cv-test")
	}
}

func TestIsTrackingTargetID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "local bead id", input: "gt-gsky", want: true},
		{name: "town convoy id", input: "hq-cv-7rzqg", want: true},
		{name: "sub-issue id", input: "om-95s.1", want: true},
		{name: "external cross-rig", input: "external:om:om-95s.1", want: true},
		{name: "external with dotted id", input: "external:ghostty:ghostty-123", want: true},

		// The reported damage (gt-gsky): a convoy *title* recorded where an ID
		// belongs. It reached the dependency table because the title starts with
		// a short lowercase word, so ExtractPrefix found the om- rig and the
		// target was wrapped as an external edge.
		{name: "convoy title", input: "om-gate coverage: om", want: false},
		{name: "title in external form", input: "external:om:om-gate coverage: om", want: false},
		{name: "empty", input: "", want: false},
		{name: "external without id", input: "external:om", want: false},
		{name: "external without rig", input: "external::om-95s.1", want: false},
		{name: "trailing space", input: "om-95s.1 ", want: false},
		{name: "leading hyphen", input: "-force", want: false},
		{name: "flag-like", input: "--force", want: false},
		{name: "colon in id", input: "om:95s", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTrackingTargetID(tc.input); got != tc.want {
				t.Errorf("isTrackingTargetID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestValidateTrackingTargets(t *testing.T) {
	t.Parallel()

	if err := validateTrackingTargets([]string{"gt-gsky", "external:om:om-95s.1", "hq-cv-7rzqg"}); err != nil {
		t.Fatalf("validateTrackingTargets(valid) = %v, want nil", err)
	}
	if err := validateTrackingTargets(nil); err != nil {
		t.Fatalf("validateTrackingTargets(nil) = %v, want nil", err)
	}

	err := validateTrackingTargets([]string{"gt-gsky", "om-gate coverage: om"})
	if err == nil {
		t.Fatal("validateTrackingTargets accepted a convoy title")
	}
	if !strings.Contains(err.Error(), `"om-gate coverage: om"`) {
		t.Errorf("error should name the offending target, got: %v", err)
	}
	if strings.Contains(err.Error(), "gt-gsky") {
		t.Errorf("error should not name valid targets, got: %v", err)
	}
}

// TestAddTrackingRelation_RejectsNonBeadIDTarget pins the invariant that keeps
// a phantom edge out of the dependency table: the write path refuses a target
// that is not a bead ID instead of wrapping it into external:<rig>:<id>
// (gt-gsky). It must refuse before touching the store, so no town workspace is
// needed for the call to fail — a valid target here would instead try to open
// one.
func TestAddTrackingRelation_RejectsNonBeadIDTarget(t *testing.T) {
	t.Parallel()

	err := addTrackingRelation(t.TempDir(), "hq-cv-test", "om-gate coverage: om")
	if err == nil {
		t.Fatal("addTrackingRelation recorded an edge to a non-bead-ID target")
	}
	if !strings.Contains(err.Error(), "not a bead ID") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFallbackTrackingRelationUsesExternalTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte("{\"prefix\":\"ag-\",\"path\":\"agentcompany/.beads\"}\n"), 0o644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeBDStub(t, binDir, `#!/usr/bin/env sh
{
	printf 'args:'
	for arg in "$@"; do
		printf '[%s]' "$arg"
	done
	printf '\n'
} >> "$BD_STUB_LOG"
`, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)

	storeErr := errors.New("store unavailable")
	if err := fallbackTrackingRelation(townRoot, "hq-cv-test", "ag-95s.1", true, storeErr); err != nil {
		t.Fatalf("fallback add: %v", err)
	}
	if err := fallbackTrackingRelation(townRoot, "hq-cv-test", "ag-95s.1", false, storeErr); err != nil {
		t.Fatalf("fallback remove: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	for _, want := range []string{
		"args:[dep][add][hq-cv-test][external:ag:ag-95s.1][--type=tracks]",
		"args:[dep][remove][hq-cv-test][external:ag:ag-95s.1][--type=tracks]",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("bd stub log missing %q:\n%s", want, log)
		}
	}
}
