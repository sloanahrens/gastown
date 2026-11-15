package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

const (
	// fractalsRepo is the repository whose per-commit tags the newest rule
	// ranks.
	fractalsRepo = "fractals-nextjs"
	// keepNewest is how many of the repository's newest tags a plan always
	// keeps. It is a constant so the kept list cannot follow a caller's
	// setting, and its value is what the kept rule names.
	keepNewest = 5
	// actionRemove is the only Action kind this plan carries.
	actionRemove = "remove-image"
	// noneField is how docker prints a repository or tag an image does not have.
	noneField = "<none>"
	// labelMarker separates a runner label's name from the image it names, in
	// the form forgejo-runner register takes.
	labelMarker = ":docker://"
	// latestTag is the tag docker reads on a ref that names none.
	latestTag = "latest"
	// imageTimeLayout is the created time docker image ls prints.
	imageTimeLayout = "2006-01-02 15:04:05 -0700 MST"
	// fieldPunctuation is what a workflow's YAML or shell line may put around an
	// image ref: a quote, a list bracket, a shell grouping character, a trailing
	// comma or semicolon.
	fieldPunctuation = "\"'`,;()[]{}"
	// inspectFormat asks docker for an image's id and the registry refs it
	// carries, one line per image, because the id docker lists is the device's
	// name for the image and a pin names the registry's.
	inspectFormat = "{{.Id}} {{json .RepoDigests}}"
	// refNameMax bounds the ref a refusal names, so one malformed line cannot
	// put a whole file into a log.
	refNameMax = 120
)

// The rules a Kept entry names, one per way an image survives a plan.
const (
	// ruleLabel keeps a tag a runner label or the deploy workflow names. A
	// deploy workflow starts its jobs on a runner label, and the label's image
	// is the image those jobs run in; the workflow names its app image itself,
	// which is why both sources are read.
	ruleLabel = "named by a runner label or deploy workflow"
	// ruleKeptImage keeps a tag whose image another rule or another tag keeps:
	// a removal names the image id, and docker drops every tag of it.
	ruleKeptImage = "shares an image a rule keeps"
	// ruleOther keeps a tag this plan never removes: another repository's, a
	// repository or tag docker printed as absent, or a dangling image.
	ruleOther = "not a fractals-nextjs tag; this plan removes none"
)

// ruleNewest names the newest rule with the count it keeps, so a dry run says
// how many tags it spares.
var ruleNewest = fmt.Sprintf("among the newest %d %s tags", keepNewest, fractalsRepo)

// The reasons a plan is refused. A runner's error carries the command's output,
// which a refusal never echoes; the three that name a ref take it from a file the
// caller configured, cut at refNameMax, so the operator can find the line.
const (
	refuseNoLabels    = "no runner label list is configured; the plan is refused"
	refuseEmptyLabels = "the runner label list names no label; the plan is refused"
	refuseLabelRead   = "the runner label list is unreadable; the plan is refused"
	refuseLabelShort  = "the runner label list does not end in a newline, so the read is cut short; the plan is refused"
	refuseLabelLine   = "a runner label names a fractals-nextjs image with no tag; the plan is refused"
	refuseLabelName   = "a runner label names a tag or digest docker cannot have; the plan is refused"
	refuseLabelRef    = "a runner label names an image ref docker cannot read: %s; the plan is refused"
	refuseNoDeploy    = "no deploy workflow is configured; the plan is refused"
	refuseEmptyDeploy = "the deploy workflow is empty; the plan is refused"
	refuseDeployRead  = "the deploy workflow is unreadable; the plan is refused"
	refuseDeployName  = "a deploy workflow names a fractals-nextjs image with a tag or digest docker cannot have; the plan is refused"
	refuseDeployRef   = "a deploy workflow names an image ref docker cannot read: %s; the plan is refused"
	refuseDigestRead  = "the registry digests the daemon carries are unreadable; the plan is refused"
	refuseUnpinned    = "a runner label or deploy workflow pins an image digest no listed image carries: %s; the plan is refused"
	refuseUndated     = "a fractals-nextjs tag has no readable created time; the plan is refused"
)

