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

// BeadAccessMode is the read a caller takes the mode from: unset reads as the
// dolt default, the two accepted values read back, and any other value is
// refused there, so a caller never carries a mode Validate would reject
// (gt-jwg5ff).
func TestSandboxProfileBeadAccessMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		set     string
		want    string
		wantErr string
	}{
		{name: "unset reads as the dolt default", want: SandboxBeadAccessDolt},
		{name: "dolt reads back", set: SandboxBeadAccessDolt, want: SandboxBeadAccessDolt},
		{name: "files reads back", set: SandboxBeadAccessFiles, want: SandboxBeadAccessFiles},
		{name: "an unknown value is refused", set: "beads", wantErr: `bead_access "beads": want dolt or files`},
		{name: "the value is case-sensitive", set: "Files", wantErr: `bead_access "Files": want dolt or files`},
		{name: "a near miss is refused", set: "file", wantErr: `bead_access "file": want dolt or files`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := SandboxProfile{BeadAccess: tc.set}.BeadAccessMode()
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("BeadAccessMode = %q, %v, want the error %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("BeadAccessMode = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}
