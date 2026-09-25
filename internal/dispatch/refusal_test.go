package dispatch

import "testing"

func TestSlingRefusalReason(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   string
		wantOK bool
	}{
		{
			name:   "pool full, wrapped by cobra and the spawn path",
			stderr: "Error: spawning polecat: sling refused: every local seat is taken (2/2); raise polecat_pool.max_local/max_overflow to spawn\n",
			want:   "sling refused: every local seat is taken (2/2); raise polecat_pool.max_local/max_overflow to spawn",
			wantOK: true,
		},
		{
			name:   "refusal after other output",
			stderr: "resolving rig...\nError: sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework\n",
			want:   "sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework",
			wantOK: true,
		},
		{
			name:   "genuine failure",
			stderr: "Error: bead gt-x not found\n",
			wantOK: false,
		},
		{
			name:   "empty",
			stderr: "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SlingRefusalReason(tt.stderr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}
