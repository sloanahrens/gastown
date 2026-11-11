package constants

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeadsCustomTypesList(t *testing.T) {
	t.Parallel()
	types := BeadsCustomTypesList()
	expected := []string{"agent", "role", "rig", "convoy", "slot", "queue", "event", "message", "molecule", "gate", "merge-request"}

	if len(types) != len(expected) {
		t.Fatalf("BeadsCustomTypesList() returned %d items, want %d", len(types), len(expected))
	}
	for i, typ := range types {
		if typ != expected[i] {
			t.Errorf("BeadsCustomTypesList()[%d] = %q, want %q", i, typ, expected[i])
		}
	}
}

func TestBeadsInfraTypesList(t *testing.T) {
	t.Parallel()
	types := BeadsInfraTypesList()
	expected := []string{"agent", "role", "message"}

	if len(types) != len(expected) {
		t.Fatalf("BeadsInfraTypesList() returned %d items, want %d", len(types), len(expected))
	}
	for i, typ := range types {
		if typ != expected[i] {
			t.Errorf("BeadsInfraTypesList()[%d] = %q, want %q", i, typ, expected[i])
		}
		if typ == "rig" {
			t.Fatal("rig must stay durable and must not be an infra type")
		}
	}
}

func TestNonDispatchableBeadWispTypes(t *testing.T) {
	t.Parallel()
	got := NonDispatchableBeadWispTypes()
	// Every entry must be one of the durable types, "wisp" excluded (the wisp
	// scan walks the wisps table, where every row is a wisp), and order kept.
	for i, typ := range got {
		found := false
		for _, want := range NonDispatchableBeadTypes {
			if want == typ {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("NonDispatchableBeadWispTypes()[%d] = %q, not in NonDispatchableBeadTypes", i, typ)
		}
		if typ == "wisp" {
			t.Errorf("NonDispatchableBeadWispTypes() includes %q, want it excluded from the wisp scan", typ)
		}
	}
	if want := len(NonDispatchableBeadTypes) - 1; len(got) != want {
		t.Errorf("NonDispatchableBeadWispTypes() returned %d items, want %d", len(got), want)
	}
}

func TestNonDispatchableBeadTypesLabels(t *testing.T) {
	t.Parallel()
	// The representative mail case: a mail bead is typed "task" and distinguished
	// only by its "gt:message" label, so both halves must carry the message
	// family.
	if !containsString(NonDispatchableBeadTypes, "message") {
		t.Error("NonDispatchableBeadTypes missing \"message\"")
	}
	if !containsString(NonDispatchableBeadLabels, "gt:message") {
		t.Error("NonDispatchableBeadLabels missing \"gt:message\"")
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMayorRigsPath(t *testing.T) {
	t.Parallel()
	got := MayorRigsPath("/town")
	expect := "/town/mayor/rigs.json"
	if got != expect {
		t.Errorf("MayorRigsPath = %q, want %q", got, expect)
	}
}

func TestMayorTownPath(t *testing.T) {
	t.Parallel()
	got := MayorTownPath("/town")
	expect := "/town/mayor/town.json"
	if got != expect {
		t.Errorf("MayorTownPath = %q, want %q", got, expect)
	}
}

func TestRigMayorPath(t *testing.T) {
	t.Parallel()
	got := RigMayorPath("/rig")
	expect := "/rig/mayor/rig"
	if got != expect {
		t.Errorf("RigMayorPath = %q, want %q", got, expect)
	}
}

func TestRigBeadsPathTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rig  string
		want string
	}{
		{name: "absolute rig", rig: "/t/r", want: "/t/r/mayor/rig/.beads"},
		{name: "top-level absolute rig", rig: "/rig", want: "/rig/mayor/rig/.beads"},
		{name: "relative rig", rig: "gastown", want: "gastown/mayor/rig/.beads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RigBeadsPath(tt.rig); got != tt.want {
				t.Errorf("RigBeadsPath(%q) = %q, want %q", tt.rig, got, tt.want)
			}
		})
	}
}

