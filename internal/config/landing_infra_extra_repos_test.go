package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestLandingInfraExtraRepos: extra_repos is a declared key, a repeat is dropped
// in place, and a malformed entry refuses the list with an error naming it.
func TestLandingInfraExtraRepos(t *testing.T) {
	t.Parallel()

	t.Run("decodes through the strict decoder", func(t *testing.T) {
		t.Parallel()
		var cfg LandingInfraConfig
		dec := json.NewDecoder(strings.NewReader(`{"extra_repos":["acme/app"]}`))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil || !reflect.DeepEqual(cfg.ExtraRepos, []string{"acme/app"}) {
			t.Fatalf("decode = %q, %v; want extra_repos [acme/app] under a strict decoder", cfg.ExtraRepos, err)
		}
	})

	app, web := ExtraRepo{Owner: "acme", Repo: "app"}, ExtraRepo{Owner: "acme", Repo: "web"}
	valid := []struct {
		name    string
		entries []string
		want    []ExtraRepo
	}{
		{"valid entries keep their order", []string{"acme/app", "acme/web"}, []ExtraRepo{app, web}},
		{"duplicate dropped in place", []string{"acme/app", "acme/web", "acme/app"}, []ExtraRepo{app, web}},
		{"no entries gives none", nil, nil},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&LandingInfraConfig{ExtraRepos: tt.entries}).ExtraRepoRefs()
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtraRepoRefs() = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}

	t.Run("nil config gives none", func(t *testing.T) {
		t.Parallel()
		if got, err := (*LandingInfraConfig)(nil).ExtraRepoRefs(); err != nil || got != nil {
			t.Errorf("ExtraRepoRefs() on nil config = %v, %v; want none, nil", got, err)
		}
	})

	for _, bad := range []string{
		"acme", "acme/app/extra", "/app", "acme/", "ac me/app", "acme/ap$p", "acme/..", "./app",
	} {
		t.Run("refuses "+bad, func(t *testing.T) {
			t.Parallel()
			got, err := (&LandingInfraConfig{ExtraRepos: []string{"acme/good", bad}}).ExtraRepoRefs()
			if err == nil || got != nil || !strings.Contains(err.Error(), bad) {
				t.Errorf("ExtraRepoRefs() = %v, %v; want no refs and an error naming %q", got, err, bad)
			}
		})
	}
}