// refusals is how one source words the refs it cannot use: an image ref docker
// cannot read (named, so the line is found), a tag or digest docker cannot have,
// a ref of this repository that names no tag, and the tag such a ref names where
// the source allows one.
type refusals struct {
	unreadable, badName, untagged, latest string
}

var (
	labelRefusals  = refusals{unreadable: refuseLabelRef, badName: refuseLabelName, untagged: refuseLabelLine}
	deployRefusals = refusals{unreadable: refuseDeployRef, badName: refuseDeployName, latest: latestTag}
)

// Plan lists the fractals-nextjs images the two keep rules leave for removal.
// It keeps the repository's newest keepNewest tags and every tag, digest or
// image id a runner label or the deploy workflow names, reading both lists
// through Run from cfg.LabelsFile and cfg.DeployFile so the set is never a
// constant; each kept image carries the rule that spared it. A read the plan
// cannot make, or a value it cannot read, refuses the plan with no actions.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget, Policy: p}
	named, why := t.namedRefs(ctx)
	if why != "" {
		plan.Refused = why
		return plan, nil
	}
	images, err := t.client().Images(ctx)
	if err != nil {
		plan.Refused = err.Error()
		return plan, nil
	}
	ranked, why := byAge(images)
	if why != "" {
		plan.Refused = why
		return plan, nil
	}
	pinned, why := t.pinnedImages(ctx, images, named.digests)
	if why != "" {
		plan.Refused = why
		return plan, nil
	}
	plan.Kept, plan.Actions = decide(images, ranked, named, pinned)
	return plan, nil
}

// named is what the runner labels and the deploy workflow name: tags of the
// repository, registry digests they pin, and image ids they name outright. The
// three stay apart because a digest and an image id are different values, and a
// ref that names a tag and a digest puts one in each.
type named struct {
	tags, digests, ids map[string]bool
}

// newNamed returns empty sets ready to add to.
func newNamed() named {
	return named{tags: map[string]bool{}, digests: map[string]bool{}, ids: map[string]bool{}}
}

// merge adds another source's refs to this one.
func (n *named) merge(o named) {
	for tag := range o.tags {
		n.tags[tag] = true
	}
	for digest := range o.digests {
		n.digests[digest] = true
	}
	for id := range o.ids {
		n.ids[id] = true
	}
}

// namedRefs returns the refs the runner labels and the deploy workflow name, or
// the fixed reason one of the two lists cannot be read. Both are caller settings
// read through Run: a list this code kept of its own would name whatever it
// chose, and a list it cannot read is a set of images it cannot know are in use.
func (t *Target) namedRefs(ctx context.Context) (named, string) {
	labels, why := t.labelRefs(ctx)
	if why != "" {
		return named{}, why
	}
	deploy, why := t.deployRefs(ctx)
	if why != "" {
		return named{}, why
	}
	labels.merge(deploy)
	return labels, ""
}

// labelRefs reads the runner label list the caller configured.
func (t *Target) labelRefs(ctx context.Context) (named, string) {
	if t.cfg.LabelsFile == "" {
		return named{}, refuseNoLabels
	}
	// "--" ends cat's options, so a path that starts with a dash is a file name
	// and not a flag.
	out, err := t.cfg.Run(ctx, nil, "cat", "--", t.cfg.LabelsFile)
	if err != nil {
		return named{}, refuseLabelRead
	}
	return parseLabels(string(out))
}

// deployRefs reads the deploy workflow the caller configured.
func (t *Target) deployRefs(ctx context.Context) (named, string) {
	if t.cfg.DeployFile == "" {
		return named{}, refuseNoDeploy
	}
	out, err := t.cfg.Run(ctx, nil, "cat", "--", t.cfg.DeployFile)
	if err != nil {
		return named{}, refuseDeployRead
	}
	return parseDeploy(string(out))
}

