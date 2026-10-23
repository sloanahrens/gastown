package git

import "testing"

func TestSeatIdentityFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		seat          string
		worktreeEmail string
		want          SeatIdentity
	}{
		{
			name:          "polecat takes its bare name and the worktree email",
			seat:          "gastown/polecats/coma",
			worktreeEmail: "seat@test.com",
			want:          SeatIdentity{Name: "coma", Email: "seat@test.com"},
		},
		{
			name:          "crew takes its bare name",
			seat:          "gastown/crew/jack",
			worktreeEmail: "crew@test.com",
			want:          SeatIdentity{Name: "jack", Email: "crew@test.com"},
		},
		{
			name:          "trailing slash names the role",
			seat:          "deacon/",
			worktreeEmail: "deacon@test.com",
			want:          SeatIdentity{Name: "deacon", Email: "deacon@test.com"},
		},
		{
			name:          "no worktree email leaves the email empty",
			seat:          "gastown/polecats/coma",
			worktreeEmail: "",
			want:          SeatIdentity{Name: "coma"},
		},
		{
			name:          "surrounding space is not part of the identity",
			seat:          " gastown/polecats/coma ",
			worktreeEmail: " seat@test.com ",
			want:          SeatIdentity{Name: "coma", Email: "seat@test.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SeatIdentityFor(tt.seat, tt.worktreeEmail); got != tt.want {
				t.Errorf("SeatIdentityFor(%q, %q) = %+v, want %+v", tt.seat, tt.worktreeEmail, got, tt.want)
			}
		})
	}
}
