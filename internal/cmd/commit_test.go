package cmd

import "testing"

func TestIdentityToEmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity string
		domain   string
		want     string
	}{
		{
			name:     "crew member",
			identity: "gastown/crew/jack",
			domain:   "gastown.local",
			want:     "gastown.crew.jack@gastown.local",
		},
		{
			name:     "polecat",
			identity: "gastown/polecats/max",
			domain:   "gastown.local",
			want:     "gastown.polecats.max@gastown.local",
		},
		{
			name:     "witness",
			identity: "gastown/witness",
			domain:   "gastown.local",
			want:     "gastown.witness@gastown.local",
		},
		{
			name:     "refinery",
			identity: "gastown/refinery",
			domain:   "gastown.local",
			want:     "gastown.refinery@gastown.local",
		},
		{
			name:     "mayor with trailing slash",
			identity: "mayor/",
			domain:   "gastown.local",
			want:     "mayor@gastown.local",
		},
		{
			name:     "deacon with trailing slash",
			identity: "deacon/",
			domain:   "gastown.local",
			want:     "deacon@gastown.local",
		},
		{
			name:     "custom domain",
			identity: "myrig/crew/alice",
			domain:   "example.com",
			want:     "myrig.crew.alice@example.com",
		},
		{
			name:     "deeply nested",
			identity: "rig/polecats/nested/deep",
			domain:   "test.io",
			want:     "rig.polecats.nested.deep@test.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identityToEmail(tt.identity, tt.domain)
			if got != tt.want {
				t.Errorf("identityToEmail(%q, %q) = %q, want %q",
					tt.identity, tt.domain, got, tt.want)
			}
		})
	}
}

func TestCommitIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		cwd  string
		want string
	}{
		{
			name: "no env in crew dir passes through",
			cwd:  "/home/u/gt/gastown/crew/max",
			want: "",
		},
		{
			name: "no env in polecat dir passes through",
			cwd:  "/home/u/gt/gastown/polecats/slit/gastown",
			want: "",
		},
		{
			name: "no env in witness dir passes through",
			cwd:  "/home/u/gt/gastown/witness",
			want: "",
		},
		{
			name: "GT_POLECAT alone passes through",
			env:  map[string]string{"GT_POLECAT": "slit", "GT_RIG": "gastown"},
			cwd:  "/home/u/gt/gastown/polecats/slit/gastown",
			want: "",
		},
		{
			name: "BD_ACTOR alone passes through",
			env:  map[string]string{"BD_ACTOR": "daemon"},
			cwd:  "/home/u/gt/gastown/crew/max",
			want: "",
		},
		{
			name: "GT_ROLE crew with env",
			env:  map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "max"},
			cwd:  "/elsewhere",
			want: "gastown/crew/max",
		},
		{
			name: "GT_ROLE polecat with env",
			env:  map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "slit"},
			cwd:  "/elsewhere",
			want: "gastown/slit",
		},
		{
			name: "GT_ROLE full address",
			env:  map[string]string{"GT_ROLE": "gastown/crew/jack"},
			cwd:  "/elsewhere",
			want: "gastown/crew/jack",
		},
		{
			name: "GT_ROLE simple name falls back to cwd",
			env:  map[string]string{"GT_ROLE": "crew"},
			cwd:  "/home/u/gt/gastown/crew/max",
			want: "gastown/crew/max",
		},
		{
			name: "GT_ROLE that resolves to no agent passes through",
			env:  map[string]string{"GT_ROLE": "crew"},
			cwd:  "/elsewhere",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string { return tt.env[k] }
			if got := commitIdentity(getenv, tt.cwd); got != tt.want {
				t.Errorf("commitIdentity(%v, %q) = %q, want %q", tt.env, tt.cwd, got, tt.want)
			}
		})
	}
}
