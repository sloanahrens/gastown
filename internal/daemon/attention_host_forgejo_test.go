package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// forgejoStubTransport answers every health request with the status or error
// the test set, so no test opens a socket.
type forgejoStubTransport struct {
	req    *http.Request
	status int
	err    error
}

func (s *forgejoStubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.req = r
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     fmt.Sprintf("%d %s", s.status, http.StatusText(s.status)),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}, nil
}

// probesFor builds the production health probe over a fixed origin and the
// stub transport.
func probesFor(origin string, originErr error, tr *forgejoStubTransport) systemHostProbes {
	return systemHostProbes{
		forgejoBaseURL: func() (string, error) { return origin, originErr },
		forgejoHTTP:    &http.Client{Transport: tr},
	}
}

// forgejoProbeHost is the fake host with the production Forgejo health read:
// the origin resolves through the production function and the request goes to
// a stub transport, while disk, memory, docker and the park state stay fake so
// a tick starts no external tool.
type forgejoProbeHost struct {
	*fakeHost
	health systemHostProbes
}

func (h forgejoProbeHost) ForgejoHealthy(ctx context.Context) error {
	return h.health.ForgejoHealthy(ctx)
}

// One unanswered health read is an info line; attentionForgejoProbeDownBeats in
// a row raises the High host.forgejo_down naming the origin and no credential,
// and a healthy read clears both.
func TestHostForgejoHealthRaisesAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.forgejoErr = errors.New("forgejo health https://forgejo.example/api/healthz: dial tcp: connection refused")
	f.src.host = h
	wakes := func() []attention.Event {
		var out []attention.Event
		for _, e := range f.events(t) {
			if e.Wake {
				out = append(out, e)
			}
		}
		return out
	}

	st := f.tick(t, now.Add(time.Minute))
	probe := itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)
	if probe.Severity != attention.SeverityLow || probe.Kind != attention.KindHostProbe {
		t.Errorf("one failed read = %+v, want a low host-probe item", probe)
	}
	if items := hostItems(st); len(items) != 1 {
		t.Errorf("host items after one failed read = %v, want only the probe line", hostKeys(st))
	}
	if _, ok := attention.Find(st, hostRuleForgejoDown); ok {
		t.Errorf("host items = %v, want no %s yet", hostKeys(st), hostRuleForgejoDown)
	}
	if got := wakes(); len(got) != 0 {
		t.Fatalf("one failed read wake events = %+v, want none", got)
	}

	for beat := 2; beat < attentionForgejoProbeDownBeats; beat++ {
		st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
		if _, ok := attention.Find(st, hostRuleForgejoDown); ok {
			t.Fatalf("beat %d raised %s before the configured run", beat, hostRuleForgejoDown)
		}
	}

	st = f.tick(t, now.Add(time.Duration(attentionForgejoProbeDownBeats)*time.Minute))
	down := itemByKey(t, st, hostRuleForgejoDown)
	if down.Severity != attention.SeverityHigh || down.Kind != attention.KindHostForgejo {
		t.Errorf("%s = %+v, want a high host-forgejo item", hostRuleForgejoDown, down)
	}
	if !strings.Contains(down.Summary, "https://forgejo.example/api/healthz") {
		t.Errorf("summary = %q, want it to name the origin", down.Summary)
	}
	if !strings.Contains(down.Summary, fmt.Sprintf("%d heartbeats", attentionForgejoProbeDownBeats)) {
		t.Errorf("summary = %q, want it to name the run length", down.Summary)
	}
	if strings.Contains(down.Summary, "@") || strings.Contains(down.Summary, "\n") {
		t.Errorf("summary = %q, want one line and no credential", down.Summary)
	}
	got := wakes()
	if len(got) != 1 || got[0].Rule != attention.RuleHostForgejoDown || got[0].Handled != attention.HandledOperatorAction || got[0].State != attention.EventNew {
		t.Fatalf("wake events = %+v, want one raised host.forgejo_down", got)
	}

	f.tick(t, now.Add(time.Duration(attentionForgejoProbeDownBeats+1)*time.Minute))
	if n := len(wakes()); n != 1 {
		t.Errorf("wake events after a further failed tick = %d, want the one", n)
	}

	h.forgejoErr = nil
	st = f.tick(t, now.Add(time.Duration(attentionForgejoProbeDownBeats+2)*time.Minute))
	if items := hostItems(st); len(items) != 0 {
		t.Errorf("host items = %v, want both cleared once the instance answers", hostKeys(st))
	}
}

