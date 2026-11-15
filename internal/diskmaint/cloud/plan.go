package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

const (
	// fractalsRepo is the repository whose per-commit tags the newest rule
	// ranks.
	fractalsRepo = "fractals-nextjs"
	// deployBaseRepo is the repository of the base images the deploy builds
	// from, one tag per commit the base was built at. Its tags are candidates
	// too: the rollback rule keeps one of them and the rest are removable.
	deployBaseRepo = "deploy-base"
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
	// fieldPunctuation is what a workflow's YAML, JSON or shell line may put around
	// an image ref: a quote, a list bracket, a brace, a shell grouping character, a
	// comma or semicolon. Each ends a field wherever it sits.
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
	// ruleInUse keeps a tag whose image a container references, in any state: a
	// stopped container is one `docker start` away from needing it, and docker
	// refuses to remove an image a container of any state names.
	ruleInUse = "a container references the image"
	// ruleRollback keeps the newest deploy-base tag other than the one in use,
	// so a deploy that fails can go back one base.
	ruleRollback = "the newest deploy-base tag besides the one in use: the rollback"
	// ruleOther keeps a tag this plan never removes: another repository's, a
	// repository or tag docker printed as absent, or a dangling image.
	ruleOther = "not a fractals-nextjs or deploy-base tag; this plan removes none"
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
	refuseLabelLine   = "a runner label names a fractals-nextjs or deploy-base image with no tag; the plan is refused"
	refuseLabelName   = "a runner label names a tag or digest docker cannot have; the plan is refused"
	refuseLabelRef    = "a runner label names an image ref docker cannot read: %s; the plan is refused"
	refuseNoDeploy    = "no deploy workflow is configured; the plan is refused"
	refuseEmptyDeploy = "the deploy workflow is empty; the plan is refused"
	refuseDeployRead  = "the deploy workflow is unreadable; the plan is refused"
	refuseDeployName  = "a deploy workflow names a fractals-nextjs or deploy-base image with a tag or digest docker cannot have; the plan is refused"
	refuseDeployRef   = "a deploy workflow names an image ref docker cannot read: %s; the plan is refused"
	refuseDigestRead  = "the registry digests the daemon carries are unreadable; the plan is refused"
	refuseUnpinned    = "a runner label or deploy workflow pins an image digest no listed image carries: %s; the plan is refused"
	refuseUndated     = "a fractals-nextjs or deploy-base tag has no readable created time; the plan is refused"
)

// refusals is how one source words the refs it cannot use: an image ref docker
// cannot read (named, so the line is found), a tag or digest docker cannot have,
// a ref of one of the plan's repositories that names no tag, and the tag such a
// ref names where the source allows one.
type refusals struct {
	unreadable, badName, untagged, latest string
}

var (
	labelRefusals  = refusals{unreadable: refuseLabelRef, badName: refuseLabelName, untagged: refuseLabelLine}
	deployRefusals = refusals{unreadable: refuseDeployRef, badName: refuseDeployName, latest: latestTag}
)

// Plan lists the fractals-nextjs and deploy-base images the keep rules leave
// for removal. It keeps the fractals repository's newest keepNewest tags, the
// newest deploy-base tag besides the one in use as the rollback, every image a
// container references, and every tag, digest or image id a runner label or the
// deploy workflow names, reading both lists through Run from cfg.LabelsFile and
// cfg.DeployFile so the set is never a constant; each kept image carries the
// rule that spared it. A read the plan cannot make, or a value it cannot read,
// refuses the plan with no actions.
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
	ranked, why := byAge(images, fractalsRepo)
	if why != "" {
		plan.Refused = why
		return plan, nil
	}
	bases, why := byAge(images, deployBaseRepo)
	if why != "" {
		plan.Refused = why
		return plan, nil
	}
	// Every container's image is read, whatever the container's state: docker
	// refuses to remove an image a stopped container names, and a stopped
	// container is one start away from needing it.
	inUse, err := t.client().InUse(ctx)
	if err != nil {
		plan.Refused = err.Error()
		return plan, nil
	}
	pinned, why := t.pinnedImages(ctx, images, named.digests)
	if why != "" {
		plan.Refused = why
		return plan, nil
	}
	plan.Kept, plan.Actions = decide(planInput{
		images: images,
		ranked: ranked,
		bases:  bases,
		named:  named,
		pinned: pinned,
		inUse:  inUse,
	})
	return plan, nil
}

