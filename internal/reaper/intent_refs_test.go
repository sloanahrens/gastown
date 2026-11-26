package reaper

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAgentBeadRetireIntentRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string]string // path under .runtime/agents → record JSON
		want    map[string]struct{}
		wantErr bool
	}{
		{
			name: "records at both levels",
			files: map[string]string{
				"deacon.json":                `{"work_bead":"gt-town"}`,
				"gastown/polecat.flint.json": `{"work_bead":"gt-rig"}`,
			},
			want: map[string]struct{}{"gt-rig": {}, "gt-town": {}},
		},
		{
			name:  "empty work bead names nothing",
			files: map[string]string{"gastown/witness.json": `{"work_bead":""}`},
			want:  map[string]struct{}{},
		},
		{name: "absent agents directory", want: map[string]struct{}{}},
		{name: "agents path is a file", files: map[string]string{"": "x"}, wantErr: true}, // "" is the agents directory itself
		{
			name: "malformed record is an error, not a partial set",
			files: map[string]string{
				"deacon.json":                `{"work_bead":"gt-town"}`,
				"gastown/polecat.flint.json": `{`,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			for rel, body := range tt.files {
				path := filepath.Join(town, ".runtime", "agents", filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := IntentWorkBeads(town)
			if tt.wantErr {
				if err == nil || got != nil {
					t.Fatalf("IntentWorkBeads() = %v, %v; want an error and no set", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("IntentWorkBeads() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}
