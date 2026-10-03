package cmd

import (
	"fmt"
)

// ConvoyInfo holds convoy details for an issue's tracking convoy.
type ConvoyInfo struct {
	ID            string // Convoy bead ID (e.g., "hq-cv-abc")
	Owned         bool   // true if convoy has gt:owned label
	MergeStrategy string // "mr", "local", or "" (default = mr)
}

// validateConvoyMergeFlag checks a --merge value for gt convoy create.
// "direct" was removed (gt-fcxe9.4): it pushed polecat branches to
// the default branch from gt done with no gate, no om review and no merge
// slot (G2-02), and no convoy ever used it.
func validateConvoyMergeFlag(v string) error {
	switch v {
	case "", "mr", "local":
		return nil
	case "direct":
		return fmt.Errorf("--merge=direct was removed (gt-fcxe9.4): work lands through the merge queue (--merge=mr, the default) or stays on its branch (--merge=local)")
	default:
		return fmt.Errorf("invalid --merge value %q: must be mr or local", v)
	}
}
