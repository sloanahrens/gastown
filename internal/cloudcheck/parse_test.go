package cloudcheck

import (
	"reflect"
	"strings"
	"testing"
)

func TestAppsFileParsing(t *testing.T) {
	t.Parallel()
	noApps := Problem{Level: LevelFail, Line: 0, Reason: "names no apps"}
	tests := []struct {
		name     string
		file     string
		want     []App
		problems []Problem
	}{
		{
			name: "one check",
			file: "gt=https://gt.example/health\n",
			want: []App{{Name: "gt", URL: "https://gt.example/health"}},
		},
		{
			name: "a section names the checks after it",
			file: "app=gastown\nweb=https://a.example/\napi=https://b.example/\n",
			want: []App{
				{Section: "gastown", Name: "web", URL: "https://a.example/"},
				{Section: "gastown", Name: "api", URL: "https://b.example/"},
			},
		},
		{
			name: "comments, blank lines and spaces are not checks",
			file: "# a comment\n\n   # an indented comment\n\tgt = https://gt.example \n",
			want: []App{{Name: "gt", URL: "https://gt.example"}},
		},
		{
			name: "only the first = splits the line",
			file: "gt=https://gt.example/?a=b\n",
			want: []App{{Name: "gt", URL: "https://gt.example/?a=b"}},
		},
		{
			name:     "no = is a malformed line",
			file:     "gt https://gt.example\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "expected name=url"}, noApps},
		},
		{
			name:     "an empty check name",
			file:     "=https://gt.example\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "the check name is not a plain name"}, noApps},
		},
		{
			name:     "an empty URL",
			file:     "gt=\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "the URL is not an absolute http or https URL"}, noApps},
		},
		{
			name:     "a non-http scheme",
			file:     "gt=file:///etc/passwd\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "the URL is not an absolute http or https URL"}, noApps},
		},
		{
			name:     "a URL with no host",
			file:     "gt=https:///path\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "the URL is not an absolute http or https URL"}, noApps},
		},
		{
			name:     "a name that is not plain",
			file:     "../escape=https://gt.example/\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "the check name is not a plain name"}, noApps},
		},
		{
			name:     "an empty app=",
			file:     "app=\n",
			want:     []App{},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "has an empty app= field"}, noApps},
		},
		{
			name:     "an app name that is not plain",
			file:     "app=../escape\ngt=https://gt.example/\n",
			want:     []App{{Name: "gt", URL: "https://gt.example/"}},
			problems: []Problem{{Level: LevelWarn, Line: 1, Reason: "app= names no plain app name"}},
		},
		{
			name:     "a file that names no apps",
			file:     "# nothing yet\n\n",
			want:     []App{},
			problems: []Problem{noApps},
		},
		{
			name:     "a malformed line does not drop the others",
			file:     "gt=https://gt.example/\nbroken line\n",
			want:     []App{{Name: "gt", URL: "https://gt.example/"}},
			problems: []Problem{{Level: LevelWarn, Line: 2, Reason: "expected name=url"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			apps, problems, err := ParseApps(strings.NewReader(tt.file))
			if err != nil {
				t.Fatalf("ParseApps: %v", err)
			}
			if !reflect.DeepEqual(apps, tt.want) {
				t.Errorf("apps = %+v, want %+v", apps, tt.want)
			}
			if len(problems) != len(tt.problems) || (len(problems) > 0 && !reflect.DeepEqual(problems, tt.problems)) {
				t.Errorf("problems = %+v, want %+v", problems, tt.problems)
			}
		})
	}
}

func TestAppsFileOverlongLineRefuses(t *testing.T) {
	t.Parallel()
	overlong := "gt=" + strings.Repeat("a", maxAppsLineBytes)
	if _, _, err := ParseApps(strings.NewReader(overlong)); err == nil {
		t.Fatal("a line over the cap was read instead of refused")
	}
}