// parseLabels returns the refs a runner label list names. Each line is one
// label, "<name>:docker://<ref>", where the ref is the image a job started by
// that label runs in. A line naming no image is a label of another kind and is
// skipped; a ref of this repository the plan cannot read, or one that names no
// tag, refuses the plan, since the image it names is then unknown. A file with
// no label line, or one whose last line has no newline after it, refuses the
// plan too: a short read cannot be told from a file that names fewer labels, and
// reading one as a smaller keep list would free images a label still uses.
func parseLabels(text string) (named, string) {
	if strings.TrimSpace(text) == "" {
		return named{}, refuseEmptyLabels
	}
	if !strings.HasSuffix(text, "\n") {
		return named{}, refuseLabelShort
	}
	n := newNamed()
	labels := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		labels++
		i := strings.Index(line, labelMarker)
		if i < 0 {
			continue
		}
		if why := n.place(strings.TrimSpace(line[i+len(labelMarker):]), labelRefusals); why != "" {
			return named{}, why
		}
	}
	if labels == 0 {
		return named{}, refuseEmptyLabels
	}
	return n, ""
}

// parseDeploy returns the refs a deploy workflow names. The workflow is YAML the
// plan does not parse: every field of every line is read as a possible image
// ref, quoted, in a list, or after an assignment, so "image:
// registry/fractals-nextjs:v1" and "IMAGE=fractals-nextjs:v1" both count. A ref
// with no tag names docker's default tag, latest. A tag or digest docker cannot
// have — a templated one above all, whose image is not known until the run —
// refuses the plan. An empty file refuses it: it cannot be told from a truncated
// read.
func parseDeploy(text string) (named, string) {
	if strings.TrimSpace(text) == "" {
		return named{}, refuseEmptyDeploy
	}
	n := newNamed()
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A YAML list item starts with a dash; the field after it is the value.
		for _, field := range strings.Fields(strings.TrimPrefix(line, "-")) {
			// A ref holds no "=", so in an assignment (IMAGE=repo:tag) or a long
			// flag (--image=repo:tag) the ref is the part after the last one.
			if i := strings.LastIndex(field, "="); i >= 0 {
				field = field[i+1:]
			}
			if why := n.place(strings.Trim(field, fieldPunctuation), deployRefusals); why != "" {
				return named{}, why
			}
		}
	}
	return n, ""
}

// place adds one ref to the sets, or returns the refusal for a ref this plan
// cannot use. A ref of another repository names no image this plan removes and
// is skipped. A ref of this repository that docker cannot read refuses the plan
// naming it: the file says it names our image and which image is unknown. A ref
// that names a tag and a digest puts both in the sets, so the images either one
// matches are kept.
func (n *named) place(raw string, r refusals) string {
	ref, ok := parseImageRef(raw)
	if ref.id != "" {
		n.ids[ref.id] = true
		return ""
	}
	if !ok {
		// A digest with no repository in front of it is refused as well: nothing
		// says whose image it names, and a real digest there is a pin the plan
		// has to honor.
		if ours(ref.repo) || (ref.repo == "" && registry.ValidDigest(ref.digest)) {
			return fmt.Sprintf(r.unreadable, refForName(raw))
		}
		return ""
	}
	if !ours(ref.repo) {
		return ""
	}
	if ref.digest != "" {
		if !registry.ValidDigest(ref.digest) {
			return r.badName
		}
		n.digests[ref.digest] = true
	}
	switch {
	case ref.tag != "":
		if !registry.ValidTag(ref.tag) {
			return r.badName
		}
		n.tags[ref.tag] = true
	case ref.digest != "":
		// The digest names the image and needs no tag.
	case r.latest == "":
		return r.untagged
	default:
		n.tags[r.latest] = true
	}
	return ""
}

// imageRef is one docker image reference split into the repository it names and
// the tag, digest or image id it pins.
type imageRef struct {
	repo, tag, digest, id string
}

// parseImageRef reads a ref as a repository, tag, digest or image id, and
// reports false for one docker cannot read: no repository, a second tag, a tag
// with no name, no digest after the "@", or a second digest. The repository
// comes back even then, so a caller can tell a malformed ref of this repository
// from a ref of another one.
func parseImageRef(raw string) (imageRef, bool) {
	if registry.ValidDigest(raw) {
		return imageRef{id: raw}, true
	}
	var out imageRef
	name, pinned := raw, false
	if i := strings.Index(raw, "@"); i >= 0 {
		name, out.digest, pinned = raw[:i], raw[i+1:], true
	}
	repo, tag, tagged := splitTag(name)
	out.repo, out.tag = repo, tag
	switch {
	case out.repo == "":
		return out, false
	case strings.Contains(repoBase(out.repo), ":"):
		return out, false
	case tagged && out.tag == "":
		return out, false
	case pinned && (out.digest == "" || strings.Contains(out.digest, "@")):
		return out, false
	}
	return out, true
}

