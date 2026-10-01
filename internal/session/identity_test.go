package session

import (
	"testing"
)

// testRegistry returns a PrefixRegistry populated with test rig prefixes.
func testRegistry() *PrefixRegistry {
	r := NewPrefixRegistry()
	r.Register("gt", "gastown")
	r.Register("bd", "beads")
	r.Register("hop", "hop")
	r.Register("sky", "sky")
	r.Register("mp", "my-project")
	r.Register("hq", "knjn")
	return r
}

func TestParseSessionName(t *testing.T) {
	t.Parallel()
	reg := testRegistry()

	tests := []struct {
		name       string
		session    string
		wantRole   Role
		wantRig    string
		wantName   string
		wantPrefix string
		wantErr    bool
	}{
		// Town-level roles (hq-mayor)
		{
			name:     "mayor",
			session:  "hq-mayor",
			wantRole: RoleMayor,
		},

		// Rig prefix "hq" collision: hq-<polecat>
		// should resolve as rig-level roles when "hq" is a registered prefix.
		{
			// Refinery role removed (gt-v4ssj.6): no longer parses as refinery.
			name:       "refinery session parses as polecat",
			session:    "gt-refinery",
			wantRole:   RolePolecat,
			wantRig:    "gastown",
			wantName:   "refinery",
			wantPrefix: "gt",
		},
		{
			name:       "hq prefix polecat",
			session:    "hq-jasper",
			wantRole:   RolePolecat,
			wantRig:    "knjn",
			wantName:   "jasper",
			wantPrefix: "hq",
		},
		{
			name:       "hq prefix crew",
			session:    "hq-crew-rushd",
			wantRole:   RoleCrew,
			wantRig:    "knjn",
			wantName:   "rushd",
			wantPrefix: "hq",
		},

		{
			// Witness role retired (gt-4k3fj.6.1): no longer parses as witness.
			name:       "witness session parses as polecat",
			session:    "gt-witness",
			wantRole:   RolePolecat,
			wantRig:    "gastown",
			wantName:   "witness",
			wantPrefix: "gt",
		},

		// Crew (new format: <prefix>-crew-<name>)
		{
			name:       "crew gastown",
			session:    "gt-crew-max",
			wantRole:   RoleCrew,
			wantRig:    "gastown",
			wantName:   "max",
			wantPrefix: "gt",
		},
		{
			name:       "crew beads",
			session:    "bd-crew-alice",
			wantRole:   RoleCrew,
			wantRig:    "beads",
			wantName:   "alice",
			wantPrefix: "bd",
		},
		{
			name:       "crew hyphenated name",
			session:    "gt-crew-my-worker",
			wantRole:   RoleCrew,
			wantRig:    "gastown",
			wantName:   "my-worker",
			wantPrefix: "gt",
		},

		// Polecat (new format: <prefix>-<name>)
		{
			name:       "polecat gastown",
			session:    "gt-morsov",
			wantRole:   RolePolecat,
			wantRig:    "gastown",
			wantName:   "morsov",
			wantPrefix: "gt",
		},
		{
			name:       "polecat beads",
			session:    "bd-worker1",
			wantRole:   RolePolecat,
			wantRig:    "beads",
			wantName:   "worker1",
			wantPrefix: "bd",
		},
		{
			name:       "polecat hop",
			session:    "hop-ostrom",
			wantRole:   RolePolecat,
			wantRig:    "hop",
			wantName:   "ostrom",
			wantPrefix: "hop",
		},
		{
			name:       "polecat sky",
			session:    "sky-furiosa",
			wantRole:   RolePolecat,
			wantRig:    "sky",
			wantName:   "furiosa",
			wantPrefix: "sky",
		},

		// Error cases: unknown prefixes should fail (not fall back to splitting on dash)
		{
			name:    "unknown prefix polecat",
			session: "zz-alpha",
			wantErr: true,
		},
		{
			name:    "unknown prefix witness",
			session: "foo-witness",
			wantErr: true,
		},
		{
			name:    "empty string",
			session: "",
			wantErr: true,
		},
		{
			name:    "no dash",
			session: "gtwitness",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSessionNameWithRegistry(tt.session, reg)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSessionName(%q) error = %v, wantErr %v", tt.session, err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if got.Role != tt.wantRole {
				t.Errorf("ParseSessionName(%q).Role = %v, want %v", tt.session, got.Role, tt.wantRole)
			}
			if got.Rig != tt.wantRig {
				t.Errorf("ParseSessionName(%q).Rig = %v, want %v", tt.session, got.Rig, tt.wantRig)
			}
			if got.Name != tt.wantName {
				t.Errorf("ParseSessionName(%q).Name = %v, want %v", tt.session, got.Name, tt.wantName)
			}
			if tt.wantPrefix != "" && got.Prefix != tt.wantPrefix {
				t.Errorf("ParseSessionName(%q).Prefix = %v, want %v", tt.session, got.Prefix, tt.wantPrefix)
			}
		})
	}
}