// named is what the runner labels and the deploy workflow name: tags of the two
// candidate repositories, registry digests they pin, and image ids they name
// outright. The three stay apart because a digest and an image id are different
// values, and a ref that names a tag and a digest puts one in each. A tag is
// held as its repository and tag together (see tagKey), so one repository's tag
// never keeps the other repository's image of the same name.
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
// skipped; a ref of one of the plan's repositories that it cannot read, or one
// that names no tag, refuses the plan, since the image it names is then unknown.
// A file with
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
		// A YAML list item starts with a dash; the field after it is the value. A ref
		// holds no space, quote, bracket, brace, comma, semicolon or "=", so each of
		// them ends a field wherever it sits: a ref glued to JSON ({"image":"repo:tag"}),
		// to a list (['repo:tag']), to an assignment (IMAGE=repo:tag) or to a long flag
		// (--image=repo:tag) is read as the ref alone. A templated tag keeps its "$"
		// in the field, so it still refuses the plan.
		for _, field := range strings.FieldsFunc(strings.TrimPrefix(line, "-"), isFieldBreak) {
			if why := n.place(field, deployRefusals); why != "" {
				return named{}, why
			}
		}
	}
	return n, ""
}

// isFieldBreak reports whether r ends a field in a workflow line: white space,
// or one of the characters a workflow puts around or in front of an image ref
// (fieldPunctuation and "=").
func isFieldBreak(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(fieldPunctuation+"=", r)
}

// place adds one ref to the sets, or returns the refusal for a ref this plan
// cannot use. A ref of another repository names no image this plan removes and
// is skipped. A ref of one of the plan's repositories that docker cannot read
// refuses the plan naming it: the file says it names an image the plan decides
// about and which image is unknown. A ref that names a tag and a digest puts
// both in the sets, so the images either one matches are kept.
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
		n.tags[tagKey(ref.repo, ref.tag)] = true
	case ref.digest != "":
		// The digest names the image and needs no tag.
	case r.latest == "":
		return r.untagged
	default:
		n.tags[tagKey(ref.repo, r.latest)] = true
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

// ours reports whether a ref names one of the two candidate repositories: its
// last path element, read up to a colon, so the tail of a malformed ref still
// reads as ours.
func ours(repo string) bool {
	base := repoBase(repo)
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[:i]
	}
	return base == fractalsRepo || base == deployBaseRepo
}

// tagKey is how a tag is held in the named sets and compared in a plan: the
// repository's base name and the tag together, so a fractals-nextjs tag and a
// deploy-base tag of one name stay apart, and a registry-prefixed ref matches
// the row docker lists under the bare name.
func tagKey(repo, tag string) string { return repoBase(repo) + ":" + tag }

// imageKey is a listed row's tag in the same form as tagKey.
func imageKey(im dockerimg.Image) string { return tagKey(im.Repository, im.Tag) }

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

// aged is one candidate tag and its image's creation time.
type aged struct {
	im      dockerimg.Image
	created time.Time
}