// splitTag splits a repository from the tag after its last slash, and reports
// whether a tag colon was there: a registry's port is a colon before the last
// slash and stays part of the repository.
func splitTag(name string) (repo, tag string, tagged bool) {
	if i, slash := strings.LastIndex(name, ":"), strings.LastIndex(name, "/"); i > slash {
		return name[:i], name[i+1:], true
	}
	return name, "", false
}

// ours reports whether a ref names this repository: its last path element, read
// up to a colon, so the tail of a malformed ref still reads as ours.
func ours(repo string) bool {
	base := repoBase(repo)
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[:i]
	}
	return base == fractalsRepo
}

// repoBase is a repository's last path element, so a label naming
// registry.example/team/fractals-nextjs matches the repository docker lists as
// fractals-nextjs.
func repoBase(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// refForName is a ref as a refusal names it, cut at refNameMax bytes so one
// malformed line cannot put a whole file into a log.
func refForName(ref string) string {
	if len(ref) > refNameMax {
		return ref[:refNameMax] + "..."
	}
	return ref
}

// pinnedImages resolves the registry digests the labels and the workflow pin to
// the image ids the daemon says carry them, reading the daemon only when a pin
// exists. A pin is matched against an image's registry refs, never its id: the
// id is this device's name for the image and the digest is the content's. A pin
// no listed image carries refuses the plan naming it, because nothing says the
// image it names is gone rather than read past.
func (t *Target) pinnedImages(ctx context.Context, images []dockerimg.Image, pins map[string]bool) (map[string]bool, string) {
	if len(pins) == 0 {
		return map[string]bool{}, ""
	}
	carried, why := t.registryDigests(ctx, images)
	if why != "" {
		return nil, why
	}
	byDigest := map[string][]string{}
	for id, refs := range carried {
		for _, ref := range refs {
			d := digestOf(ref)
			byDigest[d] = append(byDigest[d], id)
		}
	}
	ids := map[string]bool{}
	var unpinned []string
	for pin := range pins {
		if len(byDigest[pin]) == 0 {
			unpinned = append(unpinned, pin)
			continue
		}
		for _, id := range byDigest[pin] {
			ids[id] = true
		}
	}
	if len(unpinned) == 0 {
		return ids, ""
	}
	sort.Strings(unpinned)
	return nil, fmt.Sprintf(refuseUnpinned, unpinned[0])
}

// registryDigests reads the registry refs every listed image carries in one
// docker inspect call. A read that fails, a line that does not parse, a ref that
// is not a repository digest, or an image the daemon does not answer for refuses
// the plan: a pin matched against a partial map would free the image it names.
func (t *Target) registryDigests(ctx context.Context, images []dockerimg.Image) (map[string][]string, string) {
	ids := make([]string, 0, len(images))
	seen := make(map[string]bool, len(images))
	for _, im := range images {
		if !seen[im.ID] {
			seen[im.ID] = true
			ids = append(ids, im.ID)
		}
	}
	if len(ids) == 0 {
		return map[string][]string{}, ""
	}
	args := append([]string{"image", "inspect", "--format", inspectFormat}, ids...)
	out, err := t.client().Docker(ctx, args...)
	if err != nil {
		return nil, refuseDigestRead
	}
	carried := make(map[string][]string, len(ids))
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, rest, ok := strings.Cut(line, " ")
		if !ok || !registry.ValidDigest(id) {
			return nil, refuseDigestRead
		}
		var refs []string
		if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &refs); err != nil {
			return nil, refuseDigestRead
		}
		for _, ref := range refs {
			if !registry.ValidDigest(digestOf(ref)) {
				return nil, refuseDigestRead
			}
		}
		carried[id] = refs
	}
	if len(carried) != len(ids) {
		return nil, refuseDigestRead
	}
	return carried, ""
}

