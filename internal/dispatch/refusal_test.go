package dispatch

import "testing"

func TestSlingRefusalLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		text   string
		want   string
		wantOK bool
	}{
		{
			name:   "capacity marker, wrapped by cobra",
			text:   "Error: sling refused: pool: full (2/2)\n",
			want:   "sling refused: pool: full (2/2)",
			wantOK: true,
		},
		{
			name:   "re-sling guard",
			text:   "Error: refusing to re-sling gt-x: its branch survives\n",
			want:   "refusing to re-sling gt-x: its branch survives",
			wantOK: true,
		},
		{
			name:   "parked rig, no marker",
			text:   "[sling] step admission took 1s\nError: cannot sling to parked rig \"x\"\ngt rig unpark x\n",
			want:   `cannot sling to parked rig "x"`,
			wantOK: true,
		},
		{
			name:   "docked rig",
			text:   "cannot sling to docked rig \"x\"\ngt rig undock x",
			want:   `cannot sling to docked rig "x"`,
			wantOK: true,
		},
		{
			name:   "e-stopped rig",
			text:   "Error: cannot sling to rig \"x\": E-stop active\nClear it with: gt thaw\n",
			want:   `cannot sling to rig "x": E-stop active`,
			wantOK: true,
		},
		{
			name:   "e-stopped rig, wrapped by the spawn path",
			text:   "Error: spawning polecat: cannot sling to rig \"x\": E-stop active\n",
			want:   `cannot sling to rig "x": E-stop active`,
			wantOK: true,
		},
		{
			name:   "parked rig, wrapped by the dry-run preview",
			text:   "gt sling --dry-run: exit status 1: cannot sling to parked rig \"x\"",
			want:   `cannot sling to parked rig "x"`,
			wantOK: true,
		},
		{name: "rig refusal quoted in a bead title", text: "bead gt-x: fix so cannot sling to docked rig \"x\" stops\n"},
		{name: "rig refusal quoted mid-sentence in other output", text: "note: the daemon said cannot sling to rig \"x\" last week\n"},
		{name: "another cannot-sling failure", text: "Error: cannot sling to bead gt-x: not found\n"},
		{name: "genuine failure", text: "Error: bead gt-x not found\n"},
		{name: "empty", text: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SlingRefusalLine(tt.text)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("SlingRefusalLine = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSlingRefusalReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   string
		wantOK bool
	}{
		{
			name:   "pool full, wrapped by cobra and the spawn path",
			stderr: "Error: spawning polecat: sling refused: pool: full (2/2) -> no seat for deepseek-flash; raise polecat_pool.max_seats to spawn\n",
			want:   "sling refused: pool: full (2/2) -> no seat for deepseek-flash; raise polecat_pool.max_seats to spawn",
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
