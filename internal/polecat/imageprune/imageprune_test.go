package imageprune

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func img(id string, age int, tags, digests []string) Image {
	return Image{ID: id, Tags: tags, Digests: digests, Created: t0.Add(-time.Duration(age) * time.Hour)}
}

func ids(list []Image) []string {
	var out []string
	for _, im := range list {
		out = append(out, im.ID)
	}
	return out
}

func TestPlanKeepsTheNewestTagsAndRemovesTheRest(t *testing.T) {
	t.Parallel()
	images := []Image{
		img("new", 1, []string{"localhost:5001/gastown-polecat:aaa"}, nil),
		img("mid", 2, []string{"localhost:5001/gastown-polecat:bbb"}, nil),
		img("old", 3, []string{"localhost:5001/gastown-polecat:ccc"}, nil),
		img("oldest", 4, []string{"localhost:5001/gastown-polecat:ddd"}, nil),
	}
	got := ids(Plan(images, map[string]bool{}, 2))
	if want := []string{"old", "oldest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Plan keep 2 removed %v, want %v", got, want)
	}
}

func TestPlanNeverRemovesAnImageAContainerUses(t *testing.T) {
	t.Parallel()
	images := []Image{
		img("used-by-id", 9, []string{"localhost:5001/gastown-polecat:aaa"}, nil),
		img("used-by-digest", 10, nil, []string{"localhost:5001/gastown-polecat@sha256:beef"}),
		img("unused", 11, []string{"localhost:5001/gastown-polecat:bbb"}, nil),
	}
	inUse := map[string]bool{"used-by-id": true, "localhost:5001/gastown-polecat@sha256:beef": true}
	got := ids(Plan(images, inUse, 0))
	if want := []string{"unused"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Plan removed %v, want only %v", got, want)
	}
}

func TestPlanKeepsTheDevTag(t *testing.T) {
	t.Parallel()
	images := []Image{
		img("dev", 50, []string{"localhost:5001/gastown-polecat:dev"}, nil),
		img("old", 60, []string{"localhost:5001/gastown-polecat:aaa"}, nil),
	}
	got := ids(Plan(images, map[string]bool{}, 0))
	if want := []string{"old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Plan removed %v, want only %v (dev kept)", got, want)
	}
}

func TestPlanRemovesDigestOnlyPolecatImages(t *testing.T) {
	t.Parallel()
	images := []Image{
		img("untagged", 30, nil, []string{"localhost:5001/gastown-polecat@sha256:f627"}),
	}
	got := ids(Plan(images, map[string]bool{}, 0))
	if want := []string{"untagged"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Plan removed %v, want %v (an untagged polecat image is a candidate)", got, want)
	}
}

func TestPlanReposKeepsTheNewestOfEachRepository(t *testing.T) {
	t.Parallel()
	repos := []string{"gastown-polecat", "gastown-daemon", "gastown-bd"}
	var images []Image
	var want []string
	for _, repo := range repos {
		for age := 1; age <= 8; age++ {
			id := fmt.Sprintf("%s-%d", repo, age)
			images = append(images, img(id, age, []string{"localhost:5001/" + repo + ":" + id}, nil))
			if age > 3 {
				want = append(want, id)
			}
		}
	}
	if got := ids(PlanRepos(images, map[string]bool{}, 3, repos)); !reflect.DeepEqual(got, want) {
		t.Errorf("PlanRepos keep 3 removed %v, want the 5 oldest of each repository %v", got, want)
	}
}

func TestPlanReposNeverPlansAnUnknownAgeOrAMixedImage(t *testing.T) {
	t.Parallel()
	images := []Image{
		{ID: "no-age", Tags: []string{"localhost:5001/gastown-bd:aaa"}, Unknown: true},
		img("mixed", 100, []string{"localhost:5001/gastown-bd:bbb", "localhost:5001/gastown-daemon:bbb"}, nil),
	}
	if got := PlanRepos(images, map[string]bool{}, 0, []string{"gastown-bd", "gastown-daemon"}); len(got) != 0 {
		t.Errorf("PlanRepos removed %v, want nothing: an unknown age or a mixed-repository image is never planned", ids(got))
	}
}

func TestPlanLeavesOtherImagesAlone(t *testing.T) {
	t.Parallel()
	images := []Image{
		img("kind", 40, []string{"docker.io/kindest/kindnetd:v1"}, nil),
		img("mixed", 41, []string{"localhost:5001/gastown-polecat:aaa", "node:20"}, nil),
		img("no-refs", 42, nil, nil),
		img("suffix", 43, []string{"localhost:5001/other-gastown-polecat:aaa"}, nil),
	}
	if got := Plan(images, map[string]bool{}, 0); len(got) != 0 {
		t.Errorf("Plan removed %v, want nothing: other repos, mixed or reference-less images are not candidates", ids(got))
	}
}

// TestPlanReposSparesTheNeverCandidates pairs each image PlanRepos must spare
// with one old unused control that must be returned, so an empty plan cannot pass.
func TestPlanReposSparesTheNeverCandidates(t *testing.T) {
	t.Parallel()
	const digest = "localhost:5001/gastown-polecat@sha256:beef"
	images := []Image{
		img("used-by-id", 10, []string{"localhost:5001/gastown-polecat:aaa"}, nil),
		img("used-by-digest", 11, nil, []string{digest}),
		{ID: "no-age", Tags: []string{"localhost:5001/gastown-polecat:bbb"}, Unknown: true},
		img("mixed", 12, []string{"localhost:5001/gastown-polecat:ccc", "localhost:5001/gastown-daemon:ccc"}, nil),
		img("dev", 13, []string{"localhost:5001/gastown-polecat:dev"}, nil),
		img("old", 100, []string{"localhost:5001/gastown-polecat:ddd"}, nil),
	}
	inUse := map[string]bool{"used-by-id": true, digest: true}
	got := ids(PlanRepos(images, inUse, 0, []string{"gastown-polecat", "gastown-daemon"}))
	if want := []string{"old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PlanRepos removed %v, want only %v: an in-use, unknown-age, mixed-repository or dev image is never returned", got, want)
	}
}

// TestPlanReposSparesTheUsedWithoutCounting checks that an image in use is
// skipped without taking one of the keep slots, so the next unused image is kept.
func TestPlanReposSparesTheUsedWithoutCounting(t *testing.T) {
	t.Parallel()
	images := []Image{
		img("unused-1", 1, []string{"localhost:5001/gastown-polecat:a1"}, nil),
		img("unused-2", 2, []string{"localhost:5001/gastown-polecat:a2"}, nil),
		img("used", 3, []string{"localhost:5001/gastown-polecat:a3"}, nil),
		img("unused-3", 4, []string{"localhost:5001/gastown-polecat:a4"}, nil),
		img("unused-4", 5, []string{"localhost:5001/gastown-polecat:a5"}, nil),
	}
	inUse := map[string]bool{"used": true}
	got := ids(PlanRepos(images, inUse, 3, []string{Repo}))
	if want := []string{"unused-4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PlanRepos keep 3 removed %v, want only %v: the in-use image must not count toward keep", got, want)
	}
}
