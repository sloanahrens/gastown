package registry

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// dg builds a manifest digest from a short hex label, so a fixture line names
// one digest and the assertion can name the same one.
func dg(label string) string {
	return "sha256:" + label + strings.Repeat("0", 64-len(label))
}

// commit builds the full sha gt version reports for a tag.
func commit(tag string) string {
	return tag + strings.Repeat("f", 40-len(tag))
}

func mkTag(tag, digest string, created int64) TagInfo {
	return TagInfo{Repo: RepoMain, Tag: tag, Digest: digest, Created: time.Unix(created, 0).UTC()}
}

// input is a readable main repo with an installed commit that matches no tag in
// it, so a test opts into the installed-commit keep by setting InstalledCommit.
func input(tags []TagInfo) PlanInput {
	return PlanInput{
		Repos:           map[string][]TagInfo{RepoMain: tags},
		InstalledCommit: commit("c000099"),
		InUseDigests:    map[string]bool{},
		InUseRepos:      map[string]bool{},
	}
}

func actionRefs(p diskmaint.Plan) []string {
	refs := make([]string, len(p.Actions))
	for i, a := range p.Actions {
		refs[i] = a.Ref
	}
	return refs
}

func keptRule(p diskmaint.Plan, ref string) string {
	for _, k := range p.Kept {
		if k.Ref == ref {
			return k.Rule
		}
	}
	return ""
}

func assertRefused(t *testing.T, p diskmaint.Plan) {
	t.Helper()
	if p.Refused == "" {
		t.Fatalf("plan was not refused: %+v", p)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("refused plan carries %d actions: %v", len(p.Actions), actionRefs(p))
	}
}

// candidateTag is the 7-hex-character tag of the nth per-commit image.
func candidateTag(n int) string {
	return fmt.Sprintf("c%06d", n)
}

func digestLabel(n int) string {
	return fmt.Sprintf("dead%04x", n)
}

func mainRef(digest string) string {
	return RepoMain + "@" + digest
}

func TestPlanDeletesOldCandidatesBeyondKeep(t *testing.T) {
	var tags []TagInfo
	for i := 1; i <= 8; i++ {
		tags = append(tags, mkTag(candidateTag(i), dg(digestLabel(i)), int64(i)))
	}
	in := input(append(tags, mkTag(TagDev, dg("defaced"), 100)))
	in.Keep = 5

	p := BuildPlan(in)

	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	want := []string{
		mainRef(dg(digestLabel(1))),
		mainRef(dg(digestLabel(2))),
		mainRef(dg(digestLabel(3))),
	}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want the 3 oldest %v", got, want)
	}
	for _, a := range p.Actions {
		if a.Kind != "delete-manifest" {
			t.Errorf("action kind = %q, want delete-manifest", a.Kind)
		}
		if a.Why == "" {
			t.Errorf("action %s has no why", a.Ref)
		}
	}
	if kept := len(p.Kept); kept != 6 {
		t.Errorf("kept %d refs, want 5 newest candidates plus dev: %v", kept, p.Kept)
	}
}

func TestPlanKeepsTagSharingDevDigest(t *testing.T) {
	shared := dg("defaced")
	in := input([]TagInfo{
		mkTag(TagDev, shared, 1),
		mkTag("aaa1111", shared, 2),
		mkTag("c000001", dg("dead0001"), 3),
		mkTag("c000002", dg("dead0002"), 4),
	})
	in.Keep = 1

	p := BuildPlan(in)

	want := []string{mainRef(dg("dead0001"))}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want only the unshared old digest %v", got, want)
	}
	if rule := keptRule(p, RepoMain+":aaa1111"); rule != "shares a kept digest" {
		t.Fatalf("aaa1111 kept by %q, want shares a kept digest", rule)
	}
}

func TestPlanKeepsInstalledCommitTag(t *testing.T) {
	in := input([]TagInfo{
		mkTag("c000001", dg("dead0001"), 1),
		mkTag("c000002", dg("dead0002"), 2),
		mkTag("c000003", dg("dead0003"), 3),
		mkTag(TagDev, dg("defaced"), 4),
	})
	in.Keep = 1
	in.InstalledCommit = commit("c000001")

	p := BuildPlan(in)

	want := []string{mainRef(dg("dead0002"))}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v: the oldest tag is the installed commit", got, want)
	}
	if rule := keptRule(p, RepoMain+":c000001"); rule != "installed commit" {
		t.Fatalf("c000001 kept by %q, want installed commit", rule)
	}
}

