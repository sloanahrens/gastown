package bdgateway

import (
	"errors"
	"maps"
	"testing"
)

// TestEnvAllowlist: accepted names are forwarded, everything else is dropped
// (never forwarded), the read class forces BD_READONLY true, and a token class
// the gateway does not know is an error.
func TestEnvAllowlist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		class   TokenClass
		env     map[string]string
		want    map[string]string
		wantErr error
	}{
		{
			name:  "accepted names are forwarded and unknown names dropped",
			class: TokenDaemon,
			env: map[string]string{
				"BD_ACTOR":               "town/crew/dipper",
				"BEADS_ACTOR":            "town/crew/dipper",
				"BD_MACHINE":             "1",
				"BD_JSON_ENVELOPE":       "1",
				"BD_READONLY":            "false",
				"BD_DOLT_AUTO_COMMIT":    "1",
				"BEADS_DIR":              "/town/gastown/.beads",
				"BEADS_DOLT_SERVER_HOST": "dolt",
				"BEADS_DOLT_SERVER_PORT": "3307",
				"TRACKER_URL":            "http://trackerd",
				"TRACKER_TOKEN_FILE":     "/run/secrets/tracker",
				"HOME":                   "/root",
				"PATH":                   "/usr/local/bin",
				"BD_ALLOW_NEW_DATABASE":  "1",
				"SOMETHING_ELSE":         "x",
			},
			want: map[string]string{
				"BD_ACTOR":            "town/crew/dipper",
				"BEADS_ACTOR":         "town/crew/dipper",
				"BD_MACHINE":          "1",
				"BD_JSON_ENVELOPE":    "1",
				"BD_READONLY":         "false",
				"BD_DOLT_AUTO_COMMIT": "1",
			},
		},
		{
			name:  "read class forces BD_READONLY true over a caller false",
			class: TokenRead,
			env:   map[string]string{"BD_READONLY": "false", "BD_ACTOR": "dashboard"},
			want:  map[string]string{"BD_READONLY": "true", "BD_ACTOR": "dashboard"},
		},
		{
			name:  "read class forces BD_READONLY true when the caller sent none",
			class: TokenRead,
			env:   map[string]string{"BD_ACTOR": "dashboard"},
			want:  map[string]string{"BD_READONLY": "true", "BD_ACTOR": "dashboard"},
		},
		{
			name:  "daemon class keeps the caller's BD_READONLY",
			class: TokenDaemon,
			env:   map[string]string{"BD_READONLY": "true"},
			want:  map[string]string{"BD_READONLY": "true"},
		},
		{
			name:  "empty environment",
			class: TokenDaemon,
			env:   nil,
			want:  map[string]string{},
		},
		{
			name:    "unknown token class",
			class:   TokenClass("root"),
			env:     map[string]string{"BD_ACTOR": "someone"},
			wantErr: ErrUnknownTokenClass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FilterEnv(tc.class, tc.env)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("FilterEnv(%q) = %v, want error %v", tc.class, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("FilterEnv(%q) = %v, want %v", tc.class, got, tc.want)
			}
		})
	}
}
