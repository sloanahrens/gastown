package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
)

// writeViewerTokenFile writes the viewer's credential file in dir, at the mode a
// token may live at.
func writeViewerTokenFile(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "forgejo-"+config.ForgejoRoleViewer+".env")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// TestPairReadyRigResolution: a rig resolves to its repository, its API and its
// registry only when its own settings and the viewer's credential file all say
// so. Each refusal names what the rig lacks and is never a half-resolved rig.
func TestPairReadyRigResolution(t *testing.T) {
	t.Parallel()
	const remote = "https://forgejo.example.com/town/gastown.git"
	goodToken := "FORGEJO_USER=viewer-bot\nFORGEJO_TOKEN=viewer-token-never-logged\n"

	cases := []struct {
		name         string
		fc           *config.ForgejoConfig
		resolveErr   error
		token        *string // nil: no token file in the directory
		tokenDirErr  error
		wantErr      string
		wantOwner    string
		wantRegOwner string
	}{
		{
			name:        "the token directory cannot be found",
			fc:          &config.ForgejoConfig{RemoteURL: remote},
			token:       &goodToken,
			tokenDirErr: errors.New("no home directory"),
			wantErr:     "Forgejo token directory",
		},
		{
			name:       "the rig's settings cannot be read",
			fc:         &config.ForgejoConfig{RemoteURL: remote},
			token:      &goodToken,
			resolveErr: errors.New("rig.json: unexpected end of input"),
			wantErr:    "could not be read",
		},
		{
			name:    "the rig names no settings block",
			fc:      nil,
			token:   &goodToken,
			wantErr: "names no merge_queue.forgejo remote_url",
		},
		{
			name:    "the rig's remote is blank",
			fc:      &config.ForgejoConfig{RemoteURL: "   "},
			token:   &goodToken,
			wantErr: "names no merge_queue.forgejo remote_url",
		},
		{
			name:    "the remote has no scheme",
			fc:      &config.ForgejoConfig{RemoteURL: "forgejo.example.com/town/gastown"},
			token:   &goodToken,
			wantErr: "is not a URL with a scheme and host",
		},
		{
			name:    "the remote names no owner and repository",
			fc:      &config.ForgejoConfig{RemoteURL: "https://forgejo.example.com/"},
			token:   &goodToken,
			wantErr: "names no owner and repository",
		},
		{
			name:    "the registry owner the rig names is not an account",
			fc:      &config.ForgejoConfig{RemoteURL: remote, RegistryOwner: "Bad Owner"},
			token:   &goodToken,
			wantErr: "names no usable registry",
		},
		{
			name:    "the viewer's token file is missing",
			fc:      &config.ForgejoConfig{RemoteURL: remote},
			token:   nil,
			wantErr: "Forgejo token",
		},
		{
			name:    "the viewer's token file holds no token",
			fc:      &config.ForgejoConfig{RemoteURL: remote},
			token:   ptrTo("FORGEJO_USER=viewer-bot\n"),
			wantErr: "Forgejo token",
		},
		{
			name:    "the viewer's token file names no login",
			fc:      &config.ForgejoConfig{RemoteURL: remote},
			token:   ptrTo("FORGEJO_TOKEN=viewer-token-never-logged\n"),
			wantErr: "token file",
		},
		{
			name:         "the rig resolves to its repository and its registry owner",
			fc:           &config.ForgejoConfig{RemoteURL: remote, RegistryOwner: "bot-registry"},
			token:        &goodToken,
			wantOwner:    "town",
			wantRegOwner: "bot-registry",
		},
		{
			name:         "without a registry owner the repository's owner publishes",
			fc:           &config.ForgejoConfig{RemoteURL: remote},
			token:        &goodToken,
			wantOwner:    "town",
			wantRegOwner: "town",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.token != nil {
				writeViewerTokenFile(t, dir, *tc.token)
			}
			got := pairReadyRigFrom("gastown", tc.fc, tc.resolveErr, dir, tc.tokenDirErr)
			if tc.wantErr != "" {
				require.Error(t, got.Err)
				assert.Contains(t, got.Err.Error(), tc.wantErr)
				assert.NotContains(t, got.Err.Error(), "viewer-token-never-logged", "a refusal carries the token")
				assert.Nil(t, got.API, "a refused rig is half resolved")
				assert.Nil(t, got.Registry, "a refused rig is half resolved")
				return
			}
			require.NoError(t, got.Err)
			assert.Equal(t, "gastown", got.Rig)
			assert.Equal(t, tc.wantOwner, got.Owner)
			assert.Equal(t, "gastown", got.Repo)
			assert.NotNil(t, got.API)
			reader, ok := got.Registry.(pairReadyTagReader)
			require.True(t, ok, "the registry is %T, want the viewer's tag reader", got.Registry)
			assert.Equal(t, tc.wantRegOwner, reader.reg.Owner)
			assert.Equal(t, "https", reader.reg.Scheme)
			assert.Equal(t, "forgejo.example.com", reader.reg.Host)
			assert.Equal(t, "viewer-bot", reader.user)
		})
	}
}

// TestPairReadyRefusesAnUnknownImageRig: an image rig the town does not register
// is refused by name before any rig is read, so no read reaches a repository the
// config did not name.
func TestPairReadyRefusesAnUnknownImageRig(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newTierSweepDaemon(t, pairT0, "gastown")
	d.patrolConfig.Patrols.TierSweep.ImageRigs = []string{"gastwon"}

	rigs, err := d.productionPairReadyRigs()
	require.Error(t, err)
	assert.Nil(t, rigs)
	assert.Contains(t, err.Error(), `"gastwon" is not a known rig`)
}

// ptrTo is a pointer to s, for a case whose token file body is set.
func ptrTo(s string) *string { return &s }
