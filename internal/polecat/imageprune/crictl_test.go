package imageprune

import (
	"reflect"
	"testing"
)

const imagesJSON = `{"images":[{"id":"sha256:59c3","repoTags":[],"repoDigests":["localhost:5001/gastown-polecat@sha256:f627"],"size":"1999754377"},{"id":"sha256:aa11","repoTags":["localhost:5001/gastown-polecat:dev"],"repoDigests":["localhost:5001/gastown-polecat@sha256:beef"]}]}`

const inspectJSON = `{"info":{"imageSpec":{"created":"2026-10-25T16:58:26.123456789Z"}}}`

const psJSON = `{"containers":[{"image":{"image":"localhost:5001/gastown-polecat@sha256:beef"},"imageRef":"sha256:aa11"}]}`

func TestParseImagesReadsIDsTagsAndDigests(t *testing.T) {
	t.Parallel()
	got, err := ParseImages([]byte(imagesJSON))
	if err != nil {
		t.Fatalf("ParseImages: %v", err)
	}
	want := []Image{
		{ID: "sha256:59c3", Tags: []string{}, Digests: []string{"localhost:5001/gastown-polecat@sha256:f627"}},
		{ID: "sha256:aa11", Tags: []string{"localhost:5001/gastown-polecat:dev"}, Digests: []string{"localhost:5001/gastown-polecat@sha256:beef"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseImages = %+v, want %+v", got, want)
	}
}

func TestParseImagesRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	if _, err := ParseImages([]byte(`{"images":`)); err == nil {
		t.Error("ParseImages on truncated JSON = nil error, want one")
	}
}

func TestParseCreatedReadsTheImageConfigTime(t *testing.T) {
	t.Parallel()
	got, err := ParseCreated([]byte(inspectJSON))
	if err != nil {
		t.Fatalf("ParseCreated: %v", err)
	}
	if got.Year() != 2026 || got.Hour() != 16 {
		t.Errorf("ParseCreated = %v, want 2026-10-25 16:58", got)
	}
}

func TestParseCreatedRejectsAMissingTime(t *testing.T) {
	t.Parallel()
	if _, err := ParseCreated([]byte(`{"info":{"imageSpec":{}}}`)); err == nil {
		t.Error("ParseCreated with no created time = nil error, want one")
	}
}

func TestInUseCollectsImageRefsAndIDs(t *testing.T) {
	t.Parallel()
	used, err := InUse([]byte(psJSON))
	if err != nil {
		t.Fatalf("InUse: %v", err)
	}
	for _, want := range []string{"localhost:5001/gastown-polecat@sha256:beef", "sha256:aa11"} {
		if !used[want] {
			t.Errorf("InUse missed %q: %v", want, used)
		}
	}
}
