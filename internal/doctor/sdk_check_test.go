package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// malformedStub is the head of MacOSX27.0.sdk's usr/lib/libresolv.tbd, the stub
// whose "arm64e.x1" targets made every cgo link fail (gt-1a0t). The list wraps,
// putting the offending token on the continuation line.
const malformedStub = `--- !tapi-tbd
tbd-version:     4
targets:         [ x86_64-macos, x86_64-maccatalyst, arm64e-macos, arm64e-maccatalyst,
                   arm64e.x1-macos, arm64e.x1-maccatalyst ]
install-name:    '/usr/lib/libresolv.9.dylib'
`

// wellFormedStub is the same stub from MacOSX26.5.sdk.
const wellFormedStub = `--- !tapi-tbd
tbd-version:     4
targets:         [ x86_64-macos, x86_64-maccatalyst, arm64e-macos, arm64e-maccatalyst ]
install-name:    '/usr/lib/libresolv.9.dylib'
`

// writeSDK builds a fake SDK rooted at dir with the given stubs under usr/lib.
func writeSDK(t *testing.T, dir string, stubs map[string]string) {
	t.Helper()
	libDir := filepath.Join(dir, "usr", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("creating fake SDK: %v", err)
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(libDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("writing stub %s: %v", name, err)
		}
	}
}

// brokenXcrun stands in for the xcrun fallback on a host where it fails, so a
// resolution that reaches it surfaces as an error rather than a pass.
func brokenXcrun() (string, error) { return "", errors.New("xcrun: broken stub") }

// noProcessEnv is a process environment with no variables set.
func noProcessEnv(string) string { return "" }

// macOSCheck is a MacOSSDKCheck on a host it treats as macOS whatever the test
// runs on, with a process environment that sets nothing and a broken xcrun.
// Run resolves the SDK through its production wiring over these two inputs.
func macOSCheck() *MacOSSDKCheck {
	c := NewMacOSSDKCheck()
	c.goos = "darwin"
	c.getenv = noProcessEnv
	c.xcrun = brokenXcrun
	return c
}

// sdkCheckAt is a macOSCheck whose process environment pins SDKROOT to path.
func sdkCheckAt(path string) *MacOSSDKCheck {
	c := macOSCheck()
	c.getenv = func(key string) string {
		if key == "SDKROOT" {
			return path
		}
		return ""
	}
	return c
}

// TestMacOSSDKCheck_NotApplicableOffMacOS pins the other branch: off macOS the
// check reports OK without resolving an SDK at all.
func TestMacOSSDKCheck_NotApplicableOffMacOS(t *testing.T) {
	t.Parallel()
	check := NewMacOSSDKCheck()
	check.goos = "linux"
	check.getenv = func(string) string {
		t.Error("environment read off macOS")
		return ""
	}
	check.xcrun = func() (string, error) {
		t.Error("xcrun called off macOS")
		return "", errors.New("unreachable")
	}

	result := check.Run(&CheckContext{})

	if result.Status != StatusOK || !strings.Contains(result.Message, "not applicable") {
		t.Fatalf("result = %v %q, want OK and not applicable", result.Status, result.Message)
	}
}

func TestMacOSSDKCheck_MalformedStubFails(t *testing.T) {
	t.Parallel()
	sdk := t.TempDir()
	writeSDK(t, sdk, map[string]string{
		"libresolv.tbd": malformedStub,
		"libz.tbd":      wellFormedStub, // a good stub beside a bad one must not mask it
	})
	check := sdkCheckAt(sdk)

	result := check.Run(&CheckContext{})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError for a stub declaring arm64e.x1", result.Status)
	}
	if !strings.Contains(result.Message, "1 stub(s)") {
		t.Errorf("Message = %q, want the offending stub count", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "usr/lib/libresolv.tbd") {
		t.Errorf("Details = %q, want the offending stub path", joined)
	}
	if !strings.Contains(joined, "arm64e.x1-macos") {
		t.Errorf("Details = %q, want the offending architecture token", joined)
	}
	if !strings.Contains(result.FixHint, "settings/daemon.env") {
		t.Errorf("FixHint = %q, want the remedy (pin SDKROOT)", result.FixHint)
	}
}