// A town where no rig the landing worker serves names a Forgejo has nothing to
// watch: the probe is not a failure, and an item a configured town raised is
// dropped rather than alarmed on for good.
func TestHostForgejoNotConfiguredRaisesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	f.src.host = h

	st := f.tick(t, now)
	if items := hostItems(st); len(items) != 0 {
		t.Fatalf("host items in a town with no Forgejo = %v, want none", hostKeys(st))
	}

	h.forgejoErr = errors.New("forgejo health https://forgejo.example/api/healthz: connection refused")
	for beat := 1; beat <= attentionForgejoProbeDownBeats; beat++ {
		st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}
	itemByKey(t, st, hostRuleForgejoDown)

	h.forgejoErr = errForgejoNotConfigured
	st = f.tick(t, now.Add(time.Duration(attentionForgejoProbeDownBeats+1)*time.Minute))
	if items := hostItems(st); len(items) != 0 {
		t.Errorf("host items = %v, want the raised item cleared when the town runs no Forgejo", hostKeys(st))
	}
}

// The production health read asks the origin's /api/healthz, bounds the request
// by attentionForgejoHealthTimeout, carries no credential, and refuses every
// answer but 200.
func TestHostForgejoHealthProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		origin       string
		originErr    error
		status       int
		transportErr error
		// wantErr is a substring of the error, "" for a healthy read.
		wantErr string
		// wantUnreadable marks a read that must be errForgejoSettingUnreadable,
		// so the collector keeps its last answer rather than reading a broken
		// setting as no Forgejo at all.
		wantUnreadable bool
		// noRequest is set for a probe that must refuse before it can ask.
		noRequest bool
	}{
		{name: "healthy", origin: "https://forgejo.example", status: http.StatusOK},
		{name: "a trailing slash on the origin", origin: "https://forgejo.example/", status: http.StatusOK},
		{name: "a 503 is not healthy", origin: "https://forgejo.example", status: http.StatusServiceUnavailable, wantErr: "503 Service Unavailable"},
		{name: "an unreachable instance", origin: "https://forgejo.example", transportErr: errors.New("dial tcp: connection refused"), wantErr: "connection refused"},
		{name: "an unreadable setting", originErr: errors.New("reading the rig settings: permission denied"),
			wantErr: "permission denied", wantUnreadable: true, noRequest: true},
		{name: "a town with no Forgejo", origin: "", wantErr: errForgejoNotConfigured.Error(), noRequest: true},
		{name: "an origin no request can name", origin: "://forgejo.example", wantErr: "missing protocol scheme", noRequest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := &forgejoStubTransport{status: tt.status, err: tt.transportErr}
			p := probesFor(tt.origin, tt.originErr, tr)
			before := time.Now()
			err := p.ForgejoHealthy(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ForgejoHealthy = %v, want healthy", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ForgejoHealthy = %v, want it to name %q", err, tt.wantErr)
			}
			if got := errors.Is(err, errForgejoSettingUnreadable); got != tt.wantUnreadable {
				t.Errorf("ForgejoHealthy = %v, unreadable = %v; want %v", err, got, tt.wantUnreadable)
			}
			if tt.noRequest {
				if tr.req != nil {
					t.Errorf("a probe that must refuse made a request to %s", tr.req.URL)
				}
				return
			}
			if tr.req == nil {
				t.Fatal("no request was made")
			}
			if tr.req.Method != http.MethodGet || tr.req.URL.Path != forgejoHealthPath {
				t.Errorf("request = %s %s, want GET %s", tr.req.Method, tr.req.URL, forgejoHealthPath)
			}
			if tr.req.URL.User != nil || tr.req.Header.Get("Authorization") != "" {
				t.Errorf("request carries a credential: %s", tr.req.URL)
			}
			deadline, ok := tr.req.Context().Deadline()
			if !ok {
				t.Fatal("request has no deadline")
			}
			if d := deadline.Sub(before); d > attentionForgejoHealthTimeout+time.Second || d < attentionForgejoHealthTimeout-time.Second {
				t.Errorf("request deadline is %v away, want about %v", d, attentionForgejoHealthTimeout)
			}
		})
	}
}

