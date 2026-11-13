package config

import (
	"strings"
	"testing"
)

// TestForgeOwnerDefaultsToTheCurrentOwner: a town that names no forge.owner
// reads as DefaultForgeOwner, a town that names one reads it, and a value that
// is not a usable owner name is an error rather than the default (gt-ucigk3).
func TestForgeOwnerDefaultsToTheCurrentOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		townJSON  string
		emptyRoot bool // no town root at all, rather than a town without a town.json
		want      string
		wantErr   string
	}{
		{"no town root", "", true, DefaultForgeOwner, ""},
		{"a town with no town.json", "", false, DefaultForgeOwner, ""},
		{"a town with no forge section", testTownJSON + `}`, false, DefaultForgeOwner, ""},
		{"an empty forge section", testTownJSON + `,"forge":{}}`, false, DefaultForgeOwner, ""},
		{"the setting overrides the default", testTownJSON + `,"forge":{"owner":"gastown"}}`, false, "gastown", ""},
		{"an empty owner is refused", testTownJSON + `,"forge":{"owner":""}}`, false, "", "forge.owner"},
		{"an owner with a slash is refused", testTownJSON + `,"forge":{"owner":"org/team"}}`, false, "", "forge.owner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := ""
			if !tt.emptyRoot {
				root = writeDoltTown(t, tt.townJSON, "")
			}
			got, err := ResolveForgeOwner(root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveForgeOwner error = %v, want it to name %q", err, tt.wantErr)
				}
				if got != "" {
					t.Errorf("ResolveForgeOwner = %q with an error; it must not fall back to an owner", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveForgeOwner: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveForgeOwner = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestExpandForgeOwnerReplacesThePlaceholder: expansion writes the owner where
// the placeholder stood, and an owner that is not a usable name is refused
// rather than written into a URL (gt-ucigk3).
func TestExpandForgeOwnerReplacesThePlaceholder(t *testing.T) {
	t.Parallel()
	const pin = "bd http://forgejo:3000/{owner}/beads.git 0123456789abcdef0123456789abcdef01234567"
	got, err := ExpandForgeOwner(pin, "gastown")
	if err != nil {
		t.Fatalf("ExpandForgeOwner: %v", err)
	}
	if want := "bd http://forgejo:3000/gastown/beads.git 0123456789abcdef0123456789abcdef01234567"; got != want {
		t.Errorf("ExpandForgeOwner = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "org/team"} {
		if _, err := ExpandForgeOwner(pin, bad); err == nil || !strings.Contains(err.Error(), "forge.owner") {
			t.Errorf("ExpandForgeOwner(%q) = %v, want a refusal naming forge.owner", bad, err)
		}
	}
}

// TestExpandForgeOwnerFromTown: the town's owner expands the text, no town
// expands with the default, and a town.json that does not load refuses rather
// than expanding with the default (gt-ucigk3).
func TestExpandForgeOwnerFromTown(t *testing.T) {
	t.Parallel()
	pin := []byte("bd http://forgejo:3000/" + ForgeOwnerPlaceholder + "/beads.git")
	if got, err := ExpandForgeOwnerFromTown(writeDoltTown(t, testTownJSON+`,"forge":{"owner":"gastown"}}`, ""), pin); err != nil {
		t.Fatalf("ExpandForgeOwnerFromTown: %v", err)
	} else if !strings.Contains(string(got), "/gastown/") {
		t.Errorf("ExpandForgeOwnerFromTown = %q, want the town's owner", got)
	}
	if got, err := ExpandForgeOwnerFromTown("", pin); err != nil {
		t.Fatalf("ExpandForgeOwnerFromTown with no town: %v", err)
	} else if !strings.Contains(string(got), "/"+DefaultForgeOwner+"/") {
		t.Errorf("ExpandForgeOwnerFromTown with no town = %q, want the setting's default", got)
	}
	if _, err := ExpandForgeOwnerFromTown(writeDoltTown(t, `{"type":"town"`, ""), pin); err == nil {
		t.Error("ExpandForgeOwnerFromTown expanded from a town.json that does not load")
	}
}

// TestRefuseUnexpandedForgeOwner: a value still holding the placeholder is
// refused and names no part of the value, since a URL may carry a credential; a
// value without it passes (gt-ucigk3).
func TestRefuseUnexpandedForgeOwner(t *testing.T) {
	t.Parallel()
	err := RefuseUnexpandedForgeOwner("the pin url", "http://forgejo:3000/"+ForgeOwnerPlaceholder+"/beads.git")
	if err == nil {
		t.Fatal("RefuseUnexpandedForgeOwner accepted a value holding the placeholder")
	}
	if strings.Contains(err.Error(), "forgejo") {
		t.Errorf("the refusal echoes the value: %v", err)
	}
	if err := RefuseUnexpandedForgeOwner("the pin url", "http://forgejo:3000/gastown/beads.git"); err != nil {
		t.Errorf("RefuseUnexpandedForgeOwner refused an expanded value: %v", err)
	}
}
