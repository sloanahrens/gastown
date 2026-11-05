package daemon

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// TestPromoterSkipsAppPromoteRigs: a rig whose promote_via leaves GitHub main
// to gt promote has no daemon-side promoter on either path - the post-land
// verdict or the tier sweep - while the same rig without the key promotes as
// before; an unknown value is refused the same way and the refusal is logged
// (gt-182zb).
func TestPromoterSkipsAppPromoteRigs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, promoteVia string
		want             bool
		wantLog          string
	}{
		{"app-promote", `,"promote_via":"app-promote"`, false, "app-promote"},
		{"no promote_via", "", true, ""},
		{"unknown value", `,"promote_via":"app_promote"`, false, "promote_via"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			rigPath := filepath.Join(townRoot, "testrig")
			body := `{"type":"rig","version":1,"name":"testrig","merge_queue":{"forgejo":{"promote_target":"git@github.com:example/testrig.git","promote_key_file":"/home/gt/promote-testrig.key"` + tc.promoteVia + `}}}`
			writeDaemonRigConfigFile(t, rigPath, body)

			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0), config: &Config{TownRoot: townRoot}}
			fj := rig.ResolveForgejoConfigIn(d.repoFileOpener(), townRoot, "testrig")
			if fj == nil {
				t.Fatal("the fixture's forgejo block did not resolve")
			}
			repo := filepath.Join(rigPath, ".repo.git")

			landing := d.newPromoter("testrig", repo, fj)
			sweep := d.tierSweepPromoter("testrig", repo)
			if (landing != nil) != tc.want {
				t.Errorf("landing worker promoter = %v, want non-nil = %v", landing, tc.want)
			}
			if (sweep != nil) != tc.want {
				t.Errorf("tier sweep promoter = %v, want non-nil = %v", sweep, tc.want)
			}
			if tc.wantLog != "" {
				got := logs.String()
				if !strings.Contains(got, tc.wantLog) || !strings.Contains(got, "testrig") {
					t.Errorf("log = %q, want the refusal for testrig naming %s", got, tc.wantLog)
				}
			}
		})
	}
}
