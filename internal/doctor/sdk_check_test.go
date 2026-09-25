package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// stubResolveSDK points the check at an SDK path and source for the test's duration.
func stubResolveSDK(t *testing.T, path, source string, err error) {
	t.Helper()
	orig := resolveSDK
	t.Cleanup(func() { resolveSDK = orig })
	resolveSDK = func() (string, string, error) { return path, source, err }
}

func TestMacOSSDKCheck_MalformedStubFails(t *testing.T) {
	sdk := t.TempDir()
	writeSDK(t, sdk, map[string]string{
		"libresolv.tbd": malformedStub,
		"libz.tbd":      wellFormedStub, // a good stub beside a bad one must not mask it
	})
	stubResolveSDK(t, sdk, "SDKROOT", nil)

	result := NewMacOSSDKCheck().Run(&CheckContext{})

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
	sdk := t.TempDir()
	writeSDK(t, sdk, map[string]string{"libresolv.tbd": wellFormedStub, "libSystem.tbd": wellFormedStub})
	stubResolveSDK(t, sdk, "SDKROOT", nil)

	result := NewMacOSSDKCheck().Run(&CheckContext{})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details: %v", result.Status, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "SDKROOT") {
		t.Errorf("Details = %v, want the resolution source", result.Details)
	}
}

func TestMacOSSDKCheck_MissingSDKErrors(t *testing.T) {
	// A stale SDKROOT outlives the SDK it names: renaming the offending SDK
	// leaves every build pointed at a deleted path.
	stubResolveSDK(t, filepath.Join(t.TempDir(), "MacOSX27.0.sdk"), "SDKROOT", nil)

	result := NewMacOSSDKCheck().Run(&CheckContext{})

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
	stubResolveSDK(t, "", "", errors.New("xcrun: command not found"))

	result := NewMacOSSDKCheck().Run(&CheckContext{})

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
	// An SDK with no stub tree was not judged, so it must not pass as clean.
	stubResolveSDK(t, t.TempDir(), "xcrun", nil)

	result := NewMacOSSDKCheck().Run(&CheckContext{})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped when nothing was read", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestMacOSSDKCheck_ReportsAtMostMaxStubs(t *testing.T) {
	sdk := t.TempDir()
	stubs := map[string]string{}
	for _, name := range []string{"a.tbd", "b.tbd", "c.tbd", "d.tbd", "e.tbd", "f.tbd", "g.tbd"} {
		stubs[name] = malformedStub
	}
	writeSDK(t, sdk, stubs)
	stubResolveSDK(t, sdk, "SDKROOT", nil)

	result := NewMacOSSDKCheck().Run(&CheckContext{})

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
