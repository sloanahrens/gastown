package beads

import (
	"fmt"
	"strings"
	"testing"
)

const bdUpdateHelpWithGuard = `Update an issue

Usage:
  bd update <id> [flags]

Flags:
      --acceptance string      Replace the acceptance block
      --if-acceptance string   Only write while the stored acceptance equals this (text or sha256)
  -h, --help                   help for update
      --if-status string       Only update while the status equals this
`

const bdUpdateHelpWithoutGuard = `Update an issue

Usage:
  bd update <id> [flags]

Flags:
      --acceptance string      Replace the acceptance block
  -h, --help                   help for update
      --if-status string       Only update while the status equals this (see --if-acceptance in newer bd)
`

// TestHelpOffersFlag: the probe reads a flag from bd's help table and nothing
// else: a flag named only in prose, or a longer flag with the same prefix, is
// not the flag.
func TestHelpOffersFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		help string
		flag string
		want bool
	}{
		{"flag in the table", bdUpdateHelpWithGuard, "--if-acceptance", true},
		{"flag named only in prose", bdUpdateHelpWithoutGuard, "--if-acceptance", false},
		{"a different flag", bdUpdateHelpWithGuard, "--if-assignee", false},
		{"a prefix of a longer flag", bdUpdateHelpWithGuard, "--if", false},
		{"long flag beside its short form", bdUpdateHelpWithGuard, "--help", true},
	} {
		if got := HelpOffersFlag([]byte(tc.help), tc.flag); got != tc.want {
			t.Errorf("%s: HelpOffersFlag(%q) = %v, want %v", tc.name, tc.flag, got, tc.want)
		}
	}
}

// TestHelpOffersVerb: the probe reads a command from bd's help table and
// nothing else: a verb named only in prose, or a longer verb with the same
// prefix, is not the verb.
func TestHelpOffersVerb(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		help string
		verb string
		want bool
	}{
		{"verb in the table", bdHelpWithSampleVerb, "sample", true},
		{"verb absent", bdHelpWithoutSampleVerb, "sample", false},
		{"longer verb with the prefix", bdHelpWithSampleVerb, "samp", false},
		{"verb named only in prose", "Usage:\n  bd [command]\n\nSee bd sample add for cost rows.\n", "sample", false},
	} {
		if got := HelpOffersVerb([]byte(tc.help), tc.verb); got != tc.want {
			t.Errorf("%s: HelpOffersVerb(%q) = %v, want %v", tc.name, tc.verb, got, tc.want)
		}
	}
}

// TestSampleVerbProbeAsksBdHelp: the typed probe runs bd --help and answers
// from it, for a bd with the verb and one without; a bd that does not answer
// is an error, not a false.
func TestSampleVerbProbeAsksBdHelp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		help string
		fail bool
		want bool
	}{
		{"a bd with the verb", bdHelpWithSampleVerb, false, true},
		{"a bd without the verb", bdHelpWithoutSampleVerb, false, false},
		{"a bd that does not answer", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				if strings.Join(args, " ") == "--help" {
					if tc.fail {
						return reply{err: fmt.Errorf("bd: exit status 1")}
					}
					return reply{stdout: tc.help}
				}
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			got, err := b.sampleOffersVerb()
			if tc.fail {
				if err == nil {
					t.Fatalf("sampleOffersVerb = %v, nil; want the bd failure returned", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("sampleOffersVerb = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			if calls := r.argvs(); len(calls) != 1 || calls[0] != "--help" {
				t.Errorf("bd calls = %v, want one \"--help\"", calls)
			}
		})
	}
}

// TestUpdateOffersFlagAsksBdUpdateHelp: the typed probe runs bd update --help
// and answers from it, for a bd with the flag and one without; a bd that does
// not answer is an error, not a false.
func TestUpdateOffersFlagAsksBdUpdateHelp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		help string
		fail bool
		want bool
	}{
		{"a bd with the flag", bdUpdateHelpWithGuard, false, true},
		{"a bd without the flag", bdUpdateHelpWithoutGuard, false, false},
		{"a bd that does not answer", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				if strings.Join(args, " ") == "update --help" {
					if tc.fail {
						return reply{err: fmt.Errorf("bd: exit status 1")}
					}
					return reply{stdout: tc.help}
				}
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			got, err := b.UpdateOffersFlag("--if-acceptance")
			if tc.fail {
				if err == nil {
					t.Fatalf("UpdateOffersFlag = %v, nil; want the bd failure returned", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("UpdateOffersFlag = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			if calls := r.calls(); len(calls) != 1 || strings.Join(calls[0].args, " ") != "update --help" {
				t.Errorf("bd calls = %v, want one \"update --help\"", r.argvs())
			}
		})
	}
}
