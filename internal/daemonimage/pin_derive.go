package daemonimage

import "strings"

// derivedInputPaths turns the output of `go list -deps -f '{{.Dir}}'` (one
// absolute directory per line) into the module-relative directories the image
// builds from, in order without duplicates (gt-j3354t). root is the module root
// in the same path form go list prints. A directory that is not root or under
// it is dropped: the standard library, another module, and a sibling whose path
// only starts with root's text (/src/devopsx is not under /src/devops).
func derivedInputPaths(out, root string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		rel, ok := moduleRelative(strings.TrimSpace(line), root)
		if !ok || seen[rel] {
			continue
		}
		seen[rel] = true
		paths = append(paths, rel)
	}
	return paths
}

// missingFromInputList returns, in order, each derived directory that no path of
// PinInputPaths("devops") covers, and nil when the list covers them all, so the
// guard can name the directory a static list lacks (gt-tjfeqp).
func missingFromInputList(derived []string) []string {
	paths := PinInputPaths("devops")
	var missing []string
	for _, dir := range derived {
		if !coveredBy(paths, dir) {
			missing = append(missing, dir)
		}
	}
	return missing
}

// moduleRelative returns dir relative to root, with the root itself as ".", and
// false when dir is neither root nor under it.
func moduleRelative(dir, root string) (string, bool) {
	if dir == root {
		return ".", true
	}
	return strings.CutPrefix(dir, root+"/")
}