// The probe built with no Forgejo URL source is a wiring error, never a
// healthy answer.
func TestHostForgejoHealthProbeNeedsAURLSource(t *testing.T) {
	t.Parallel()
	err := (systemHostProbes{}).ForgejoHealthy(context.Background())
	if err == nil {
		t.Fatal("ForgejoHealthy with no URL source = nil, want an error")
	}
}

// The origin a remote URL names keeps the scheme and host and drops the path,
// the query and any user info, so no credential can reach a summary. A URL
// that does not parse yields an error that names the rig and the field, and
// the host only when it is recoverable after the user info is stripped: no
// other part of the remote appears.
func TestForgejoOriginFromRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, remote, want string
		wantErr            string // the error it must name, "" for none
		// wantHost is the host the error may carry, "" for none.
		wantHost string
	}{
		{name: "a repository URL", remote: "https://forgejo.example/gastown/gastown.git", want: "https://forgejo.example"},
		{name: "a port", remote: "http://127.0.0.1:3000/acme/rig", want: "http://127.0.0.1:3000"},
		{name: "user info is dropped", remote: "https://bot:s3cret@forgejo.example/acme/rig.git", want: "https://forgejo.example"},
		{name: "surrounding space", remote: "  https://forgejo.example/acme/rig.git  ", want: "https://forgejo.example"},
		{
			name:     "no scheme names the rig and the field, and the host",
			remote:   "forgejo.example/acme/rig.git",
			wantErr:  "remote origin for rig alpha is unparseable",
			wantHost: "forgejo.example",
		},
		{
			name:     "user info survives no parse and is dropped from the host",
			remote:   "https://bot:s3cret@forgejo.example/acme/rig%zz.git",
			wantErr:  "remote origin for rig alpha is unparseable",
			wantHost: "forgejo.example",
		},
		{
			name:    "a credential before a slash is not a host",
			remote:  "https://svc:s3cret/x@forgejo.example:3000/acme%zz",
			wantErr: "remote origin for rig alpha is unparseable",
		},
		{
			name:    "a port that is not a number drops the host",
			remote:  "https://forgejo.example:s3cret/acme%zz",
			wantErr: "remote origin for rig alpha is unparseable",
		},
		{
			name:    "a flat name is not a recoverable host",
			remote:  "https://localhost/acme%zz",
			wantErr: "remote origin for rig alpha is unparseable",
		},
		{
			name:     "a bracketed literal is a host",
			remote:   "http://[::1]:3000/acme%zz",
			wantErr:  "remote origin for rig alpha is unparseable",
			wantHost: "[::1]:3000",
		},
		{
			name:    "a bracketed literal with trailing junk drops the host",
			remote:  "http://[::1]junk/acme%zz",
			wantErr: "remote origin for rig alpha is unparseable",
		},
		{name: "no host", remote: "https://", wantErr: "remote origin for rig alpha is unparseable"},
		{name: "empty", remote: "", wantErr: "remote origin for rig alpha is unparseable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := forgejoOriginFromRemoteURL("alpha", tt.remote)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Errorf("forgejoOriginFromRemoteURL(%q) = %q, %v; want %q", tt.remote, got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("forgejoOriginFromRemoteURL(%q) = %q, %v; want an error naming %q", tt.remote, got, err, tt.wantErr)
			}
			hasHost := strings.Contains(err.Error(), "(host ")
			switch {
			case tt.wantHost == "" && hasHost:
				t.Errorf("error = %q, want it to carry no host", err)
			case tt.wantHost != "" && !strings.Contains(err.Error(), "(host "+tt.wantHost+")"):
				t.Errorf("error = %q, want it to carry the host %q", err, tt.wantHost)
			}
			for _, s := range []string{"s3cret", "bot:"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error = %q, want it to hold no %q", err, s)
				}
			}
		})
	}
}

// rigForgejoSettings is a rig's settings/config.json naming remoteURL in its
// Forgejo block; an empty remoteURL names no block, and text the caller
// writes as-is is a file that does not parse.
func rigForgejoSettings(remoteURL string) string {
	if remoteURL == "" {
		return `{"type":"rig-settings","version":1}`
	}
	return fmt.Sprintf(`{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":%q}}}`, remoteURL)
}

