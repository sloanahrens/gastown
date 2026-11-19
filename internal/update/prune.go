package update

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrPruneRefused is returned by PlanPrune when an input it cannot decide on
// fails closed: a tag with no name, digest or push time, an installed pair with
// no daemon or polecat digest, or a negative newest-tags count. The plan then
// lists no deletions.
var ErrPruneRefused = errors.New("the retention plan refuses this input")

// pruneImages are the only image names the planner decides for, in the order
// the plan lists them. Every other name in the listing is ignored, so no tag of
// another app or a Go module package is ever listed.
var pruneImages = [...]string{"gastown-daemon", "gastown-polecat", "gastown-bd"}

const pruneBdImage = "gastown-bd"

// PruneTag is one tag of an image in the registry listing. The tag's name is
// the sha12 its build was tagged with. Digest is the manifest the tag points
// at, and PushedAt is when it was pushed.
type PruneTag struct {
	Name     string
	Digest   string
	PushedAt time.Time
}

// PruneInput is the listing the planner decides on. Images maps an image name
// to its tags. Installed and Previous are the digest install's pairs, the
// ImageRecord Current and Previous. Green holds the sha12 of every
// green/<sha12> ref. KeepNewest is the number of newest tags kept per image by
// push time. Bd on either pair may be empty, as ImagePair allows.
type PruneInput struct {
	Images     map[string][]PruneTag
	Installed  ImagePair
	Previous   ImagePair
	Green      map[string]bool
	KeepNewest int
}

// PruneEntry is one tag the plan keeps or deletes, with the reason.
type PruneEntry struct {
	Image  string
	Tag    string
	Digest string
	Reason string
}

// PrunePlan lists the tags to keep and the tags to delete, across the three
// images, each entry with its reason.
type PrunePlan struct {
	Keep   []PruneEntry
	Delete []PruneEntry
}

// PlanPrune decides which tags of the three gastown images to keep and which
// are delete candidates. A tag is kept, in priority order, when its digest is
// the installed pair's or the previous pair's, when its name is a green ref's
// sha12, or when it is one of the newest KeepNewest tags of its image. A tag
// that shares a digest with a kept tag is kept too. Everything else is a delete
// candidate. It returns ErrPruneRefused, and an empty plan, on any input it
// cannot decide on.
func PlanPrune(in PruneInput) (PrunePlan, error) {
	if in.KeepNewest < 0 {
		return PrunePlan{}, fmt.Errorf("%w: the newest-tags count is %d", ErrPruneRefused, in.KeepNewest)
	}
	for _, p := range []struct {
		field, digest string
	}{
		{"daemon", in.Installed.Daemon},
		{"polecat", in.Installed.Polecat},
	} {
		if p.digest == "" {
			return PrunePlan{}, fmt.Errorf("%w: the installed pair names no %s digest", ErrPruneRefused, p.field)
		}
	}
	// ImagePair documents Bd as empty for a town with no bd gateway and for a
	// record written before the gateway existed. With that digest unknown, no
	// bd tag can be ruled out as the running gateway, so every bd tag is kept
	// (gt-s3v3ij).
	bdUnknown := in.Installed.Bd == ""

	pinned := map[string]string{}
	for _, d := range []string{in.Installed.Daemon, in.Installed.Polecat, in.Installed.Bd} {
		if d != "" {
			pinned[d] = "installed pair"
		}
	}
	for _, d := range []string{in.Previous.Daemon, in.Previous.Polecat, in.Previous.Bd} {
		if _, ok := pinned[d]; !ok && d != "" {
			pinned[d] = "previous pair"
		}
	}

	type verdict struct {
		image  string
		tag    PruneTag
		rank   int
		total  int
		reason string
	}
	var verdicts []verdict
	for _, image := range pruneImages {
		tags := slices.Clone(in.Images[image])
		for _, t := range tags {
			if err := checkPruneTag(image, t); err != nil {
				return PrunePlan{}, err
			}
		}
		slices.SortFunc(tags, newestFirst)
		for rank, t := range tags {
			verdicts = append(verdicts, verdict{
				image:  image,
				tag:    t,
				rank:   rank,
				total:  len(tags),
				reason: keepReason(image, t, rank, in.KeepNewest, pinned, in.Green, bdUnknown),
			})
		}
	}

	kept := map[string]bool{}
	for _, v := range verdicts {
		if v.reason != "" {
			kept[v.tag.Digest] = true
		}
	}

	var plan PrunePlan
	for _, v := range verdicts {
		entry := PruneEntry{Image: v.image, Tag: v.tag.Name, Digest: v.tag.Digest, Reason: v.reason}
		switch {
		case v.reason != "":
			plan.Keep = append(plan.Keep, entry)
		case kept[v.tag.Digest]:
			entry.Reason = "shares its digest with a kept tag"
			plan.Keep = append(plan.Keep, entry)
		default:
			entry.Reason = deleteReason(v.tag, v.rank, v.total, in.KeepNewest)
			plan.Delete = append(plan.Delete, entry)
		}
	}
	return plan, nil
}

// keepReason names the rule that keeps t, or returns "" when no rule keeps it.
// The pinned digests come first, then the green refs, then the newest tags.
func keepReason(image string, t PruneTag, rank, keepNewest int, pinned map[string]string, green map[string]bool, bdUnknown bool) string {
	if r, ok := pinned[t.Digest]; ok {
		return r
	}
	if image == pruneBdImage && bdUnknown {
		return "the installed pair names no bd digest, so the running bd image cannot be ruled out"
	}
	if green[t.Name] {
		return "green/" + t.Name + " ref"
	}
	if rank < keepNewest {
		return fmt.Sprintf("one of the newest %d tags", keepNewest)
	}
	return ""
}

// deleteReason says why t is a delete candidate, from its own place in the
// image's listing, so every candidate carries its push time and rank.
func deleteReason(t PruneTag, rank, total, keepNewest int) string {
	return fmt.Sprintf("pushed %s, rank %d of %d by push time; not in the installed pair, the previous pair, or a green ref, and not covered by the newest-tags rule (keep newest %d)",
		t.PushedAt.UTC().Format(time.RFC3339), rank+1, total, keepNewest)
}

// checkPruneTag refuses a tag the planner cannot rank, match or name.
func checkPruneTag(image string, t PruneTag) error {
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: a %s tag has no name", ErrPruneRefused, image)
	case t.Digest == "":
		return fmt.Errorf("%w: %s tag %q has no digest", ErrPruneRefused, image, t.Name)
	case t.PushedAt.IsZero():
		return fmt.Errorf("%w: %s tag %q has no push time", ErrPruneRefused, image, t.Name)
	}
	return nil
}

// newestFirst orders tags by push time, newest first, then by name, so the
// plan is the same for the same input.
func newestFirst(a, b PruneTag) int {
	if c := b.PushedAt.Compare(a.PushedAt); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}
