package imageprune

import (
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