// digestOf is the digest part of a repo@digest ref, or "" when it names none: a
// pin names the same content whatever host prefix the ref carries, so the digest
// alone matches it to a local image.
func digestOf(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// aged is one fractals-nextjs tag and its image's creation time.
type aged struct {
	im      dockerimg.Image
	created time.Time
}

// byAge returns the repository's tags newest first, the tag name breaking a tie
// so the order is stable. A tag whose created time does not parse refuses the
// plan: age is what ranks it, and the zero time would sort it last and plan its
// removal.
func byAge(images []dockerimg.Image) ([]aged, string) {
	var rows []aged
	for _, im := range images {
		if !isFractalsTag(im) {
			continue
		}
		created, err := time.Parse(imageTimeLayout, im.CreatedAt)
		if err != nil {
			return nil, refuseUndated
		}
		rows = append(rows, aged{im: im, created: created})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].created.Equal(rows[j].created) {
			return rows[i].created.After(rows[j].created)
		}
		return rows[i].im.Tag < rows[j].im.Tag
	})
	return rows, ""
}

// isFractalsTag reports whether an image row is one tag of the repository.
func isFractalsTag(im dockerimg.Image) bool {
	return im.Repository == fractalsRepo && im.Tag != noneField && im.Tag != ""
}

// decide splits every listed image into the kept list and the removals. An
// image id is removed only when no tag of it is kept and no tag of it sits
// outside this repository's candidates, because a removal names the id and
// docker drops every tag that points at it. n holds the refs a runner label or
// the deploy workflow names, pinned the image ids a digest pin resolved to.
func decide(images []dockerimg.Image, ranked []aged, n named, pinned map[string]bool) ([]diskmaint.Kept, []diskmaint.Action) {
	keptImage := map[string]bool{}
	for _, im := range images {
		if !isFractalsTag(im) {
			keptImage[im.ID] = true
		}
	}
	for i, r := range ranked {
		if i < keepNewest || n.tags[r.im.Tag] {
			keptImage[r.im.ID] = true
		}
	}
	for id := range pinned {
		keptImage[id] = true
	}
	for id := range n.ids {
		keptImage[id] = true
	}

	// tagsOf is every tag of each image, so a removal can name the rows that
	// leave with the image it names.
	tagsOf := make(map[string][]string, len(ranked))
	for _, r := range ranked {
		tagsOf[r.im.ID] = append(tagsOf[r.im.ID], r.im.Tag)
	}

	var kept []diskmaint.Kept
	var actions []diskmaint.Action
	planned := map[string]bool{}
	for i, r := range ranked {
		switch {
		case i < keepNewest:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleNewest})
		case n.tags[r.im.Tag], pinned[r.im.ID], n.ids[r.im.ID]:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleLabel})
		case keptImage[r.im.ID]:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleKeptImage})
		case planned[r.im.ID]:
			// One action per image id: another tag of it already names it, and
			// the action names every tag it drops.
		default:
			planned[r.im.ID] = true
			actions = append(actions, diskmaint.Action{
				Kind:  actionRemove,
				Ref:   r.im.ID,
				Bytes: r.im.Bytes(),
				Why:   removeWhy(tagsOf[r.im.ID]),
			})
		}
	}
	for _, im := range images {
		if !isFractalsTag(im) {
			kept = append(kept, diskmaint.Kept{Ref: refOf(im), Rule: ruleOther})
		}
	}
	return kept, actions
}

// removeWhy explains a removal and names every tag it drops, so a dry run shows
// the rows that leave with the image.
func removeWhy(tags []string) string {
	refs := make([]string, len(tags))
	for i, tag := range tags {
		refs[i] = fractalsRepo + ":" + tag
	}
	return fmt.Sprintf("older than the newest %d %s tags and no runner label or deploy workflow names it; the removal drops %s",
		keepNewest, fractalsRepo, strings.Join(refs, ", "))
}

// tagRef names a tag the plan decided not to remove.
func tagRef(im dockerimg.Image) string { return im.Repository + ":" + im.Tag }

// refOf names an image row the plan keeps for a reason other than its tag: a
// repository or tag docker printed as absent has no repo:tag to name.
func refOf(im dockerimg.Image) string {
	if im.Repository == noneField || im.Tag == noneField {
		return noneField + "@" + im.ID
	}
	return tagRef(im)
}
