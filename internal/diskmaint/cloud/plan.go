package cloud

import (
	"context"
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

// The reasons a plan is refused. Each is fixed text: a runner's error carries
// the command's output, which a refusal never echoes.
const (
	refuseNoLabels    = "no runner label list is configured; the plan is refused"
	refuseEmptyLabels = "the runner label list names no label; the plan is refused"
	refuseLabelRead   = "the runner label list is unreadable; the plan is refused"
	refuseLabelLine   = "a runner label names a fractals-nextjs image with no tag; the plan is refused"
	refuseLabelName   = "a runner label names a tag or digest docker cannot have; the plan is refused"
	refuseNoDeploy    = "no deploy workflow is configured; the plan is refused"
	refuseEmptyDeploy = "the deploy workflow is empty; the plan is refused"
	refuseDeployRead  = "the deploy workflow is unreadable; the plan is refused"
	refuseDeployName  = "a deploy workflow names a fractals-nextjs image with a tag or digest docker cannot have; the plan is refused"
	refuseUndated     = "a fractals-nextjs tag has no readable created time; the plan is refused"
)

// Plan lists the fractals-nextjs images the two keep rules leave for removal.
// It keeps the repository's newest keepNewest tags and every tag or image a
// runner label or the deploy workflow names, reading both lists through Run
// from cfg.LabelsFile and cfg.DeployFile so the set is never a constant; each
// kept image carries the rule that spared it. A read the plan cannot make, or a
// value it cannot read, refuses the plan with no actions.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget, Policy: p}
	named, namedIDs, why := t.namedRefs(ctx)
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
	plan.Kept, plan.Actions = decide(images, ranked, named, namedIDs)
	return plan, nil
}

// namedRefs returns the fractals-nextjs tags and image ids the runner labels and
// the deploy workflow name, or the fixed reason one of the two lists cannot be
// read. Both are caller settings read through Run: a list this code kept of its
// own would name whatever it chose, and a list it cannot read is a set of images
// it cannot know are in use.
func (t *Target) namedRefs(ctx context.Context) (map[string]bool, map[string]bool, string) {
	tags, ids, why := t.labelRefs(ctx)
	if why != "" {
		return nil, nil, why
	}
	deployTags, deployIDs, why := t.deployRefs(ctx)
	if why != "" {
		return nil, nil, why
	}
	for tag := range deployTags {
		tags[tag] = true
	}
	for id := range deployIDs {
		ids[id] = true
	}
	return tags, ids, ""
}

// labelRefs reads the runner label list the caller configured.
func (t *Target) labelRefs(ctx context.Context) (map[string]bool, map[string]bool, string) {
	if t.cfg.LabelsFile == "" {
		return nil, nil, refuseNoLabels
	}
	// "--" ends cat's options, so a path that starts with a dash is a file name
	// and not a flag.
	out, err := t.cfg.Run(ctx, nil, "cat", "--", t.cfg.LabelsFile)
	if err != nil {
		return nil, nil, refuseLabelRead
	}
	return parseLabels(string(out))
}

// deployRefs reads the deploy workflow the caller configured.
func (t *Target) deployRefs(ctx context.Context) (map[string]bool, map[string]bool, string) {
	if t.cfg.DeployFile == "" {
		return nil, nil, refuseNoDeploy
	}
	out, err := t.cfg.Run(ctx, nil, "cat", "--", t.cfg.DeployFile)
	if err != nil {
		return nil, nil, refuseDeployRead
	}
	return parseDeploy(string(out))
}

// parseLabels returns the fractals-nextjs tags and image ids a runner label list
// names. Each line is one label, "<name>:docker://<ref>", where the ref is the
// image a job started by that label runs in. A line naming no image is a label
// of another kind and is skipped; a ref of this repository that names no tag, or
// a tag or digest docker cannot have, refuses the plan, since the image it names
// is unknown. A file with no label line at all refuses it too: an empty read
// cannot be told from a truncated one, and reading it as an empty keep list
// would free every image outside the newest window.
func parseLabels(text string) (map[string]bool, map[string]bool, string) {
	tags, ids := map[string]bool{}, map[string]bool{}
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
		ref := parseImageRef(strings.TrimSpace(line[i+len(labelMarker):]))
		if repoBase(ref.repo) != fractalsRepo {
			continue
		}
		switch {
		case ref.digest != "":
			if !registry.ValidDigest(ref.digest) {
				return nil, nil, refuseLabelName
			}
			ids[ref.digest] = true
		case ref.tag == "":
			return nil, nil, refuseLabelLine
		case !registry.ValidTag(ref.tag):
			return nil, nil, refuseLabelName
		default:
			tags[ref.tag] = true
		}
	}
	if labels == 0 {
		return nil, nil, refuseEmptyLabels
	}
	return tags, ids, ""
}

