package drift

import (
	"maps"
	"testing"
)

func TestEditKindsNamesTheIdentifiersTheChangedLinesUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		changed string
		want    []string
	}{
		{
			name:    "a flag is named as a flag and not as a config key",
			changed: "+Pass `--recent` to widen the window.\n",
			want:    []string{KindFlag},
		},
		{
			name:    "a gt command phrase is named as a command",
			changed: "-Run gt jev landing to see the log.\n",
			want:    []string{KindCommand},
		},
		{
			name:    "a snake_case name is named as a config key",
			changed: "+Set max_usd to 3.\n",
			want:    []string{KindConfigKey},
		},
		{
			name:    "a flag and a config key in one edit name both",
			changed: "+Set max_usd and pass --recent.\n",
			want:    []string{KindConfigKey, KindFlag},
		},
		{
			name:    "an edit that names none of them is a behavior change",
			changed: "+The battery now asks about each section.\n",
			want:    []string{KindBehavior},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EditKinds(tc.changed)
			want := map[string]bool{}
			for _, k := range tc.want {
				want[k] = true
			}
			if !maps.Equal(got, want) {
				t.Errorf("EditKinds(%q) = %v, want %v", tc.changed, got, want)
			}
		})
	}
}