// writeRigSettingsFile writes one rig's settings/config.json and returns its
// path. Anything already at the path goes first, so a test that broke the file
// into something unwritable can replace it with a good one.
func writeRigSettingsFile(t *testing.T, townRoot, rigName, body string) string {
	t.Helper()
	path := filepath.Join(townRoot, rigName, "settings", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// forgejoTown is a temp town whose rig registry lists names, each with the
// settings/config.json body the caller gives; "" leaves the rig with no
// settings file at all.
func forgejoTown(t *testing.T, settings map[string]string) string {
	t.Helper()
	town := t.TempDir()
	rigs := make(map[string]any, len(settings))
	for name := range settings {
		rigs[name] = map[string]any{}
	}
	writeJSONFile(t, filepath.Join(town, "mayor", "rigs.json"), map[string]any{"version": 1, "rigs": rigs})
	for name, body := range settings {
		if body != "" {
			writeRigSettingsFile(t, town, name, body)
		}
	}
	return town
}

// forgejoTownDaemon is a daemon over town whose git seam starts no git.
func forgejoTownDaemon(t *testing.T, town string) *Daemon {
	t.Helper()
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	useGitfake(t, d)
	return d
}

// unreadableRepo is a rig repository whose tracked-file read fails: a repo
// this run cannot read, which no settings tier may paper over.
type unreadableRepo struct{ daemonGit }

func (unreadableRepo) RefExists(string) (bool, error) {
	return false, errors.New("git: reading refs: corrupt repository")
}

// The town's Forgejo origin comes from the landing worker's setting in a real
// rig settings tree, read by the production resolver: the first rig in name
// order that names a remote URL answers, a rig naming none or with no file at
// all is passed over, a town where none names one has no Forgejo, and a
// setting or repo tier the run cannot read is an error rather than an absence.
func TestHostForgejoOriginResolvesFromRigSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		settings map[string]string
		// extra lays out the rest of the town's files and seams.
		extra   func(t *testing.T, d *Daemon)
		want    string
		wantErr string
	}{
		{
			name: "the rig that names one answers",
			settings: map[string]string{
				"alpha": rigForgejoSettings(""),
				"beta":  rigForgejoSettings("https://forgejo.example/acme/rig.git"),
			},
			want: "https://forgejo.example",
		},
		{
			name: "the first rig in name order answers",
			settings: map[string]string{
				"alpha": rigForgejoSettings("https://first.example/acme/rig.git"),
				"beta":  rigForgejoSettings("https://second.example/acme/rig.git"),
			},
			want: "https://first.example",
		},
		{
			name: "a rig with no settings file is passed over",
			settings: map[string]string{
				"alpha": "",
				"beta":  rigForgejoSettings("https://forgejo.example/acme/rig.git"),
			},
			want: "https://forgejo.example",
		},
		{
			name:     "no rig names one",
			settings: map[string]string{"alpha": rigForgejoSettings(""), "beta": ""},
		},
		{
			name:     "an empty rig list is unknown, not a town without Forgejo",
			settings: map[string]string{},
			wantErr:  "rig list is unreadable or empty",
		},
		{
			name:     "an unreadable rig registry is unknown, not a town without Forgejo",
			settings: map[string]string{"alpha": rigForgejoSettings("")},
			extra: func(t *testing.T, d *Daemon) {
				t.Helper()
				writeTownFile(t, d.config.TownRoot, filepath.Join("mayor", "rigs.json"), "{not json")
			},
			wantErr: "rig list is unreadable or empty",
		},
		{
			name:     "a remote that is not a URL names the rig and the field",
			settings: map[string]string{"alpha": rigForgejoSettings("forgejo.example/acme/rig")},
			wantErr:  "remote origin for rig alpha is unparseable",
		},
		{
			name:     "a settings file that does not parse",
			settings: map[string]string{"alpha": "{not json"},
			wantErr:  "rig alpha: reading",
		},
		{
			name:     "a rig config that does not parse",
			settings: map[string]string{"alpha": ""},
			extra: func(t *testing.T, d *Daemon) {
				t.Helper()
				writeTownFile(t, d.config.TownRoot, filepath.Join("alpha", "config.json"), "{not json")
			},
			wantErr: "rig alpha: reading",
		},
		{
			name:     "a repo tier that cannot be read",
			settings: map[string]string{"alpha": rigForgejoSettings("https://forgejo.example/acme/rig.git")},
			extra: func(t *testing.T, d *Daemon) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(d.config.TownRoot, "alpha", ".repo.git"), 0o755); err != nil {
					t.Fatal(err)
				}
				open := d.openGitFn
				d.openGitFn = func(dir string) daemonGit { return unreadableRepo{open(dir)} }
			},
			wantErr: "rig alpha: reading",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := forgejoTownDaemon(t, forgejoTown(t, tt.settings))
			if tt.extra != nil {
				tt.extra(t, d)
			}
			got, err := d.townForgejoBaseURL()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("townForgejoBaseURL() = %q, %v; want an error naming %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("townForgejoBaseURL(): %v", err)
			}
			if got != tt.want {
				t.Errorf("townForgejoBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A setting the run cannot read is not a town without Forgejo: it keeps the
// alarm an earlier tick raised open, says why in its summary, and raises
// nothing new; a setting that reads and names no URL clears it; and an
// unreadable setting with no alarm open never raises one.
func TestHostForgejoUnreadableSettingKeepsTheAlarm(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	breaks := []struct {
		name   string
		break_ func(t *testing.T, path string)
	}{
		{"a settings file that does not parse", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a settings file that cannot be read", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	// fixture is a daemon over a town whose one landing rig names origin, with
	// the production origin resolution behind the health probe.
	fixture := func(t *testing.T, origin string) (*attentionFixture, string, *forgejoStubTransport) {
		t.Helper()
		f := newAttentionFixture(t, now)
		town := f.d.config.TownRoot
		writeJSONFile(t, filepath.Join(town, "mayor", "rigs.json"), map[string]any{
			"version": 1, "rigs": map[string]any{"alpha": map[string]any{}},
		})
		// The town's rig registry makes the bead-reading collectors ask for
		// alpha's beads; the fake answers them so the tick starts no bd.
		f.d.openWorkBeads = (&labelBeads{}).open
		path := writeRigSettingsFile(t, town, "alpha", rigForgejoSettings(origin))
		tr := &forgejoStubTransport{status: http.StatusOK}
		f.src.host = forgejoProbeHost{
			fakeHost: healthyHost(),
			health:   systemHostProbes{forgejoBaseURL: f.d.townForgejoBaseURL, forgejoHTTP: &http.Client{Transport: tr}},
		}
		return f, path, tr
	}
	wakeCount := func(t *testing.T, f *attentionFixture) int {
		t.Helper()
		n := 0
		for _, e := range f.events(t) {
			if e.Wake {
				n++
			}
		}
		return n
	}

	for _, bc := range breaks {
		t.Run(bc.name, func(t *testing.T) {
			t.Parallel()
			f, path, tr := fixture(t, "https://forgejo.example/acme/rig.git")
			// The instance stops answering, so the alarm opens.
			tr.status = http.StatusServiceUnavailable
			var st attention.State
			for beat := 1; beat <= attentionForgejoProbeDownBeats; beat++ {
				st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
			}
			itemByKey(t, st, hostRuleForgejoDown)
			if got := wakeCount(t, f); got != 1 {
				t.Fatalf("wakes = %d, want the one raise", got)
			}

			// The instance answers again and the setting breaks: the alarm is
			// kept open, because the town's origin is now unknown rather than
			// absent.
			tr.status = http.StatusOK
			bc.break_(t, path)
			at := now.Add(time.Duration(attentionForgejoProbeDownBeats+1) * time.Minute)
			st = f.tick(t, at)
			down := itemByKey(t, st, hostRuleForgejoDown)
			if !strings.Contains(down.Summary, forgejoUnreadableReason) || !strings.Contains(down.Summary, "rig alpha") {
				t.Errorf("summary = %q, want the reason and the rig", down.Summary)
			}
			itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)
			if got := wakeCount(t, f); got != 1 {
				t.Errorf("wakes = %d, want no new raise for an unreadable setting", got)
			}

			// Kept again on the next tick, the reason is replaced, not stacked.
			st = f.tick(t, at.Add(time.Minute))
			if again := itemByKey(t, st, hostRuleForgejoDown); strings.Count(again.Summary, forgejoUnreadableReason) != 1 {
				t.Errorf("summary = %q, want the reason said once", again.Summary)
			}

			// A setting that reads and names no Forgejo clears it.
			writeRigSettingsFile(t, f.d.config.TownRoot, "alpha", rigForgejoSettings(""))
			st = f.tick(t, at.Add(2*time.Minute))
			if items := hostItems(st); len(items) != 0 {
				t.Errorf("host items = %v, want cleared by a setting that names no Forgejo", hostKeys(st))
			}
		})
	}

	t.Run("never raises a new alarm", func(t *testing.T) {
		t.Parallel()
		f, path, _ := fixture(t, "https://forgejo.example/acme/rig.git")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		for beat := 1; beat <= attentionForgejoProbeDownBeats+1; beat++ {
			st := f.tick(t, now.Add(time.Duration(beat)*time.Minute))
			if _, ok := attention.Find(st, hostRuleForgejoDown); ok {
				t.Fatalf("beat %d raised %s for a setting it could not read: %v", beat, hostRuleForgejoDown, hostKeys(st))
			}
			itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)
		}
		if got := wakeCount(t, f); got != 0 {
			t.Errorf("wakes = %d, want none for an unreadable setting with no alarm open", got)
		}
	})
}

// An empty or unreadable rig list is not a town without Forgejo: with the
// alarm open it stays open, its summary says the list could not be read, and
// nothing new is raised; a list read cleanly whose rigs name no Forgejo still
// clears it (gt-gg9hv.36).
func TestHostForgejoEmptyRigListKeepsTheAlarm(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	fixture := func(t *testing.T) (*attentionFixture, string, *forgejoStubTransport) {
		t.Helper()
		f := newAttentionFixture(t, now)
		town := f.d.config.TownRoot
		registry := filepath.Join(town, "mayor", "rigs.json")
		writeJSONFile(t, registry, map[string]any{
			"version": 1, "rigs": map[string]any{"alpha": map[string]any{}},
		})
		// The rig registry makes the bead-reading collectors ask for alpha's
		// beads; the fake answers them so the tick starts no bd.
		f.d.openWorkBeads = (&labelBeads{}).open
		writeRigSettingsFile(t, town, "alpha", rigForgejoSettings("https://forgejo.example/acme/rig.git"))
		tr := &forgejoStubTransport{status: http.StatusOK}
		f.src.host = forgejoProbeHost{
			fakeHost: healthyHost(),
			health:   systemHostProbes{forgejoBaseURL: f.d.townForgejoBaseURL, forgejoHTTP: &http.Client{Transport: tr}},
		}
		return f, registry, tr
	}
	wakes := func(t *testing.T, f *attentionFixture) int {
		t.Helper()
		n := 0
		for _, e := range f.events(t) {
			if e.Wake {
				n++
			}
		}
		return n
	}
	// breaks are the rig lists that read as unknown: one that is empty and one
	// the run cannot read.
	breaks := []struct {
		name   string
		break_ func(t *testing.T, town, registry string)
	}{
		{"an empty rig list", func(t *testing.T, _, registry string) {
			t.Helper()
			writeJSONFile(t, registry, map[string]any{"version": 1, "rigs": map[string]any{}})
		}},
		{"a rig list that does not parse", func(t *testing.T, town, registry string) {
			t.Helper()
			writeTownFile(t, town, filepath.Join("mayor", "rigs.json"), "{not json")
		}},
	}
	for _, bc := range breaks {
		t.Run(bc.name, func(t *testing.T) {
			t.Parallel()
			f, registry, tr := fixture(t)
			// The instance stops answering, so the alarm opens.
			tr.status = http.StatusServiceUnavailable
			var st attention.State
			for beat := 1; beat <= attentionForgejoProbeDownBeats; beat++ {
				st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
			}
			itemByKey(t, st, hostRuleForgejoDown)
			if got := wakes(t, f); got != 1 {
				t.Fatalf("wakes = %d, want the one raise", got)
			}

			// The instance answers again, but the rig list now reads as unknown:
			// the alarm is kept, because the town's origin is unknown rather
			// than absent.
			tr.status = http.StatusOK
			bc.break_(t, f.d.config.TownRoot, registry)
			f.d.invalidateKnownRigsCache()
			at := now.Add(time.Duration(attentionForgejoProbeDownBeats+1) * time.Minute)
			st = f.tick(t, at)
			down := itemByKey(t, st, hostRuleForgejoDown)
			if !strings.Contains(down.Summary, forgejoUnreadableReason) || !strings.Contains(down.Summary, "rig list is unreadable or empty") {
				t.Errorf("summary = %q, want the reason and the rig list", down.Summary)
			}
			itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)
			if got := wakes(t, f); got != 1 {
				t.Errorf("wakes = %d, want no new raise for an unreadable rig list", got)
			}

			// A rig list read cleanly whose rigs name no Forgejo clears it.
			writeJSONFile(t, registry, map[string]any{
				"version": 1, "rigs": map[string]any{"alpha": map[string]any{}},
			})
			writeRigSettingsFile(t, f.d.config.TownRoot, "alpha", rigForgejoSettings(""))
			f.d.invalidateKnownRigsCache()
			st = f.tick(t, at.Add(time.Minute))
			if items := hostItems(st); len(items) != 0 {
				t.Errorf("host items = %v, want cleared by a rig list naming no Forgejo", hostKeys(st))
			}
		})
	}
}

// A remote URL can carry a credential in its user info, so a remote that does
// not parse puts neither the token nor the user info in the error, the alarm
// summary or the log: they name the rig and the field and, at most, the host.
func TestHostForgejoOriginParseErrorLeaksNoCredential(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	const token = "s3cr3t-token"
	const remote = "https://svc:" + token + "@forgejo.example:3000/acme/rig.git"
	// The same remote with a path no URL parse accepts.
	const broken = "https://svc:" + token + "@forgejo.example:3000/acme/rig%zz.git"

	f := newAttentionFixture(t, now)
	town := f.d.config.TownRoot
	writeJSONFile(t, filepath.Join(town, "mayor", "rigs.json"), map[string]any{
		"version": 1, "rigs": map[string]any{"alpha": map[string]any{}},
	})
	f.d.openWorkBeads = (&labelBeads{}).open
	path := writeRigSettingsFile(t, town, "alpha", rigForgejoSettings(broken))
	tr := &forgejoStubTransport{status: http.StatusOK}
	f.src.host = forgejoProbeHost{
		fakeHost: healthyHost(),
		health:   systemHostProbes{forgejoBaseURL: f.d.townForgejoBaseURL, forgejoHTTP: &http.Client{Transport: tr}},
	}
	// The setting names a remote no parse accepts from the first tick, so the
	// probe line it raises, and the line the tick logs for it, carry the reason.
	st := f.tick(t, now.Add(time.Minute))
	itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)

	// The remote parses and the instance stops answering, so the alarm opens.
	if err := os.WriteFile(path, []byte(rigForgejoSettings(remote)), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.status = http.StatusServiceUnavailable
	for beat := 2; beat <= attentionForgejoProbeDownBeats+1; beat++ {
		st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}
	itemByKey(t, st, hostRuleForgejoDown)

	// The setting becomes the unparseable remote again. The origin is unknown,
	// so the alarm is kept with the reason in its summary.
	if err := os.WriteFile(path, []byte(rigForgejoSettings(broken)), 0o644); err != nil {
		t.Fatal(err)
	}
	at := now.Add(time.Duration(attentionForgejoProbeDownBeats+2) * time.Minute)
	st = f.tick(t, at)

	_, err := f.d.townForgejoBaseURL()
	if err == nil {
		t.Fatal("townForgejoBaseURL over an unparseable remote = nil, want an error")
	}
	down := itemByKey(t, st, hostRuleForgejoDown)
	probe := itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)
	for _, c := range []struct{ name, text string }{
		{"the error", err.Error()},
		{"the alarm", down.Summary},
		{"the probe", probe.Summary},
		{"the log", f.logs.String()},
	} {
		if !strings.Contains(c.text, "remote origin for rig alpha is unparseable") {
			t.Errorf("%s = %q, want it to name the rig and the field", c.name, c.text)
		}
		if strings.Contains(c.text, token) || strings.Contains(c.text, "svc:") {
			t.Errorf("%s = %q, want no credential", c.name, c.text)
		}
	}
	if !strings.Contains(err.Error(), "forgejo.example:3000") {
		t.Errorf("error = %q, want the host, the most the parse error may carry", err)
	}
}
