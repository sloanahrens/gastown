// Package imageprune plans which polecat images to remove from the sandbox
// cluster's nodes after a rebuild (gt-e14j6). Each rebuild pushes a new
// gastown-polecat tag, and the nodes keep every one of them: about 2G per node
// per rebuild, which filled the Colima VM's disk twice. The plan keeps the
// images a container uses and the newest Keep tags, and removes the rest.
package imageprune

import (
	"sort"
	"strings"
	"time"
)

// Repo is the image repository whose tags this prunes.
const Repo = "gastown-polecat"

// Image is one image on a node: its id, its tags, its digests (references by
// digest, which an image keeps after its tag moves) and when it was created.
type Image struct {
	ID      string
	Tags    []string
	Digests []string
	Created time.Time
	// Unknown is set when the creation time could not be read: such an image is
	// never a candidate, because its age cannot be ranked.
	Unknown bool
}

// Plan returns the images to remove from one node. An image is kept when a
// container on the node uses it (inUse holds image ids), when it carries the
// "dev" tag, or when it is among the newest keep images by creation time. Only
// images whose tags are all in Repo are candidates; a node's other images are
// never touched.
func Plan(images []Image, inUse map[string]bool, keep int) []Image {
	var candidates []Image
	for _, im := range images {
		if im.Unknown || !onlyPolecatRefs(im) {
			continue
		}
		candidates = append(candidates, im)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Created.After(candidates[j].Created)
	})
	kept := 0
	var remove []Image
	for _, im := range candidates {
		switch {
		case inUse[im.ID] || anyInUse(im.Digests, inUse):
			continue
		case hasTag(im.Tags, "dev"):
			continue
		case kept < keep:
			kept++
			continue
		}
		remove = append(remove, im)
	}
	return remove
}

// onlyPolecatRefs reports whether the image has at least one reference and
// every tag and digest is a gastown-polecat reference. An image that also names
// another repository is left alone.
func onlyPolecatRefs(im Image) bool {
	if len(im.Tags) == 0 && len(im.Digests) == 0 {
		return false
	}
	for _, t := range im.Tags {
		if !isPolecat(t, ":") {
			return false
		}
	}
	for _, d := range im.Digests {
		if !isPolecat(d, "@") {
			return false
		}
	}
	return true
}

// isPolecat reports whether ref names the polecat repository, with or without a
// registry host, using sep (":" for a tag, "@" for a digest) before the rest.
func isPolecat(ref, sep string) bool {
	if i := strings.Index(ref, Repo+sep); i >= 0 {
		return i == 0 || ref[i-1] == '/'
	}
	return false
}

func anyInUse(refs []string, inUse map[string]bool) bool {
	for _, r := range refs {
		if inUse[r] {
			return true
		}
	}
	return false
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == Repo+":"+tag || strings.HasSuffix(t, "/"+Repo+":"+tag) {
			return true
		}
	}
	return false
}

// RemoveAll calls remove for each planned image in order. A failed removal is
// recorded and the run goes on: the image stays on the node, and the next run
// plans it again.
func RemoveAll(plan []Image, remove func(id string) error) (removed []string, failed map[string]error) {
	failed = map[string]error{}
	for _, im := range plan {
		if err := remove(im.ID); err != nil {
			failed[im.ID] = err
			continue
		}
		removed = append(removed, im.ID)
	}
	return removed, failed
}
