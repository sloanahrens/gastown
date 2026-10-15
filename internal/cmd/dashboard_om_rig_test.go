package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// omTestRead is the rig-file read a test hands the reader.
func omTestRead(data string) func() ([]byte, bool, error) {
	return func() ([]byte, bool, error) { return []byte(data), true, nil }
}

func omTestPtr(f float64) *float64 { return &f }

// The panel reports the bar the landing worker holds a landing to: the rig's
// own .om.json, not the user's om config (gt-ndbwf). The user config is the
// fallback only for a rig with none, and the caption says which one spoke.
func TestOMPanelThresholdComesFromTheRigsOMJSON(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	userCfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(userCfg, []byte(`{"backend":["/x/claude-deepseek-flash","-p"],"threshold":0.6,"depth":"deep","timeout":300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		rigOM      func() ([]byte, bool, error)
		wantThr    *float64
		wantDepth  string
		wantTO     int
		wantSource string
		wantModel  string
	}{
		{"rig bar over the user config", omTestRead(`{"threshold":0.7,"depth":"standard","timeout":900}`),
			omTestPtr(0.7), "standard", 900, "gastown .om.json", "flash"},
		{"rig with no .om.json falls back to the user config", func() ([]byte, bool, error) { return nil, false, nil },
			omTestPtr(0.6), "deep", 300, "~/.config/om/config.json, gastown has no .om.json", "flash"},
		{"rig file without a threshold falls back for it", omTestRead(`{"depth":"standard"}`),
			omTestPtr(0.6), "standard", 300, "~/.config/om/config.json, gastown .om.json sets no threshold", "flash"},
		// om ignores a backend key in the reviewed tree's .om.json, so the
		// model stays the user config's.
		{"rig backend key is ignored", omTestRead(`{"threshold":0.7,"backend":["/x/claude-deepseek-pro","-p"]}`),
			omTestPtr(0.7), "deep", 300, "gastown .om.json", "flash"},
		// A rig file the worker cannot parse fails every review: the panel
		// shows no bar, and says why, rather than the user's 0.6.
		{"unparseable rig file", omTestRead(`{"threshold":`),
			nil, "", 0, "gastown .om.json unreadable: unexpected end of JSON input", "flash"},
		{"rig file that cannot be read", func() ([]byte, bool, error) { return nil, false, errors.New("git: bad object") },
			nil, "", 0, "gastown .om.json unreadable: git: bad object", "flash"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &omReader{logPath: omTestLogFile(t, omTestMergeLine), landings: &dashLandings{}, cfgPath: userCfg, rigOM: tt.rigOM}
			om := r.read(now)
			if om == nil {
				t.Fatal("the read came back empty")
			}
			if (om.Threshold == nil) != (tt.wantThr == nil) || (tt.wantThr != nil && *om.Threshold != *tt.wantThr) {
				t.Errorf("threshold = %v, want %v", om.Threshold, tt.wantThr)
			}
			if om.Depth != tt.wantDepth || om.TimeoutS != tt.wantTO || om.ThresholdSource != tt.wantSource || om.Model != tt.wantModel {
				t.Errorf("depth %q timeout %d source %q model %q; want %q %d %q %q",
					om.Depth, om.TimeoutS, om.ThresholdSource, om.Model, tt.wantDepth, tt.wantTO, tt.wantSource, tt.wantModel)
			}
		})
	}
}

// The rig's file is read through git, so the reader keeps a read for
// omRigConfigTTL and re-reads after it: a landing that changes the bar shows
// within a minute, and a poll does not start git each time.
func TestOMPanelRigReadIsCachedForItsTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	calls := 0
	r := &omReader{rigOM: func() ([]byte, bool, error) {
		calls++
		return []byte(`{"threshold":0.7}`), true, nil
	}}
	r.rigRead(now)
	r.rigRead(now.Add(omRigConfigTTL - time.Second))
	if calls != 1 {
		t.Fatalf("read %d times inside the TTL, want 1", calls)
	}
	r.rigRead(now.Add(omRigConfigTTL))
	if calls != 2 {
		t.Fatalf("read %d times after the TTL, want 2", calls)
	}
}

// fakeOMFile answers ShowFileAtRev with a canned result.
type fakeOMFile struct {
	out string
	err error
}

func (f fakeOMFile) ShowFileAtRev(ref, path string) (string, error) { return f.out, f.err }

// omFileAt tells a target with no .om.json (git.ErrNotAtRef) from a read that
// failed: only the first is "the rig has none". A broken repository must never
// read as a rig without a file, or the panel would show the user's bar as the
// landing worker's.
func TestOMPanelFileAtSeparatesAbsentFromBroken(t *testing.T) {
	t.Parallel()
	data, found, err := omFileAt(fakeOMFile{out: `{"threshold":0.7}`}, "origin/main")
	if err != nil || !found || string(data) != `{"threshold":0.7}` {
		t.Errorf("present file: %q found=%v err=%v", data, found, err)
	}
	data, found, err = omFileAt(fakeOMFile{err: git.ErrNotAtRef}, "origin/main")
	if err != nil || found || data != nil {
		t.Errorf("absent file: %q found=%v err=%v; want none and no error", data, found, err)
	}
	broken := errors.New("resolving origin/main: not a git repository")
	if _, found, err = omFileAt(fakeOMFile{err: broken}, "origin/main"); !errors.Is(err, broken) || found {
		t.Errorf("broken repository: found=%v err=%v; want the error and not none", found, err)
	}
}

// fakeOMRig is a rig's bare repository for readRigOMJSONIn: refs maps a ref to
// the .om.json at its tip, and a ref it does not hold fails to resolve, as in
// git, so a read at the wrong ref is an error and never a file. remotes
// and order are the remotes the landing target resolves against.
type fakeOMRig struct {
	refs    map[string]string
	remotes map[string]string // remote name -> URL
	order   []string
}

func (f *fakeOMRig) ShowFileAtRev(ref, path string) (string, error) {
	out, ok := f.refs[ref]
	if !ok {
		return "", errors.New("resolving " + ref + ": unknown revision")
	}
	if out == "" {
		return "", git.ErrNotAtRef
	}
	return out, nil
}

func (f *fakeOMRig) Remotes() ([]string, error)         { return f.order, nil }
func (f *fakeOMRig) RemoteURL(n string) (string, error) { return f.remotes[n], nil }

// omRigTown writes a town with one rig whose config.json is cfg ("" writes
// none) and returns the town root.
func omRigTown(t *testing.T, cfg string) string {
	t.Helper()
	town := t.TempDir()
	rigDir := filepath.Join(town, omPanelRig)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

func omRigCfg(branch, forgejoURL string) string {
	cfg := `{"type":"rig","version":1,"name":"` + omPanelRig + `"`
	if branch != "" {
		cfg += `,"default_branch":"` + branch + `"`
	}
	if forgejoURL != "" {
		cfg += `,"merge_queue":{"forgejo":{"remote_url":"` + forgejoURL + `"}}`
	}
	return cfg + "}"
}

// The panel reads the file at the rig's landing target, the branch the worker
// merges onto. A rig that lands on master has its .om.json there, and a read at
// origin/main would show the wrong bar or none while every stubbed-reader test
// stays green (gt-8yk8n).
func TestReadRigOMJSON_UsesConfiguredDefaultBranch(t *testing.T) {
	t.Parallel()
	town := omRigTown(t, omRigCfg("master", ""))
	repo := &fakeOMRig{refs: map[string]string{"origin/master": `{"threshold":0.8}`}}
	var opened string
	data, found, err := readRigOMJSONIn(town, omPanelRig, func(path string) omRigRepo { opened = path; return repo })
	if err != nil || !found || string(data) != `{"threshold":0.8}` {
		t.Errorf("read = %q found=%v err=%v; want master's file", data, found, err)
	}
	if want := filepath.Join(town, omPanelRig, ".repo.git"); opened != want {
		t.Errorf("opened %s, want the rig's bare repository %s", opened, want)
	}
}

// Without a configured branch the target is main, whether the rig has no
// config.json or one that names none.
func TestReadRigOMJSON_DefaultsToMain(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]string{"no config.json": "", "config without default_branch": omRigCfg("", "")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeOMRig{refs: map[string]string{"origin/main": `{"threshold":0.7}`, "origin/master": `{"threshold":0.1}`}}
			data, found, err := readRigOMJSONIn(omRigTown(t, cfg), omPanelRig, func(string) omRigRepo { return repo })
			if err != nil || !found || string(data) != `{"threshold":0.7}` {
				t.Errorf("read = %q found=%v err=%v; want main's file", data, found, err)
			}
		})
	}
}

// A landing target with no .om.json is none; the rig's remote and branch still
// pick the ref it was looked for at.
func TestReadRigOMJSON_TargetWithoutTheFileIsNone(t *testing.T) {
	t.Parallel()
	repo := &fakeOMRig{refs: map[string]string{"origin/master": ""}}
	data, found, err := readRigOMJSONIn(omRigTown(t, omRigCfg("master", "")), omPanelRig, func(string) omRigRepo { return repo })
	if err != nil || found || data != nil {
		t.Errorf("read = %q found=%v err=%v; want none and no error", data, found, err)
	}
}

// The ref is the landing remote's, not always origin: a rig cut over to
// Forgejo lands on the remote carrying the configured URL.
func TestReadRigOMJSON_ReadsTheLandingRemotesBranch(t *testing.T) {
	t.Parallel()
	const url = "https://forgejo.example/gastown/gastown"
	repo := &fakeOMRig{
		order:   []string{"origin", "forgejo"},
		remotes: map[string]string{"origin": "https://github.com/acme/gastown.git", "forgejo": url},
		refs:    map[string]string{"forgejo/master": `{"threshold":0.9}`, "origin/master": `{"threshold":0.1}`},
	}
	data, found, err := readRigOMJSONIn(omRigTown(t, omRigCfg("master", url)), omPanelRig, func(string) omRigRepo { return repo })
	if err != nil || !found || string(data) != `{"threshold":0.9}` {
		t.Errorf("read = %q found=%v err=%v; want forgejo/master's file", data, found, err)
	}
}

// A failure in either lookup is an error, never "the rig has no .om.json": the
// panel would otherwise show the user's bar as the one the worker enforces.
func TestReadRigOMJSON_ConfigAndRemoteFailuresAreErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		// The landing remote is resolved first: a Forgejo URL no remote carries.
		"landing remote matches no remote": omRigCfg("main", "https://forgejo.example/other/repo"),
		// A config.json that does not decode must not read as an unconfigured rig.
		"rig config that does not decode": `{"type":"rig","version":1,"name":"` + omPanelRig + `","defualt_branch":"master"}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeOMRig{refs: map[string]string{"origin/main": `{"threshold":0.7}`}}
			data, found, err := readRigOMJSONIn(omRigTown(t, cfg), omPanelRig, func(string) omRigRepo { return repo })
			if err == nil || found || data != nil {
				t.Errorf("read = %q found=%v err=%v; want an error", data, found, err)
			}
		})
	}
}