func TestAgentIdentity_SessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity AgentIdentity
		want     string
	}{
		{
			name:     "mayor",
			identity: AgentIdentity{Role: RoleMayor},
			want:     "hq-mayor",
		},
		{
			name:     "crew",
			identity: AgentIdentity{Role: RoleCrew, Rig: "gastown", Name: "max", Prefix: "gt"},
			want:     "gt-crew-max",
		},
		{
			name:     "polecat",
			identity: AgentIdentity{Role: RolePolecat, Rig: "gastown", Name: "morsov", Prefix: "gt"},
			want:     "gt-morsov",
		},
		{
			name:     "polecat hop",
			identity: AgentIdentity{Role: RolePolecat, Rig: "hop", Name: "ostrom", Prefix: "hop"},
			want:     "hop-ostrom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.identity.SessionName(); got != tt.want {
				t.Errorf("AgentIdentity.SessionName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentIdentity_Address(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity AgentIdentity
		want     string
	}{
		{
			name:     "mayor",
			identity: AgentIdentity{Role: RoleMayor},
			want:     "mayor",
		},
		{
			name:     "crew",
			identity: AgentIdentity{Role: RoleCrew, Rig: "gastown", Name: "max", Prefix: "gt"},
			want:     "gastown/crew/max",
		},
		{
			name:     "polecat",
			identity: AgentIdentity{Role: RolePolecat, Rig: "gastown", Name: "Toast", Prefix: "gt"},
			want:     "gastown/polecats/Toast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.identity.Address(); got != tt.want {
				t.Errorf("AgentIdentity.Address() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSessionName_RoundTrip(t *testing.T) {
	t.Parallel()
	reg := testRegistry()

	// Test that parsing then reconstructing gives the same result
	sessions := []string{
		"hq-mayor",
		"hq-deacon",
		"gt-witness",
		"gt-crew-max",
		"gt-morsov",
		"hop-ostrom",
		"sky-furiosa",
		"hq-witness",
		"hq-jasper",
		"hq-crew-rushd",
	}

	for _, sess := range sessions {
		t.Run(sess, func(t *testing.T) {
			identity, err := ParseSessionNameWithRegistry(sess, reg)
			if err != nil {
				t.Fatalf("ParseSessionName(%q) error = %v", sess, err)
			}
			if got := identity.SessionName(); got != sess {
				t.Errorf("Round-trip failed: ParseSessionName(%q).SessionName() = %q", sess, got)
			}
		})
	}
}

func TestParseAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		address string
		want    AgentIdentity
		wantErr bool
	}{
		{
			name:    "mayor",
			address: "mayor/",
			want:    AgentIdentity{Role: RoleMayor},
		},
		{
			// Refinery role removed (gt-v4ssj.6): "<rig>/refinery" is now
			// just a polecat that happens to be named "refinery".
			name:    "refinery is no longer a role",
			address: "rig-a/refinery",
			want:    AgentIdentity{Role: RolePolecat, Rig: "rig-a", Name: "refinery", Prefix: DefaultPrefix},
		},
		{
			name:    "crew",
			address: "gastown/crew/max",
			want:    AgentIdentity{Role: RoleCrew, Rig: "gastown", Name: "max", Prefix: "gt"},
		},
		{
			name:    "polecat explicit",
			address: "gastown/polecats/nux",
			want:    AgentIdentity{Role: RolePolecat, Rig: "gastown", Name: "nux", Prefix: "gt"},
		},
		{
			name:    "polecat canonical",
			address: "gastown/nux",
			want:    AgentIdentity{Role: RolePolecat, Rig: "gastown", Name: "nux", Prefix: "gt"},
		},
		{
			name:    "registered prefix",
			address: "my-project/crew/max",
			want:    AgentIdentity{Role: RoleCrew, Rig: "my-project", Name: "max", Prefix: "mp"},
		},
		{
			name:    "invalid",
			address: "gastown/crew",
			wantErr: true,
		},
	}

	reg := testRegistry()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAddressWithRegistry(tt.address, reg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAddress(%q) error = %v", tt.address, err)
			}
			if *got != tt.want {
				t.Fatalf("ParseAddress(%q) = %#v, want %#v", tt.address, *got, tt.want)
			}
		})
	}
}

func TestPrefixRegistry(t *testing.T) {
	t.Parallel()
	r := NewPrefixRegistry()
	r.Register("gt", "gastown")
	r.Register("bd", "beads")

	if got := r.PrefixForRig("gastown"); got != "gt" {
		t.Errorf("PrefixForRig(gastown) = %q, want %q", got, "gt")
	}
	if got := r.RigForPrefix("bd"); got != "beads" {
		t.Errorf("RigForPrefix(bd) = %q, want %q", got, "beads")
	}
	// Unknown rig returns default
	if got := r.PrefixForRig("unknown"); got != DefaultPrefix {
		t.Errorf("PrefixForRig(unknown) = %q, want %q", got, DefaultPrefix)
	}
	// Unknown prefix returns the prefix itself
	if got := r.RigForPrefix("zz"); got != "zz" {
		t.Errorf("RigForPrefix(zz) = %q, want %q", got, "zz")
	}
}