// byAge returns one repository's tags newest first, the tag name breaking a tie
// so the order is stable. A tag whose created time does not parse refuses the
// plan: age is what ranks it, and the zero time would sort it last and plan its
// removal.
func byAge(images []dockerimg.Image, repo string) ([]aged, string) {
	var rows []aged
	for _, im := range images {
		if !isTagOf(im, repo) {
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

// isTagOf reports whether an image row is one tag of repo.
func isTagOf(im dockerimg.Image, repo string) bool {
	return im.Repository == repo && im.Tag != noneField && im.Tag != ""
}

// isCandidate reports whether an image row is one tag this plan may remove: a
// fractals-nextjs or deploy-base tag.
func isCandidate(im dockerimg.Image) bool {
	return isTagOf(im, fractalsRepo) || isTagOf(im, deployBaseRepo)
}

// planInput is what decide splits: every listed image, each candidate
// repository's tags newest first, the refs the two files name, the ids a digest
// pin resolved to, and the ids a container references.
type planInput struct {
	images []dockerimg.Image
	ranked []aged // fractals-nextjs tags, newest first
	bases  []aged // deploy-base tags, newest first
	named  named
	pinned map[string]bool
	inUse  map[string]bool
}

// decide splits every listed image into the kept list and the removals. An
// image id is removed only when no tag of it is kept, no container references
// it, and no tag of it sits outside this plan's candidates, because a removal
// names the id and docker drops every tag that points at it.
func decide(in planInput) ([]diskmaint.Kept, []diskmaint.Action) {
	rollback := rollbackTag(in.bases, in.named, in.pinned, in.inUse)
	// keptImage is every image id a rule keeps. A kept tag keeps its image too,
	// whichever repository the tag is in: a removal names the image id, so a
	// removal planned from another tag of that id would drop the kept tag with
	// it. The base tags are read here as well as in the split below, because a
	// fractals-nextjs tag sharing a base's image is spared by that rule only
	// once the base's id is known to be kept.
	keptImage := map[string]bool{}
	for _, im := range in.images {
		if !isCandidate(im) {
			keptImage[im.ID] = true
		}
	}
	for i, r := range in.ranked {
		if i < keepNewest || in.named.tags[imageKey(r.im)] {
			keptImage[r.im.ID] = true
		}
	}
	for _, r := range in.bases {
		if in.named.tags[imageKey(r.im)] || imageKey(r.im) == rollback {
			keptImage[r.im.ID] = true
		}
	}
	for id := range in.pinned {
		keptImage[id] = true
	}
	for id := range in.named.ids {
		keptImage[id] = true
	}
	for id := range in.inUse {
		keptImage[id] = true
	}

	// tagsOf is every candidate tag of each image, so a removal can name the
	// rows that leave with the image it names.
	tagsOf := make(map[string][]string, len(in.ranked)+len(in.bases))
	for _, r := range in.ranked {
		tagsOf[r.im.ID] = append(tagsOf[r.im.ID], tagRef(r.im))
	}
	for _, r := range in.bases {
		tagsOf[r.im.ID] = append(tagsOf[r.im.ID], tagRef(r.im))
	}

	var kept []diskmaint.Kept
	var actions []diskmaint.Action
	planned := map[string]bool{}
	// add places one candidate tag: in the kept list with the rule that spares
	// it, or in the removals when no rule does.
	add := func(r aged, rule string) {
		switch {
		case rule != "":
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: rule})
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
	for i, r := range in.ranked {
		if i < keepNewest {
			// The window is named as such even for a tag a file also names: the
			// window is what keeps it, and the rule says why nothing in it goes.
			add(r, ruleNewest)
			continue
		}
		add(r, in.spared(r, rollback, keptImage))
	}
	for _, r := range in.bases {
		add(r, in.spared(r, rollback, keptImage))
	}
	for _, im := range in.images {
		if !isCandidate(im) {
			kept = append(kept, diskmaint.Kept{Ref: refOf(im), Rule: ruleOther})
		}
	}
	return kept, actions
}

// spared is the rule that spares one candidate tag, or "" when no rule does.
func (in planInput) spared(r aged, rollback string, keptImage map[string]bool) string {
	switch {
	case in.named.tags[imageKey(r.im)], in.pinned[r.im.ID], in.named.ids[r.im.ID]:
		return ruleLabel
	case in.inUse[r.im.ID]:
		return ruleInUse
	case imageKey(r.im) == rollback:
		return ruleRollback
	case keptImage[r.im.ID]:
		return ruleKeptImage
	}
	return ""
}

// rollbackTag is the newest deploy-base tag besides the ones in use, which a
// deploy rolls back to, or "" when there is no base tag at all. A base is in use
// when a runner label, the deploy workflow, a digest pin or an image id names
// it, or when a container of any state references its image: none of those is
// the base a rollback should go to while it is still needed. When no base is in
// use as far as the plan can tell, the newest tag is the one a deploy would
// start from, and it is kept.
func rollbackTag(bases []aged, n named, pinned, inUse map[string]bool) string {
	for _, r := range bases {
		if n.tags[imageKey(r.im)] || pinned[r.im.ID] || n.ids[r.im.ID] || inUse[r.im.ID] {
			continue
		}
		return imageKey(r.im)
	}
	return ""
}

// removeWhy explains a removal and names every tag it drops, so a dry run shows
// the rows that leave with the image.
func removeWhy(refs []string) string {
	return fmt.Sprintf("no keep rule spares it and no container references it; the removal drops %s", strings.Join(refs, ", "))
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