// parseDeploy returns the fractals-nextjs tags and image ids a deploy workflow
// names. The workflow is YAML the plan does not parse: every field of every line
// is read as a possible image ref, quoted, in a list, or after an assignment, so
// "image: registry/fractals-nextjs:v1" and "IMAGE=fractals-nextjs:v1" both
// count. A ref with no tag names docker's default tag, latest. A tag or digest docker
// cannot have — a templated one above all, whose image is not known until the
// run — refuses the plan. An empty file refuses it: it cannot be told from a
// truncated read.
func parseDeploy(text string) (map[string]bool, map[string]bool, string) {
	tags, ids := map[string]bool{}, map[string]bool{}
	if strings.TrimSpace(text) == "" {
		return nil, nil, refuseEmptyDeploy
	}
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
			ref := parseImageRef(strings.Trim(field, fieldPunctuation))
			if repoBase(ref.repo) != fractalsRepo {
				continue
			}
			switch {
			case ref.digest != "":
				if !registry.ValidDigest(ref.digest) {
					return nil, nil, refuseDeployName
				}
				ids[ref.digest] = true
			case ref.tag == "":
				tags[latestTag] = true
			case !registry.ValidTag(ref.tag):
				return nil, nil, refuseDeployName
			default:
				tags[ref.tag] = true
			}
		}
	}
	return tags, ids, ""
}

// imageRef is one docker image reference split into the repository it names and
// the tag or digest it pins.
type imageRef struct {
	repo, tag, digest string
}

// parseImageRef splits a ref at its digest, else at its tag. The tag's colon
// counts only after the last slash, so a registry's port stays part of the
// repository: "registry.example:5000/team/fractals-nextjs" pins no tag, and
// "registry.example:5000/team/fractals-nextjs:v1" pins v1.
func parseImageRef(ref string) imageRef {
	if i := strings.Index(ref, "@"); i >= 0 {
		return imageRef{repo: ref[:i], digest: ref[i+1:]}
	}
	if i, slash := strings.LastIndex(ref, ":"), strings.LastIndex(ref, "/"); i > slash {
		return imageRef{repo: ref[:i], tag: ref[i+1:]}
	}
	return imageRef{repo: ref}
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
// docker drops every tag that points at it. named holds the tags a runner label
// or the deploy workflow names, namedIDs the image ids one pins by digest.
func decide(images []dockerimg.Image, ranked []aged, named, namedIDs map[string]bool) ([]diskmaint.Kept, []diskmaint.Action) {
	keptImage := map[string]bool{}
	for _, im := range images {
		if !isFractalsTag(im) {
			keptImage[im.ID] = true
		}
	}
	for i, r := range ranked {
		if i < keepNewest || named[r.im.Tag] {
			keptImage[r.im.ID] = true
		}
	}
	for id := range namedIDs {
		keptImage[id] = true
	}

	var kept []diskmaint.Kept
	var actions []diskmaint.Action
	planned := map[string]bool{}
	for i, r := range ranked {
		switch {
		case i < keepNewest:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleNewest})
		case named[r.im.Tag], namedIDs[r.im.ID]:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleLabel})
		case keptImage[r.im.ID]:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleKeptImage})
		case planned[r.im.ID]:
			// One action per image id: another tag of it already names it.
		default:
			planned[r.im.ID] = true
			actions = append(actions, diskmaint.Action{
				Kind:  actionRemove,
				Ref:   r.im.ID,
				Bytes: r.im.Bytes(),
				Why:   fmt.Sprintf("older than the newest %d %s tags and no runner label or deploy workflow names it", keepNewest, fractalsRepo),
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
