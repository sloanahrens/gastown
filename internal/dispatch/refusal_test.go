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

func TestReslingRefusalReason(t *testing.T) {
	// The marker is a cross-process contract: gt sling writes it, the daemon's
	// convoy feeder matches it.
	if ReslingRefusalMarker != "refusing to re-sling" {
		t.Fatalf("ReslingRefusalMarker = %q", ReslingRefusalMarker)
	}
	tests := []struct {
		name   string
		stderr string
		want   string
		wantOK bool
	}{
		{
			name:   "surviving work, wrapped by cobra, multi-line",
			stderr: "Error: refusing to re-sling gt-a: previous holder gt/polecats/p has no active session, but its branch still carries work that is not on main:\n  polecat/p/gt-a+x\n",
			want:   "refusing to re-sling gt-a: previous holder gt/polecats/p has no active session, but its branch still carries work that is not on main:",
			wantOK: true,
		},
		{
			name:   "capacity refusal is a different marker",
			stderr: "Error: sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework",
		},
		{name: "other failure", stderr: "Error: spawn failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ReslingRefusalReason(tt.stderr)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ReslingRefusalReason() = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
