package rigbootstrap

import (
	"errors"
	"testing"
)

func TestParseOptionsTable(t *testing.T) {
	t.Parallel()
	required := []string{"--town-root", "/town", "--rig", "nr", "--local-repo", "/repo"}
	withRequired := func(extra ...string) []string {
		return append(append([]string{}, required...), extra...)
	}

	tests := []struct {
		name     string
		args     []string
		want     Options
		wantErr  string // message of the *UsageError; empty when none is expected
		wantHelp bool
	}{
		{
			name: "all flags",
			args: []string{
				"--town-root", "/town", "--rig", "nightrider", "--remote", "file:///x",
				"--local-repo", "/repo", "--gt-bin", "/bin/gt", "--prefix", "nr",
				"--branch", "main", "--polecat-agent", "claude",
			},
			want: Options{
				TownRoot: "/town", Rig: "nightrider", Remote: "file:///x",
				LocalRepo: "/repo", GTBin: "/bin/gt", Prefix: "nr",
				Branch: "main", PolecatAgent: "claude",
			},
		},
		{
			name: "required only leaves optional flags empty and gt on PATH",
			args: required,
			want: Options{TownRoot: "/town", Rig: "nr", LocalRepo: "/repo", GTBin: "gt"},
		},
		{
			name:    "no arguments names TOWN_ROOT first",
			args:    nil,
			wantErr: "Missing required argument: TOWN_ROOT",
		},
		{
			name:    "only local repo names TOWN_ROOT before RIG_NAME",
			args:    []string{"--local-repo", "/repo"},
			wantErr: "Missing required argument: TOWN_ROOT",
		},
		{
			name:    "town root only names RIG_NAME before LOCAL_REPO",
			args:    []string{"--town-root", "/town"},
			wantErr: "Missing required argument: RIG_NAME",
		},
		{
			name:    "town root and rig name only names LOCAL_REPO",
			args:    []string{"--town-root", "/town", "--rig", "nr"},
			wantErr: "Missing required argument: LOCAL_REPO",
		},
		{
			name:    "explicit empty value is missing",
			args:    []string{"--town-root", "", "--rig", "nr", "--local-repo", "/repo"},
			wantErr: "Missing required argument: TOWN_ROOT",
		},
		{
			name:    "trailing required flag with no value is missing",
			args:    []string{"--town-root", "/town", "--rig", "nr", "--local-repo"},
			wantErr: "Missing required argument: LOCAL_REPO",
		},
		{
			name: "trailing optional flag with no value is empty",
			args: withRequired("--prefix"),
			want: Options{TownRoot: "/town", Rig: "nr", LocalRepo: "/repo", GTBin: "gt"},
		},
		{
			name:    "unknown argument",
			args:    withRequired("--bogus"),
			wantErr: "Unknown argument: --bogus",
		},
		{
			name:     "-h asks for help",
			args:     []string{"-h"},
			wantHelp: true,
		},
		{
			name:     "--help asks for help even with required values missing",
			args:     []string{"--help"},
			wantHelp: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOptions(tt.args)
			if tt.wantHelp {
				if !errors.Is(err, ErrHelp) {
					t.Fatalf("ParseOptions(%q) error = %v, want ErrHelp", tt.args, err)
				}
				return
			}
			if tt.wantErr != "" {
				var usage *UsageError
				if !errors.As(err, &usage) {
					t.Fatalf("ParseOptions(%q) error = %v, want a UsageError %q", tt.args, err, tt.wantErr)
				}
				if usage.Msg != tt.wantErr {
					t.Fatalf("ParseOptions(%q) message = %q, want %q", tt.args, usage.Msg, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOptions(%q) error = %v, want none", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("ParseOptions(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRetiredAgentFlagsRefused(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--witness-agent", "--refinery-agent"} {
		t.Run(flag, func(t *testing.T) {
			args := []string{"--town-root", "/town", "--rig", "nr", "--local-repo", "/repo", flag, "codex"}
			_, err := ParseOptions(args)
			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("ParseOptions(%q) error = %v, want a UsageError", args, err)
			}
			want := flag + " is not supported: the witness and refinery roles are retired"
			if usage.Msg != want {
				t.Fatalf("ParseOptions(%q) message = %q, want %q", args, usage.Msg, want)
			}
		})
	}
}
