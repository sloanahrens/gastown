package cloud

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
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
	// imageTimeLayout is the created time docker image ls prints.
	imageTimeLayout = "2006-01-02 15:04:05 -0700 MST"
)

// The rules a Kept entry names, one per way an image survives a plan.
const (
	// ruleLabel keeps a tag a runner label names. A deploy workflow starts its
	// jobs on a runner label, and the label names the image those jobs run in,
	// so the image a deploy workflow runs in is kept with it.
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
	refuseNoLabels  = "no runner label list is configured; the plan is refused"
	refuseLabelRead = "the runner label list is unreadable; the plan is refused"
	refuseLabelLine = "a runner label names a fractals-nextjs image with no tag; the plan is refused"
	refuseUndated   = "a fractals-nextjs tag has no readable created time; the plan is refused"
)

// Plan lists the fractals-nextjs images the two keep rules leave for removal.
// It keeps the repository's newest keepNewest tags and every tag a runner label
// names, reading the label list through Run from cfg.LabelsFile so the set is
// never a constant; each kept image carries the rule that spared it. A read the
// plan cannot make, or a value it cannot read, refuses the plan with no
// actions.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget, Policy: p}
	labels, why := t.labelTags(ctx)
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
	plan.Kept, plan.Actions = decide(images, ranked, labels)
	return plan, nil
}

// labelTags returns the fractals-nextjs tags the runner label list names, or
// the fixed reason the list cannot be read.
func (t *Target) labelTags(ctx context.Context) (map[string]bool, string) {
	if t.cfg.LabelsFile == "" {
		return nil, refuseNoLabels
	}
	// "--" ends cat's options, so a path that starts with a dash is a file name
	// and not a flag.
	out, err := t.cfg.Run(ctx, nil, "cat", "--", t.cfg.LabelsFile)
	if err != nil {
		return nil, refuseLabelRead
	}
	return parseLabels(string(out))
}

// parseLabels returns the fractals-nextjs tags a runner label list names. Each
// line is one label, "<name>:docker://<repo>:<tag>", where the ref is the image
// a job started by that label runs in. A line with no image ref names no tag
// and is skipped; a ref of this repository with no tag refuses the plan, since
// the tag it stands for is unknown.
func parseLabels(text string) (map[string]bool, string) {
	tags := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, labelMarker)
		if i < 0 {
			continue
		}
		repo, tag := splitRef(strings.TrimSpace(line[i+len(labelMarker):]))
		if repoBase(repo) != fractalsRepo {
			continue
		}
		if tag == "" {
			return nil, refuseLabelLine
		}
		tags[tag] = true
	}
	return tags, ""
}

// splitRef splits an image ref into its repository and tag at the last colon,
// which leaves a registry's port alone; a ref with no tag returns an empty tag.
func splitRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
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
// docker drops every tag that points at it.
func decide(images []dockerimg.Image, ranked []aged, labels map[string]bool) ([]diskmaint.Kept, []diskmaint.Action) {
	keptImage := map[string]bool{}
	for _, im := range images {
		if !isFractalsTag(im) {
			keptImage[im.ID] = true
		}
	}
	for i, r := range ranked {
		if i < keepNewest || labels[r.im.Tag] {
			keptImage[r.im.ID] = true
		}
	}

	var kept []diskmaint.Kept
	var actions []diskmaint.Action
	planned := map[string]bool{}
	for i, r := range ranked {
		switch {
		case i < keepNewest:
			kept = append(kept, diskmaint.Kept{Ref: tagRef(r.im), Rule: ruleNewest})
		case labels[r.im.Tag]:
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
				Why:   fmt.Sprintf("older than the newest %d %s tags and no runner label names it", keepNewest, fractalsRepo),
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
