package bumpversion

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestValidateVersionTable: the script's regex accepts MAJOR.MINOR.PATCH and
// nothing else. The v-prefixed and four-part rows fail only because both
// anchors are kept.
func TestValidateVersionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version string
		ok      bool
	}{
		{name: "zero version", version: "0.2.0", ok: true},
		{name: "one dot oh", version: "1.0.0", ok: true},
		{name: "multi digit", version: "10.20.30", ok: true},
		{name: "v prefix", version: "v1.0.0"},
		{name: "two parts", version: "1.0"},
		{name: "prerelease suffix", version: "1.0.0-rc1"},
		{name: "four parts", version: "1.0.0.0"},
		{name: "empty", version: ""},
		{name: "trailing newline", version: "1.0.0\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateVersion(tc.version)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateVersion(%q) = %v, want nil", tc.version, err)
				}
				return
			}
			var verr *VersionError
			if !errors.As(err, &verr) {
				t.Fatalf("ValidateVersion(%q) = %v, want a *VersionError", tc.version, err)
			}
			want := "Invalid version format '" + tc.version + "'\n" +
				"Expected semantic version format: MAJOR.MINOR.PATCH (e.g., 0.2.0)"
			if got := err.Error(); got != want {
				t.Errorf("ValidateVersion(%q) message = %q, want %q", tc.version, got, want)
			}
		})
	}
}

// TestParseVersionAndFlags: the version is the first argument and each flag
// sets only its own field.
func TestParseVersionAndFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want Options
	}{
		{
			name: "version only",
			args: []string{"0.2.0"},
			want: Options{Version: "0.2.0"},
		},
		{
			name: "every flag",
			args: []string{"0.2.0", "--commit", "--tag", "--install", "--allow-dirty"},
			want: Options{Version: "0.2.0", Commit: true, Tag: true, Install: true, AllowDirty: true},
		},
		{
			name: "install alone",
			args: []string{"1.2.3", "--install"},
			want: Options{Version: "1.2.3", Install: true},
		},
		{
			name: "allow dirty alone",
			args: []string{"1.2.3", "--allow-dirty"},
			want: Options{Version: "1.2.3", AllowDirty: true},
		},
		{
			name: "flags may repeat",
			args: []string{"1.2.3", "--commit", "--commit"},
			want: Options{Version: "1.2.3", Commit: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.args)
			if err != nil {
				t.Fatalf("Parse(%v) = %v, want no error", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseErrors: the empty command line, an unknown option and a bad version
// each fail with the type the command answers differently.
func TestParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr error  // sentinel checked with errors.Is, when the case has one
		wantMsg string // expected message; empty when wantErr is a sentinel
		usage   bool   // the error is a *UsageError, which the command follows with Usage
	}{
		{name: "no arguments", args: nil, wantErr: ErrUsage},
		{name: "unknown option", args: []string{"0.2.0", "--bogus"}, wantMsg: "Unknown option '--bogus'", usage: true},
		{name: "version flags before version", args: []string{"--commit"}, wantMsg: "Invalid version format '--commit'"},
		{name: "two-part version", args: []string{"1.0"}, wantMsg: "Invalid version format '1.0'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.args)
			if err == nil {
				t.Fatalf("Parse(%v) = no error, want a failure", tc.args)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Parse(%v) = %v, want %v", tc.args, err, tc.wantErr)
				}
				return
			}
			var usage *UsageError
			if isUsage := errors.As(err, &usage); isUsage != tc.usage {
				t.Fatalf("Parse(%v) = %T, want UsageError %v", tc.args, err, tc.usage)
			}
			if !strings.HasPrefix(err.Error(), tc.wantMsg) {
				t.Errorf("Parse(%v) = %q, want it to start with %q", tc.args, err, tc.wantMsg)
			}
		})
	}
}

// TestFlagDependencies: --tag needs --commit, and --push is refused with the
// reason it is not offered rather than quietly accepted.
func TestFlagDependencies(t *testing.T) {
	t.Parallel()
	t.Run("tag without commit is refused", func(t *testing.T) {
		t.Parallel()
		_, err := Parse([]string{"0.2.0", "--tag"})
		var refusal *Refusal
		if !errors.As(err, &refusal) || refusal.Msg != "--tag requires --commit" {
			t.Errorf("Parse(0.2.0 --tag) = %v, want the refusal --tag requires --commit", err)
		}
	})
	t.Run("commit tag install passes", func(t *testing.T) {
		t.Parallel()
		got, err := Parse([]string{"0.2.0", "--commit", "--tag", "--install"})
		if err != nil {
			t.Fatalf("Parse(0.2.0 --commit --tag --install) = %v, want no error", err)
		}
		if want := (Options{Version: "0.2.0", Commit: true, Tag: true, Install: true}); got != want {
			t.Errorf("Parse(0.2.0 --commit --tag --install) = %+v, want %+v", got, want)
		}
	})
	// The leading row is the one the script's version-first parse would report
	// as a bad version; the option is refused in that position too.
	for _, args := range [][]string{
		{"--push"},
		{"0.2.0", "--commit", "--tag", "--push"},
	} {
		t.Run("push is refused with its reason "+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			_, err := Parse(args)
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("Parse(%v) = %v, want a *Refusal", args, err)
			}
			for _, want := range []string{"--push is not offered", "landing worker"} {
				if !strings.Contains(refusal.Msg, want) {
					t.Errorf("the --push refusal %q does not say %q", refusal.Msg, want)
				}
			}
		})
	}
}

// TestUsageFirstLine: the first line is the command line an operator is meant
// to retype.
func TestUsageFirstLine(t *testing.T) {
	t.Parallel()
	want := "Usage: bump-version <version> [--commit] [--tag] [--install] [--allow-dirty]"
	if got := strings.SplitN(Usage, "\n", 2)[0]; got != want {
		t.Errorf("Usage starts %q, want %q", got, want)
	}
}

// TestUsageOmitsPush: the removed --push option is not advertised anywhere in
// the text, since the command refuses it.
func TestUsageOmitsPush(t *testing.T) {
	t.Parallel()
	if strings.Contains(Usage, "--push") {
		t.Errorf("Usage still advertises --push, which the port does not offer:\n%s", Usage)
	}
}
