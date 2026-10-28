package registry

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The two stray repositories the design names, spelled here so a change to
// StrayRepos shows up as a failing test rather than a quietly skipped case.
const (
	strayMeasure = "gastown-polecat-measure"
	straySeed    = "seedtest/polecat"
)

func strayTag(repo, tag, digest string, created int64) TagInfo {
	return TagInfo{Repo: repo, Tag: tag, Digest: digest, Created: time.Unix(created, 0).UTC()}
}

// strayInput is a readable plan whose main repo holds only dev, plus the stray
// repositories a case lists.
func strayInput(tags map[string][]TagInfo) PlanInput {
	in := input([]TagInfo{mkTag(TagDev, dg("defaced"), 9)})
	for repo, repoTags := range tags {
		in.Repos[repo] = repoTags
	}
	return in
}

func TestPlanStrayRepoDeletedWhenUnreferenced(t *testing.T) {
	t.Parallel()
	in := strayInput(map[string][]TagInfo{
		strayMeasure: {
			strayTag(strayMeasure, "m1", dg("aaaa0001"), 1),
			strayTag(strayMeasure, "m2", dg("aaaa0002"), 2),
		},
		straySeed: {
			strayTag(straySeed, "p1", dg("bbbb0001"), 1),
		},
	})

	p := BuildPlan(in)

	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	want := []string{
		strayMeasure + "@" + dg("aaaa0001"),
		strayMeasure + "@" + dg("aaaa0002"),
		straySeed + "@" + dg("bbbb0001"),
	}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want every digest of both stray repos %v", got, want)
	}
	for _, a := range p.Actions {
		if a.Kind != "delete-manifest" {
			t.Errorf("action kind = %q, want delete-manifest", a.Kind)
		}
		if a.Why != "stray repo" {
			t.Errorf("action %s why = %q, want stray repo", a.Ref, a.Why)
		}
	}
}

func TestPlanStrayRepoKeptWhenNamedBySandboxRecord(t *testing.T) {
	t.Parallel()
	in := strayInput(map[string][]TagInfo{
		strayMeasure: {
			strayTag(strayMeasure, "m1", dg("aaaa0001"), 1),
		},
		straySeed: {
			strayTag(straySeed, "p1", dg("bbbb0001"), 1),
		},
	})
	in.InUseRepos = map[string]bool{straySeed: true}

	p := BuildPlan(in)

	want := []string{strayMeasure + "@" + dg("aaaa0001")}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want only the repo no sandbox record names %v", got, want)
	}
	if rule := keptRule(p, straySeed+":p1"); rule != "named by a sandbox record" {
		t.Fatalf("p1 kept by %q, want named by a sandbox record: %v", rule, p.Kept)
	}
}

func TestPlanStrayRepoDigestSharedWithKeptTagIsKept(t *testing.T) {
	t.Parallel()
	shared := dg("dead0001")
	in := input([]TagInfo{mkTag(TagDev, shared, 1)})
	in.Repos[straySeed] = []TagInfo{
		strayTag(straySeed, "p1", shared, 2),
		strayTag(straySeed, "p2", dg("bbbb0002"), 3),
	}

	p := BuildPlan(in)

	want := []string{straySeed + "@" + dg("bbbb0002")}
	if got := actionRefs(p); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want only the digest no kept tag shares %v", got, want)
	}
	if rule := keptRule(p, straySeed+"@"+shared); rule != "shares a kept digest" {
		t.Fatalf("stray digest %s kept by %q, want shares a kept digest: %v", shared, rule, p.Kept)
	}
}

func TestPlanUnknownRepoIsNeverTouched(t *testing.T) {
	t.Parallel()
	in := strayInput(map[string][]TagInfo{
		"other/repo": {strayTag("other/repo", "x1", dg("cccc0001"), 1)},
	})

	p := BuildPlan(in)

	if got := actionRefs(p); len(got) != 0 {
		t.Fatalf("actions = %v, want none: other/repo is neither the main nor a stray repo", got)
	}
	if rule := keptRule(p, "other/repo:x1"); rule != "" {
		t.Fatalf("other/repo:x1 is in Kept with rule %q, want the repo ignored entirely", rule)
	}
}

func TestPlanRefusesOnInvalidNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		repo   string
		tag    TagInfo
		offend string
	}{
		{"repo", "../up", strayTag("../up", "p1", dg("aaaa0001"), 1), "../up"},
		{"tag", straySeed, strayTag(straySeed, "a/b", dg("aaaa0001"), 1), "a/b"},
		{"digest", straySeed, strayTag(straySeed, "p1", "sha256:"+strings.Repeat("A", 64), 1), "sha256:AAAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := strayInput(map[string][]TagInfo{tc.repo: {tc.tag}})

			p := BuildPlan(in)

			assertRefused(t, p)
			if !strings.Contains(p.Refused, tc.offend) {
				t.Fatalf("refused by %q, want the offender %q named", p.Refused, tc.offend)
			}
		})
	}
}