func TestPlanKeepsInUseDigest(t *testing.T) {
	in := input([]TagInfo{
		mkTag("c000001", dg("dead0001"), 1),
		mkTag("c000002", dg("dead0002"), 2),
		mkTag(TagDev, dg("defaced"), 3),
	})
	in.Keep = 1
	in.InUseDigests = map[string]bool{dg("dead0001"): true}

	p := BuildPlan(in)

	if got := actionRefs(p); len(got) != 0 {
		t.Fatalf("actions = %v, want none: the oldest digest is in use", got)
	}
	if rule := keptRule(p, RepoMain+":c000001"); rule != "in use" {
		t.Fatalf("c000001 kept by %q, want in use", rule)
	}
}

func TestPlanNeverTouchesNonHexTags(t *testing.T) {
	for _, name := range []string{"latest", "build-3", "v1.2.3", "C000001", "c00001", "c000000000001"} {
		t.Run(name, func(t *testing.T) {
			in := input([]TagInfo{
				mkTag(TagDev, dg("defaced"), 9),
				mkTag(name, dg("beef"), 1),
			})
			in.Keep = 0

			p := BuildPlan(in)

			if got := actionRefs(p); len(got) != 0 {
				t.Fatalf("actions = %v, want none: %q is not a commit-hash tag", got, name)
			}
			if rule := keptRule(p, RepoMain+":"+name); rule != "named tag" {
				t.Fatalf("%s kept by %q, want named tag", name, rule)
			}
		})
	}
}

func TestPlanTwoTagsOneDigestCountOnce(t *testing.T) {
	shared := dg("dead0001")
	in := input([]TagInfo{
		mkTag("c000001", shared, 1),
		mkTag("c000002", shared, 2),
		mkTag("c000003", dg("dead0003"), 3),
		mkTag(TagDev, dg("defaced"), 4),
	})
	in.Keep = 1

	p := BuildPlan(in)

	want := []string{mainRef(shared)}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want one action for the shared digest %v", got, want)
	}
}

func TestPlanKeepZeroStillKeepsDevAndInstalled(t *testing.T) {
	in := input([]TagInfo{
		mkTag("c000001", dg("dead0001"), 1),
		mkTag("c000002", dg("dead0002"), 2),
		mkTag(TagDev, dg("defaced"), 3),
	})
	in.Keep = 0
	in.InstalledCommit = commit("c000002")

	p := BuildPlan(in)

	want := []string{mainRef(dg("dead0001"))}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	if rule := keptRule(p, RepoMain+":"+TagDev); rule == "" {
		t.Fatalf("dev is not kept: %v", p.Kept)
	}
	if rule := keptRule(p, RepoMain+":c000002"); rule != "installed commit" {
		t.Fatalf("c000002 kept by %q, want installed commit", rule)
	}
}

func TestPlanRefusesOnAnyReadError(t *testing.T) {
	in := input([]TagInfo{mkTag(TagDev, dg("defaced"), 1)})
	in.Errs = []string{"tags gastown-polecat: connection refused"}

	p := BuildPlan(in)

	assertRefused(t, p)
	if !strings.Contains(p.Refused, "connection refused") {
		t.Fatalf("refused by %q, want the read error named", p.Refused)
	}
}

func TestPlanRefusesWithoutInstalledCommit(t *testing.T) {
	in := input([]TagInfo{mkTag(TagDev, dg("defaced"), 1)})
	in.InstalledCommit = ""

	assertRefused(t, BuildPlan(in))
}

func TestPlanRefusesOnEmptyMainRepo(t *testing.T) {
	assertRefused(t, BuildPlan(input(nil)))
}

func TestPlanRefusesWhenDevTagAbsent(t *testing.T) {
	in := input([]TagInfo{mkTag("c000001", dg("dead0001"), 1)})

	assertRefused(t, BuildPlan(in))
}
