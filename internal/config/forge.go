package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultForgeOwner is the Forgejo account a town whose forge.owner is unset
// names: the owner every repository URL under the town is written with.
const DefaultForgeOwner = "sloan"

// ForgeOwnerPlaceholder is the token the repository's data files write where the
// Forgejo owner goes, so the owner is named once, as DefaultForgeOwner or as the
// town's forge.owner.
const ForgeOwnerPlaceholder = "{owner}"

// forgeOwnerForm is the shape of a Forgejo owner: a name a repository URL
// carries as one path element, so it holds no separator and no dot segment.
var forgeOwnerForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ForgeConfig is the town's forge settings, mayor/town.json's "forge" object.
type ForgeConfig struct {
	// Owner is the Forgejo account the town's repositories live under. Nil is
	// the setting's absence, which takes DefaultForgeOwner; an empty or
	// malformed value is refused, never read as the default.
	Owner *string `json:"owner,omitempty"`
}

// ValidForgeOwner reports why owner is not a Forgejo owner name, or nil.
func ValidForgeOwner(owner string) error {
	if owner == "" {
		return errors.New("want a Forgejo owner name, not an empty value")
	}
	if !forgeOwnerForm.MatchString(owner) {
		return fmt.Errorf("%q is not a Forgejo owner name: want letters, digits, '.', '_' or '-'", owner)
	}
	return nil
}

// ResolveForgeOwner is the one reader of the town's Forgejo owner: the
// mayor/town.json forge.owner setting, or DefaultForgeOwner when the caller
// names no town root and when the town's town.json has no forge section or no
// owner. A town.json that does not load is an error, never the default: its
// loader refuses a forge.owner that is not a usable name.
func ResolveForgeOwner(townRoot string) (string, error) {
	if townRoot == "" {
		return DefaultForgeOwner, nil
	}
	town, err := LoadTownConfig(filepath.Join(townRoot, "mayor", "town.json"))
	if errors.Is(err, ErrNotFound) {
		return DefaultForgeOwner, nil
	}
	if err != nil {
		return "", err
	}
	if town.Forge == nil || town.Forge.Owner == nil {
		return DefaultForgeOwner, nil
	}
	return *town.Forge.Owner, nil
}

// ExpandForgeOwnerFromTown expands text's owner placeholder with the town's
// resolved forge.owner, for a reader that has a town root to read the setting
// from. A town.json that does not load is an error, never the default expansion.
func ExpandForgeOwnerFromTown(townRoot string, text []byte) (string, error) {
	owner, err := ResolveForgeOwner(townRoot)
	if err != nil {
		return "", err
	}
	return ExpandForgeOwner(string(text), owner)
}

// ExpandForgeOwner replaces every owner placeholder in text with owner, for a
// reader of a data file that writes the placeholder. An owner that is not a
// usable name is an error: expanding with it writes a repository URL no forge
// serves.
func ExpandForgeOwner(text, owner string) (string, error) {
	if err := ValidForgeOwner(owner); err != nil {
		return "", fmt.Errorf("forge.owner: %w", err)
	}
	return strings.ReplaceAll(text, ForgeOwnerPlaceholder, owner), nil
}

// RefuseUnexpandedForgeOwner refuses what (a repository URL or path) when it
// still holds the owner placeholder, which a reader that parses without
// expanding would otherwise send to the forge. The error names what and the
// fix, never the value: a URL may carry a credential.
func RefuseUnexpandedForgeOwner(what, value string) error {
	if !strings.Contains(value, ForgeOwnerPlaceholder) {
		return nil
	}
	return fmt.Errorf("%s holds %s, which nothing expanded: read the file with ExpandForgeOwner, and set forge.owner if the setting is empty", what, ForgeOwnerPlaceholder)
}
