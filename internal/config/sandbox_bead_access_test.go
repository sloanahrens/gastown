package config

import (
	"testing"
)

// bead_access names where a seat's task lives: unset and dolt mean the bead
// store, files is the no-bead-access mode, and any other value is refused with
// an error naming the profile and the key (gt-1m0kb).
func TestSandboxProfileBeadAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile SandboxProfile
		wantErr string // the exact error; "" means valid
	}{
		{
			name:    "unset validates as the dolt default",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s"},
		},
		{
			name:    "dolt validates",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", BeadAccess: SandboxBeadAccessDolt},
		},
		{
			name:    "files validates",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", BeadAccess: SandboxBeadAccessFiles},
		},
		{
			name:    "an unknown value is refused naming the profile and the key",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", BeadAccess: "beads"},
			wantErr: `sandboxes.gastown.bead_access "beads": want dolt or files`,
		},
		{
			name:    "the value is case-sensitive",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", BeadAccess: "Files"},
			wantErr: `sandboxes.gastown.bead_access "Files": want dolt or files`,
		},
		{
			name:    "a near miss is refused",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", BeadAccess: "file"},
			wantErr: `sandboxes.gastown.bead_access "file": want dolt or files`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.profile.Validate("gastown")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// A reader that takes BeadAccess after WithDefaults always sees a mode: unset
// arrives as the dolt default and a set value survives, so no read can carry
// the empty string into a session profile (gt-jwg5ff).
func TestSandboxProfileWithDefaultsFillsBeadAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		set  string
	}{
		{name: "unset becomes the dolt default"},
		{name: "dolt survives", set: SandboxBeadAccessDolt},
		{name: "files survives", set: SandboxBeadAccessFiles},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := tc.set
			if want == "" {
				want = SandboxBeadAccessDolt
			}
			if got := (SandboxProfile{BeadAccess: tc.set}).WithDefaults().BeadAccess; got != want {
				t.Fatalf("WithDefaults().BeadAccess = %q, want %q", got, want)
			}
		})
	}
}
