package beads

// IsValidBeadID reports whether s is safe to interpolate into a dependency
// query: bead IDs hold only letters, digits, hyphens, dots and underscores.
func IsValidBeadID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	return true
}

// IsBeadIDToken reports whether s is shaped like a bead ID: a leading
// alphanumeric followed by letters, digits, hyphen, underscore, or dot.
//
// This is the check that keeps a convoy *title* out of the dependency table: a
// name like "om-gate coverage: om" opens with a short lowercase word, so the
// cross-rig resolver wrapped it as external:om:<name> and the convoy could
// never resolve that edge — or auto-close — again (gt-gsky).
func IsBeadIDToken(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
