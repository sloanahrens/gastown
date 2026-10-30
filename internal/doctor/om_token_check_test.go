package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

const (
	omSidecar   = "/etc/gt/om-token.json"
	omTokenPath = "/etc/gt/om-token"
	// omSentinel stands in for token text: it is placed in files the check must
	// not read or echo, and is not a secret.
	omSentinel = "SENTINEL-om-token-text-0000"
)

var omNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// omRig is the check over fakes: the settings, the files it may read (and a log
// of every path it asked for), and the clock.
type omRig struct {
	cfg      *config.DaemonPatrolConfig
	cfgErr   error
	files    map[string]string
	reads    []string
	checkRun *OMTokenExpiryCheck
}

func newOMRig(meta string, sidecar string) *omRig {
	r := &omRig{
		cfg:   &config.DaemonPatrolConfig{OMTokenMetaSetting: meta},
		files: map[string]string{omTokenPath: omSentinel, omSidecar: sidecar},
	}
	r.checkRun = &OMTokenExpiryCheck{
		settings: func(string) (*config.DaemonPatrolConfig, error) { return r.cfg, r.cfgErr },
		readFile: func(path string) ([]byte, error) {
			r.reads = append(r.reads, path)
			text, ok := r.files[path]
			if !ok {
				return nil, errors.New("open " + path + ": no such file or directory")
			}
			return []byte(text), nil
		},
		now: func() time.Time { return omNow },
	}
	return r
}

func (r *omRig) run() *CheckResult {
	return r.checkRun.Run(&CheckContext{TownRoot: "/town"})
}

func expiresIn(d time.Duration) string {
	return `{"expires_at": "` + omNow.Add(d).Format(time.RFC3339) + `"}`
}

// output is every string the result carries, for a check that no token text
// reaches any of them.
func output(r *CheckResult) string {
	return r.Name + "\n" + r.Message + "\n" + r.FixHint + "\n" + strings.Join(r.Details, "\n")
}

// With no daemon.om_token_meta the check is not applicable, and reads no file.
func TestOMTokenExpiryNotApplicableWithoutTheSetting(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*config.DaemonPatrolConfig{
		"no settings file":  nil,
		"the setting empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newOMRig("", expiresIn(100*24*time.Hour))
			r.cfg = cfg
			got := r.run()
			if got.Status != StatusSkipped {
				t.Errorf("status = %v (%s), want skipped", got.Status, got.Message)
			}
			if len(r.reads) != 0 {
				t.Errorf("read %v, want no file read", r.reads)
			}
		})
	}
}

// 31 days before expires_at it is ok and 30 days before it warns, on the
// injected clock; an expiry in the past is an error, and so is the very moment.
func TestOMTokenExpiryStates(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	for _, tc := range []struct {
		name    string
		left    time.Duration
		want    CheckStatus
		message string
	}{
		{"31 days left", 31 * day, StatusOK, "(31 days left)"},
		{"30 days and a second left", 30*day + time.Second, StatusOK, "30 days left"},
		{"exactly 30 days left", 30 * day, StatusWarning, "(30 days left)"},
		{"one day left", day, StatusWarning, "(1 days left)"},
		{"an hour left", time.Hour, StatusWarning, "(0 days left)"},
		{"expiring now", 0, StatusError, "expired on"},
		{"expired 3 days ago", -3 * day, StatusError, "(3 days ago)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newOMRig(omSidecar, expiresIn(tc.left))
			got := r.run()
			if got.Status != tc.want || !strings.Contains(got.Message, tc.message) {
				t.Errorf("status %v, message %q; want %v and %q", got.Status, got.Message, tc.want, tc.message)
			}
			if !strings.Contains(got.Message, omNow.Add(tc.left).UTC().Format(time.DateOnly)) {
				t.Errorf("message %q names no expiry date", got.Message)
			}
		})
	}
}