func TestMacOSSDKCheck_WellFormedStubsAreOK(t *testing.T) {
	t.Parallel()
	sdk := t.TempDir()
	writeSDK(t, sdk, map[string]string{"libresolv.tbd": wellFormedStub, "libSystem.tbd": wellFormedStub})
	check := sdkCheckAt(sdk)

	result := check.Run(&CheckContext{})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details: %v", result.Status, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "SDKROOT") {
		t.Errorf("Details = %v, want the resolution source", result.Details)
	}
}

// writeDaemonEnv writes settings/daemon.env under town with the given SDKROOT.
func writeDaemonEnv(t *testing.T, town, sdkRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(config.DaemonEnvPath(town)), 0o755); err != nil {
		t.Fatalf("creating settings dir: %v", err)
	}
	if err := os.WriteFile(config.DaemonEnvPath(town), []byte("SDKROOT="+sdkRoot+"\n"), 0o644); err != nil {
		t.Fatalf("writing daemon.env: %v", err)
	}
}

// TestMacOSSDKCheck_HonorsDaemonEnvSDKROOT pins the remedy sdkFixHint names:
// an SDKROOT set in settings/daemon.env. The check runs in the operator's
// shell, which never loads that file, so it must read the pin itself. The
// process environment is empty and xcrun is broken, so an OK result can only
// have come from the pin.
func TestMacOSSDKCheck_HonorsDaemonEnvSDKROOT(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	sdk := t.TempDir()
	writeSDK(t, sdk, map[string]string{"libresolv.tbd": wellFormedStub})
	writeDaemonEnv(t, town, sdk)
	check := macOSCheck()

	result := check.Run(&CheckContext{TownRoot: town})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for the SDK pinned in daemon.env; message %q, details %v",
			result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "settings/daemon.env") {
		t.Errorf("Details = %v, want the resolution source daemon.env", result.Details)
	}
}

// TestMacOSSDKCheck_ProcessSDKROOTWinsOverDaemonEnv pins precedence: an SDKROOT
// in the process environment beats the one in daemon.env, as the daemon's
// own environment does.
func TestMacOSSDKCheck_ProcessSDKROOTWinsOverDaemonEnv(t *testing.T) {
	t.Parallel()
	good := t.TempDir()
	writeSDK(t, good, map[string]string{"libresolv.tbd": wellFormedStub})
	bad := t.TempDir()
	writeSDK(t, bad, map[string]string{"libresolv.tbd": malformedStub})
	town := t.TempDir()
	writeDaemonEnv(t, town, bad)
	processEnv := func(key string) string {
		if key == "SDKROOT" {
			return good
		}
		return ""
	}
	check := macOSCheck()
	check.getenv = processEnv

	result := check.Run(&CheckContext{TownRoot: town})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK from the process SDKROOT; message %q", result.Status, result.Message)
	}
}

// TestMacOSSDKCheck_UnreadableDaemonEnvIsSkipped pins the LoadDaemonEnv error
// branch of resolution: a daemon.env that cannot be loaded leaves the pinned
// SDK unknown, so the check reports it could not measure rather than falling
// through to xcrun and judging a different SDK than the daemon's builds use.
func TestMacOSSDKCheck_UnreadableDaemonEnvIsSkipped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, town string)
		want    string
	}{
		{
			name: "malformed line",
			prepare: func(t *testing.T, town string) {
				if err := os.MkdirAll(filepath.Dir(config.DaemonEnvPath(town)), 0o755); err != nil {
					t.Fatalf("creating settings dir: %v", err)
				}
				if err := os.WriteFile(config.DaemonEnvPath(town), []byte("SDKROOT /sdk\n"), 0o644); err != nil {
					t.Fatalf("writing daemon.env: %v", err)
				}
			},
			want: "missing '='",
		},
		{
			name: "path is a directory",
			prepare: func(t *testing.T, town string) {
				if err := os.MkdirAll(config.DaemonEnvPath(town), 0o755); err != nil {
					t.Fatalf("creating daemon.env directory: %v", err)
				}
			},
			want: "reading daemon env file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tc.prepare(t, town)
			check := macOSCheck()

			result := check.Run(&CheckContext{TownRoot: town})

			if result.Status != StatusSkipped {
				t.Fatalf("Status = %v, want StatusSkipped; message %q", result.Status, result.Message)
			}
			if !strings.HasPrefix(result.Message, "unknown:") {
				t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
			}
			if len(result.Details) == 0 || !strings.Contains(result.Details[0], tc.want) {
				t.Errorf("Details = %v, want the load error containing %q", result.Details, tc.want)
			}
		})
	}
}