// TestRigBeadsPathStates pins which beads directory RigBeadsPath returns for
// each state of <rig>/.beads and the legacy mayor/rig/.beads. Each row builds
// its rig under t.TempDir(), so no host or town path is read.
func TestRigBeadsPathStates(t *testing.T) {
	t.Parallel()
	const meta = "metadata.json"
	const redirect = "redirect"
	tests := []struct {
		name string
		// newFiles are written under <rig>/.beads, which is created first.
		newFiles []string
		// legacyDir creates <rig>/mayor/rig/.beads when true.
		legacyDir bool
		// newIsFile makes <rig>/.beads itself a regular file, so Stat of its
		// children fails with a non-not-exist error.
		newIsFile bool
		wantNew   bool
	}{
		{name: "only legacy", legacyDir: true, wantNew: false},
		{name: "only new", newFiles: []string{meta}, wantNew: true},
		{name: "both, new has redirect", newFiles: []string{meta, redirect}, legacyDir: true, wantNew: false},
		{name: "neither", wantNew: false},
		{name: "both, new has no metadata", newFiles: []string{redirect}, legacyDir: true, wantNew: false},
		{name: "both, new has metadata", newFiles: []string{meta}, legacyDir: true, wantNew: true},
		{name: "new entry unreadable answers new", newIsFile: true, legacyDir: true, wantNew: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := t.TempDir()
			newDir := filepath.Join(rig, DirBeads)
			if tt.newIsFile {
				if err := os.WriteFile(newDir, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if len(tt.newFiles) > 0 {
				if err := os.MkdirAll(newDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range tt.newFiles {
				if err := os.WriteFile(filepath.Join(newDir, name), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			legacy := filepath.Join(rig, DirMayor, DirRig, DirBeads)
			if tt.legacyDir {
				if err := os.MkdirAll(legacy, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			want := legacy
			if tt.wantNew {
				want = newDir
			}
			if got := RigBeadsPath(rig); got != want {
				t.Errorf("RigBeadsPath = %q, want %q", got, want)
			}
		})
	}
}

// TestRigBeadsWritePath pins the creation path: it is <rig>/.beads whether or
// not the rig holds any beads yet.
func TestRigBeadsWritePath(t *testing.T) {
	t.Parallel()
	rig := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rig, DirMayor, DirRig, DirBeads), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rig, DirBeads)
	if got := RigBeadsWritePath(rig); got != want {
		t.Errorf("RigBeadsWritePath = %q, want %q", got, want)
	}
}

// TestRigBeadsPathWarnsOnUnreadableState pins the signal for a Stat error other
// than not-exist: the answer is <rig>/.beads and a warning reaches the stream.
// An absent state answers the legacy directory and stays silent. Each row
// builds its rig under t.TempDir().
func TestRigBeadsPathWarnsOnUnreadableState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, newDir string)
		// wantNew is true when RigBeadsPath answers <rig>/.beads.
		wantNew  bool
		wantWarn bool
	}{
		{
			// <rig>/.beads is a regular file, so Stat of metadata.json under it
			// fails with ENOTDIR.
			name: "metadata entry unreadable",
			setup: func(t *testing.T, newDir string) {
				if err := os.WriteFile(newDir, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantNew:  true,
			wantWarn: true,
		},
		{
			// redirect is a symlink to itself, so Stat fails with ELOOP after
			// metadata.json has been read as present.
			name: "redirect entry unreadable",
			setup: func(t *testing.T, newDir string) {
				if err := os.MkdirAll(newDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(newDir, "metadata.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("redirect", filepath.Join(newDir, "redirect")); err != nil {
					t.Fatal(err)
				}
			},
			wantNew:  true,
			wantWarn: true,
		},
		{
			name:     "absent state is silent",
			setup:    func(t *testing.T, newDir string) {},
			wantNew:  false,
			wantWarn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := t.TempDir()
			newDir := RigBeadsWritePath(rig)
			tt.setup(t, newDir)
			want := filepath.Join(rig, DirMayor, DirRig, DirBeads)
			if tt.wantNew {
				want = newDir
			}
			var warn bytes.Buffer
			if got := rigBeadsPath(rig, &warn); got != want {
				t.Errorf("rigBeadsPath = %q, want %q", got, want)
			}
			if gotWarn := warn.Len() > 0; gotWarn != tt.wantWarn {
				t.Errorf("warning written = %q, want a warning: %v", warn.String(), tt.wantWarn)
			}
		})
	}
}

// TestLegacyRigBeadsPathIgnoresTheNewDirectory pins that LegacyRigBeadsPath
// names mayor/rig/.beads even when <rig>/.beads holds the rig's data, so a
// caller that tries both locations still reaches the legacy one.
func TestLegacyRigBeadsPathIgnoresTheNewDirectory(t *testing.T) {
	t.Parallel()
	rig := t.TempDir()
	if err := os.MkdirAll(RigBeadsWritePath(rig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(RigBeadsWritePath(rig), "metadata.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rig, DirMayor, DirRig, DirBeads)
	if got := LegacyRigBeadsPath(rig); got != want {
		t.Errorf("LegacyRigBeadsPath = %q, want %q", got, want)
	}
	if got := RigBeadsPath(rig); got != RigBeadsWritePath(rig) {
		t.Errorf("RigBeadsPath = %q, want the new directory %q", got, RigBeadsWritePath(rig))
	}
}

func TestRigPolecatsPath(t *testing.T) {
	t.Parallel()
	got := RigPolecatsPath("/rig")
	expect := "/rig/polecats"
	if got != expect {
		t.Errorf("RigPolecatsPath = %q, want %q", got, expect)
	}
}

func TestRigCrewPath(t *testing.T) {
	t.Parallel()
	got := RigCrewPath("/rig")
	expect := "/rig/crew"
	if got != expect {
		t.Errorf("RigCrewPath = %q, want %q", got, expect)
	}
}

func TestMayorConfigPath(t *testing.T) {
	t.Parallel()
	got := MayorConfigPath("/town")
	expect := "/town/mayor/config.json"
	if got != expect {
		t.Errorf("MayorConfigPath = %q, want %q", got, expect)
	}
}

func TestTownRuntimePath(t *testing.T) {
	t.Parallel()
	got := TownRuntimePath("/town")
	expect := "/town/.runtime"
	if got != expect {
		t.Errorf("TownRuntimePath = %q, want %q", got, expect)
	}
}

func TestRigRuntimePath(t *testing.T) {
	t.Parallel()
	got := RigRuntimePath("/rig")
	expect := "/rig/.runtime"
	if got != expect {
		t.Errorf("RigRuntimePath = %q, want %q", got, expect)
	}
}

func TestRigSettingsPath(t *testing.T) {
	t.Parallel()
	got := RigSettingsPath("/rig")
	expect := "/rig/settings"
	if got != expect {
		t.Errorf("RigSettingsPath = %q, want %q", got, expect)
	}
}

func TestMayorAccountsPath(t *testing.T) {
	t.Parallel()
	got := MayorAccountsPath("/town")
	expect := "/town/mayor/accounts.json"
	if got != expect {
		t.Errorf("MayorAccountsPath = %q, want %q", got, expect)
	}
}

// TestTestSocketName pins the shape the doctor's tmux-test-socket check reads:
// the owning pid is the trailing field, and the name is unique per call.
func TestTestSocketName(t *testing.T) {
	t.Parallel()
	name := TestSocketName("gt-test-tmux")
	if !strings.HasPrefix(name, "gt-test-tmux-") {
		t.Fatalf("TestSocketName() = %q, want the gt-test-tmux- prefix", name)
	}
	if !strings.HasSuffix(name, fmt.Sprintf("-%d", os.Getpid())) {
		t.Errorf("TestSocketName() = %q, want it to end with the owning pid %d", name, os.Getpid())
	}
	if again := TestSocketName("gt-test-tmux"); again == name {
		t.Errorf("two calls returned the same name %q", name)
	}
	if strings.ContainsAny(name, "/ .") {
		t.Errorf("TestSocketName() = %q, want no character tmux rejects in a socket name", name)
	}
}
