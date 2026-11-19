package update

import (
	"fmt"
)

// prunedLayer is one layer blob a tag's manifest names. Digest identifies the
// blob. Size is its compressed size in bytes, and it counts only when SizeKnown
// is set: the zero value is an unknown size, never a zero-byte layer.
type prunedLayer struct {
	Digest    string
	Size      int64
	SizeKnown bool
}

// prunedManifest is one tag's layers, as its manifest lists them. Tag names the
// tag in an error.
type prunedManifest struct {
	Tag    string
	Layers []prunedLayer
}

// uniqueBytes returns the compressed bytes a delete of the deleted tags would
// free: the sizes of the layers that appear in a deleted tag and in no kept
// tag, each layer counted once however many deleted tags hold it. Registry
// blobs are shared between tags, so the sum of the deleted tags' own sizes
// would over-state the figure.
//
// The figure is what a delete frees only once the registry's blob garbage
// collection runs. A deleted tag removes its name, not its blobs, so until that
// collection runs the bytes are still on disk.
//
// It returns ErrPruneRefused and 0 on any input it cannot sum: a layer with no
// digest, a layer whose size is unknown or negative in either set, or one
// digest carried with two different sizes. It never returns a partial sum.
func uniqueBytes(deleted, kept []prunedManifest) (int64, error) {
	sizes := map[string]int64{}
	for _, set := range [][]prunedManifest{deleted, kept} {
		for _, m := range set {
			for _, l := range m.Layers {
				if err := checkPrunedLayer(m.Tag, l); err != nil {
					return 0, err
				}
				if s, ok := sizes[l.Digest]; ok && s != l.Size {
					return 0, fmt.Errorf("%w: layer %s is %d bytes in one tag and %d bytes in tag %q", ErrPruneRefused, l.Digest, s, l.Size, m.Tag)
				}
				sizes[l.Digest] = l.Size
			}
		}
	}

	keptDigests := map[string]bool{}
	for _, m := range kept {
		for _, l := range m.Layers {
			keptDigests[l.Digest] = true
		}
	}

	counted := map[string]bool{}
	var total int64
	for _, m := range deleted {
		for _, l := range m.Layers {
			if keptDigests[l.Digest] || counted[l.Digest] {
				continue
			}
			counted[l.Digest] = true
			total += l.Size
		}
	}
	return total, nil
}

// checkPrunedLayer refuses a layer the sum cannot count: one with no digest, or
// one whose compressed size is unknown or negative.
func checkPrunedLayer(tag string, l prunedLayer) error {
	switch {
	case l.Digest == "":
		return fmt.Errorf("%w: tag %q names a layer with no digest", ErrPruneRefused, tag)
	case !l.SizeKnown:
		return fmt.Errorf("%w: layer %s of tag %q has no known size", ErrPruneRefused, l.Digest, tag)
	case l.Size < 0:
		return fmt.Errorf("%w: layer %s of tag %q has a negative size %d", ErrPruneRefused, l.Digest, tag, l.Size)
	}
	return nil
}