func TestMacOSSDKCheck_MissingSDKErrors(t *testing.T) {
	t.Parallel()
	// A stale SDKROOT outlives the SDK it names: renaming the offending SDK
	// leaves every build pointed at a deleted path.
	check := sdkCheckAt(filepath.Join(t.TempDir(), "MacOSX27.0.sdk"))

	result := check.Run(&CheckContext{})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError for a missing SDK", result.Status)
	}
	if !strings.Contains(result.Message, "is missing") {
		t.Errorf("Message = %q, want it to report the SDK as missing", result.Message)
	}
	if !strings.Contains(result.FixHint, "settings/daemon.env") {
		t.Errorf("FixHint = %q, want the remedy", result.FixHint)
	}
}

func TestMacOSSDKCheck_UnresolvableIsSkipped(t *testing.T) {
	t.Parallel()
	check := macOSCheck()
	check.xcrun = func() (string, error) { return "", errors.New("xcrun: command not found") }

	result := check.Run(&CheckContext{})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped (couldn't measure, not a clean pass)", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "xcrun: command not found") {
		t.Errorf("Details = %v, want the underlying error", result.Details)
	}
}

func TestMacOSSDKCheck_NoStubsIsSkipped(t *testing.T) {
	t.Parallel()
	// An SDK with no stub tree was not judged, so it must not pass as clean.
	sdk := t.TempDir()
	check := macOSCheck()
	check.xcrun = func() (string, error) { return sdk, nil }

	result := check.Run(&CheckContext{})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped when nothing was read", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestMacOSSDKCheck_ReportsAtMostMaxStubs(t *testing.T) {
	t.Parallel()
	sdk := t.TempDir()
	stubs := map[string]string{}
	for _, name := range []string{"a.tbd", "b.tbd", "c.tbd", "d.tbd", "e.tbd", "f.tbd", "g.tbd"} {
		stubs[name] = malformedStub
	}
	writeSDK(t, sdk, stubs)
	check := sdkCheckAt(sdk)

	result := check.Run(&CheckContext{})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError", result.Status)
	}
	if !strings.Contains(result.Message, "7 stub(s)") {
		t.Errorf("Message = %q, want all seven counted", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "and 2 more stubs") {
		t.Errorf("Details = %q, want the remainder summarised", joined)
	}
	if strings.Count(joined, "usr/lib/") != maxReportedStubs {
		t.Errorf("Details list %d stubs, want %d", strings.Count(joined, "usr/lib/"), maxReportedStubs)
	}
}

func TestStubTopLevelTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		stub string
		want []string
	}{
		{
			name: "wrapped list keeps the continuation line",
			stub: malformedStub,
			want: []string{
				"x86_64-macos", "x86_64-maccatalyst", "arm64e-macos", "arm64e-maccatalyst",
				"arm64e.x1-macos", "arm64e.x1-maccatalyst",
			},
		},
		{
			name: "single line list",
			stub: wellFormedStub,
			want: []string{"x86_64-macos", "x86_64-maccatalyst", "arm64e-macos", "arm64e-maccatalyst"},
		},
		{
			name: "no targets key",
			stub: "--- !tapi-tbd\ntbd-version:     4\n",
			want: nil,
		},
		{
			name: "unterminated list",
			stub: "targets:         [ arm64-macos\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stubTopLevelTargets(tc.stub)
			if len(got) != len(tc.want) {
				t.Fatalf("stubTopLevelTargets() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("target %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMalformedTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target string
		want   bool
	}{
		{"arm64e.x1-macos", true},
		{"arm64e.x1-maccatalyst", true},
		{"x86_64-macos", false},
		{"arm64e-maccatalyst", false},
		{"arm64-macos", false},
		{"x86_64h-macos", false},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			got := malformedTargets([]string{tc.target})
			if (len(got) > 0) != tc.want {
				t.Fatalf("malformedTargets(%q) = %v, want malformed=%v", tc.target, got, tc.want)
			}
		})
	}
}