// A date with no time is that day's start in UTC.
func TestOMTokenExpiryAcceptsADateOnly(t *testing.T) {
	t.Parallel()
	r := newOMRig(omSidecar, `{"expires_at": "2026-11-20"}`)
	got := r.run()
	if got.Status != StatusWarning || !strings.Contains(got.Message, "2026-11-20") || !strings.Contains(got.Message, "(20 days left)") {
		t.Errorf("result = %v %q, want a warning dated 2026-11-20 with 20 days left (the date is its start, UTC)", got.Status, got.Message)
	}
}

// A missing sidecar, one that is not JSON, one with no expires_at and one with
// an expires_at that parses as neither format each warn, and none echoes the
// file's content.
func TestOMTokenExpiryWarnsWhenTheExpiryCannotBeRead(t *testing.T) {
	t.Parallel()
	for name, sidecar := range map[string]string{
		"not JSON":              omSentinel,
		"no expires_at":         `{"note": "` + omSentinel + `"}`,
		"an empty expires_at":   `{"expires_at": ""}`,
		"an unparseable date":   `{"expires_at": "` + omSentinel + `"}`,
		"a number for the date": `{"expires_at": 1793000000}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newOMRig(omSidecar, sidecar)
			got := r.run()
			if got.Status != StatusWarning || !strings.Contains(got.Message, "no parseable expires_at") {
				t.Errorf("result = %v %q, want a warning about the unparseable expires_at", got.Status, got.Message)
			}
			if strings.Contains(output(got), omSentinel) {
				t.Error("the result echoes the sidecar's content")
			}
		})
	}
	t.Run("the sidecar is missing", func(t *testing.T) {
		t.Parallel()
		r := newOMRig(omSidecar, "")
		delete(r.files, omSidecar)
		got := r.run()
		if got.Status != StatusWarning || !strings.Contains(got.Message, omSidecar) {
			t.Errorf("result = %v %q, want a warning naming the sidecar path", got.Status, got.Message)
		}
	})
}

// A relative sidecar path is not read, and unreadable settings warn.
func TestOMTokenExpiryRefusesWhatItCannotTrust(t *testing.T) {
	t.Parallel()
	r := newOMRig("relative/om-token.json", expiresIn(100*24*time.Hour))
	if got := r.run(); got.Status != StatusWarning || len(r.reads) != 0 {
		t.Errorf("relative path: %v %q reads %v, want a warning and no read", got.Status, got.Message, r.reads)
	}
	r = newOMRig(omSidecar, expiresIn(100*24*time.Hour))
	r.cfgErr = errors.New("settings do not parse")
	if got := r.run(); got.Status != StatusWarning || len(r.reads) != 0 {
		t.Errorf("unreadable settings: %v %q reads %v, want a warning and no read", got.Status, got.Message, r.reads)
	}
}

// The check opens the sidecar and nothing else: a token file holding a sentinel
// is never read, and the sentinel is in no output of any state.
func TestOMTokenExpiryNeverReadsTheTokenFile(t *testing.T) {
	t.Parallel()
	for _, left := range []time.Duration{100 * 24 * time.Hour, 10 * 24 * time.Hour, -24 * time.Hour} {
		r := newOMRig(omSidecar, expiresIn(left))
		got := r.run()
		if len(r.reads) != 1 || r.reads[0] != omSidecar {
			t.Errorf("left %s: read %v, want the sidecar alone", left, r.reads)
		}
		if strings.Contains(output(got), omSentinel) {
			t.Errorf("left %s: the sentinel is in the output", left)
		}
	}
}

func TestOMTokenExpiryCheckIsNamed(t *testing.T) {
	t.Parallel()
	c := NewOMTokenExpiryCheck()
	if c.Name() != "om-token-expiry" || c.Description() == "" || c.CanFix() {
		t.Errorf("check = %q %q fix=%v, want om-token-expiry, described and unfixable", c.Name(), c.Description(), c.CanFix())
	}
}
