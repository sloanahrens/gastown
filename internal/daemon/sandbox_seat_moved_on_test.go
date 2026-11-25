package daemon

import "testing"

func TestSeatMovedOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
		landed string
		want   bool
	}{
		{
			name:   "the seat works another bead",
			branch: "polecat/morsov/gt-lbyw8q+mwvh30v3",
			landed: "gt-hx8f5i",
			want:   true,
		},
		{
			name:   "the seat works the landed bead itself",
			branch: "polecat/morsov/gt-lbyw8q+mwvh30v3",
			landed: "gt-lbyw8q",
			want:   false,
		},
		{
			name:   "the seat was re-slung after a crew landing",
			branch: "polecat/capable/gt-2n9wff+mwvh9myv",
			landed: "gt-0lajvh",
			want:   true,
		},
		{
			name:   "the legacy suffix separator still names its bead",
			branch: "polecat/morsov/gt-lbyw8q@mwvh30v3",
			landed: "gt-hx8f5i",
			want:   true,
		},
		{
			name:   "an issue-named branch without a suffix still names its bead",
			branch: "polecat/morsov/gt-lbyw8q",
			landed: "gt-hx8f5i",
			want:   true,
		},
		{
			name:   "a polecat branch naming no bead",
			branch: "polecat/morsov-mwvh30v3",
			landed: "gt-hx8f5i",
			want:   false,
		},
		{
			name:   "a crew branch names no bead",
			branch: "crew/gt-dfw5h2",
			landed: "gt-hx8f5i",
			want:   false,
		},
		{
			name:   "an empty branch names no bead",
			branch: "",
			landed: "gt-hx8f5i",
			want:   false,
		},
		{
			name:   "an empty landed bead names nothing the seat moved on from",
			branch: "polecat/morsov/gt-lbyw8q+mwvh30v3",
			landed: "",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := seatMovedOn(tt.branch, tt.landed); got != tt.want {
				t.Errorf("seatMovedOn(%q, %q) = %v, want %v", tt.branch, tt.landed, got, tt.want)
			}
		})
	}
}
