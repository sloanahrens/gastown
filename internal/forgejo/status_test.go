package forgejo

import "testing"

// TestCommitStatusCreatorPredicates pins the two questions the landing merge
// asks of a status's creator: whether a user posted it at all, and whether a
// named account did. An empty login matches nobody, so a status with no
// creator never passes the landing bot's check (gt-fn9e6.7).
func TestCommitStatusCreatorPredicates(t *testing.T) {
	t.Parallel()
	byUser := &CommitStatus{Context: "ci / gate (push)", Creator: &User{Login: "mallory"}}
	byCI := &CommitStatus{Context: "ci / gate (push)"}

	tests := []struct {
		name   string
		status *CommitStatus
		user   bool
		// poster is the login PostedBy must accept, "" when nobody posted it.
		poster string
	}{
		{name: "a user's status", status: byUser, user: true, poster: "mallory"},
		{name: "a workflow run's status", status: byCI, user: false},
		{name: "no status", status: nil, user: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.status.HasUserCreator(); got != tt.user {
				t.Errorf("HasUserCreator() = %v, want %v", got, tt.user)
			}
			// The guard the landing bot's check leans on: an unset bot login
			// must match no status, so a misconfiguration fails closed rather
			// than accepting any om / review status. Every status that is not
			// this account's answers false for it too.
			for _, login := range []string{"", "someone-else"} {
				if tt.status.PostedBy(login) {
					t.Errorf("PostedBy(%q) = true; want false", login)
				}
			}
			if tt.poster != "" {
				if !tt.status.PostedBy(tt.poster) {
					t.Errorf("PostedBy(%q) = false on the status %s posted", tt.poster, tt.poster)
				}
				if tt.status.CreatorLogin() != tt.poster {
					t.Errorf("CreatorLogin() = %q, want %q", tt.status.CreatorLogin(), tt.poster)
				}
			}
		})
	}
}

// TestStatusesForReturnsEveryMatch: the creator check must see a context's
// second status, or a status a user posted could hide behind the one a
// workflow posted (gt-fn9e6.7).
func TestStatusesForReturnsEveryMatch(t *testing.T) {
	t.Parallel()
	combined := &CombinedStatus{Statuses: []CommitStatus{
		{Context: "ci / gate (push)"},
		{Context: "om / review", Creator: &User{Login: "gt-landing"}},
		{Context: "ci / gate (push)", Creator: &User{Login: "mallory"}},
	}}
	gate := combined.StatusesFor("ci / gate (push)")
	if len(gate) != 2 {
		t.Fatalf("StatusesFor returned %d statuses, want both", len(gate))
	}
	if gate[0].HasUserCreator() || !gate[1].HasUserCreator() {
		t.Fatalf("StatusesFor = %+v; want the workflow's status and the user's, in order", gate)
	}
	if got := combined.StatusesFor("om / review"); len(got) != 1 || got[0].CreatorLogin() != "gt-landing" {
		t.Fatalf("StatusesFor(om / review) = %+v; want the landing bot's status", got)
	}
	if got := combined.StatusesFor("absent"); got != nil {
		t.Fatalf("StatusesFor(absent) = %+v, want nil", got)
	}
}
