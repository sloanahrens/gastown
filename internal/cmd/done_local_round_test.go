package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	localRoundTestRoundHead = "abcdef1234567890abcdef1234567890abcdef12"
	localRoundTestMovedHead = "9999999999999999999999999999999999999999"
)

var localRoundTestAt = time.Date(2026, 11, 13, 2, 30, 0, 0, time.UTC)

// localRoundTestBlock is round 1 on localRoundTestRoundHead, short as the
// daemon writes it, with the given findings.
func localRoundTestBlock(findings ...land.Finding) string {
	return land.FormatLocalRound(1, localRoundTestRoundHead[:7], findings, localRoundTestAt)
}

func localRoundTestFinding(id, severity string) land.Finding {
	return land.Finding{ID: id, Severity: severity, Path: "internal/x/a.go", Line: 12, Title: "unbounded poll loop"}
}

func TestDoneLocalRoundTable(t *testing.T) {
	t.Parallel()
	major := localRoundTestFinding("f1", "major")
	minor := localRoundTestFinding("f2", "minor")
	info := localRoundTestFinding("f4", "info")
	critical := localRoundTestFinding("f3", "critical")
	reply := land.FormatLocalRoundReply("f1", land.LocalRoundFixed, "bounded the loop")
	withReply := func(block string) string { return block + "\n" + reply }
	round2 := func(findings ...land.Finding) string {
		return land.FormatLocalRound(2, localRoundTestRoundHead[:7], findings, localRoundTestAt)
	}

	tests := []struct {
		name       string
		notes      string
		head       string
		readErr    error
		headErr    error
		wantRefuse []string
	}{
		{name: "no block allows", notes: "", head: localRoundTestMovedHead},
		{name: "unavailable block allows", notes: "LOCAL ROUND (1): unavailable - the daemon could not run the round", head: localRoundTestMovedHead},
		{name: "approving block allows", notes: localRoundTestBlock(), head: localRoundTestMovedHead},
		{
			name:       "major without a reply refuses naming the id",
			notes:      localRoundTestBlock(major),
			head:       localRoundTestMovedHead,
			wantRefuse: []string{"f1", "internal/x/a.go:12", "unbounded poll loop", "gt review-preview reply", "major or critical"},
		},
		{name: "major with a reply and a moved head allows", notes: withReply(localRoundTestBlock(major)), head: localRoundTestMovedHead},
		{
			name:       "major with a reply and the round's head refuses",
			notes:      withReply(localRoundTestBlock(major)),
			head:       localRoundTestRoundHead,
			wantRefuse: []string{"f1"},
		},
		{
			name:       "empty branch head refuses while a major blocks",
			notes:      localRoundTestBlock(major),
			head:       "",
			wantRefuse: []string{"branch head is empty"},
		},
		{name: "minors only allow", notes: localRoundTestBlock(minor), head: localRoundTestRoundHead},
		{name: "info only allows", notes: localRoundTestBlock(info), head: localRoundTestRoundHead},
		{
			name:       "critical without a reply refuses",
			notes:      localRoundTestBlock(critical),
			head:       localRoundTestMovedHead,
			wantRefuse: []string{"f3", "major or critical"},
		},
		{
			name:  "critical with a reply and a moved head allows",
			notes: localRoundTestBlock(critical) + "\n" + land.FormatLocalRoundReply("f3", land.LocalRoundDisputed, "not reachable"),
			head:  localRoundTestMovedHead,
		},
		{
			name:       "reply after round 1 does not answer a major of round 2",
			notes:      localRoundTestBlock(major) + "\n" + reply + "\n" + round2(major),
			head:       localRoundTestMovedHead,
			wantRefuse: []string{"f1", "LOCAL ROUND (2)"},
		},
		{
			name:  "reply after round 2 answers a major of round 2",
			notes: localRoundTestBlock(major) + "\n" + round2(major) + "\n" + reply,
			head:  localRoundTestMovedHead,
		},
		{
			name:       "last block that does not parse refuses",
			notes:      localRoundTestBlock(major) + "\nLOCAL ROUND (2): head=sha",
			head:       localRoundTestMovedHead,
			wantRefuse: []string{"does not parse"},
		},
		{
			name:       "unreadable notes refuse",
			notes:      "",
			readErr:    errors.New("bead store down"),
			head:       localRoundTestMovedHead,
			wantRefuse: []string{"cannot read the notes of gt-mxz8kx", "bead store down"},
		},
		{
			name:       "unreadable head refuses while a major blocks",
			notes:      localRoundTestBlock(major),
			headErr:    errors.New("no HEAD"),
			wantRefuse: []string{"could not read the branch head"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			check := localRoundSubmitCheck{
				readNotes: func(string) (string, error) { return tc.notes, tc.readErr },
				branchHead: func() (string, error) {
					if tc.headErr != nil {
						return "", tc.headErr
					}
					return tc.head, nil
				},
			}
			err := check.check("gt-mxz8kx")
			if len(tc.wantRefuse) == 0 {
				if err != nil {
					t.Fatalf("check refused a submit it should allow: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("check allowed a submit it should refuse")
			}
			for _, want := range tc.wantRefuse {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestDoneLocalRoundHasNoBypass(t *testing.T) {
	t.Parallel()
	doneCmd.Flags().VisitAll(func(f *pflag.Flag) {
		name := strings.ToLower(f.Name)
		if strings.Contains(name, "local") && strings.Contains(name, "round") {
			t.Errorf("done defines flag --%s, which would bypass the local-round check", f.Name)
		}
		if strings.Contains(name, "skip") {
			t.Errorf("done defines flag --%s, which would bypass the local-round check", f.Name)
		}
	})
}

// TestDoneLocalRoundGateChecksOnlyTheSubmittedBead: the gate reads the notes of
// the one bead done.Run resolved and of no other, so a seat hooked on another
// bead is never consulted.
func TestDoneLocalRoundGateChecksOnlyTheSubmittedBead(t *testing.T) {
	t.Parallel()
	var asked []string
	check := localRoundSubmitCheck{
		readNotes: func(bead string) (string, error) {
			asked = append(asked, bead)
			return localRoundTestBlock(localRoundTestFinding("f1", "major")), nil
		},
		branchHead: func() (string, error) { return localRoundTestMovedHead, nil },
	}
	gate := localRoundGate(ExitCompleted, check, &bytes.Buffer{})
	err := gate("gt-submitted", "polecat/capable/gt-stale+abc123")
	if err == nil || !strings.Contains(err.Error(), "gt-submitted") {
		t.Fatalf("gate = %v, want a refusal naming the submitted bead", err)
	}
	if len(asked) != 1 || asked[0] != "gt-submitted" {
		t.Fatalf("notes read for %q, want only gt-submitted", asked)
	}
}

func TestDoneLocalRoundGateSkipsNonCompletedSubmits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status string
	}{
		{name: "escalated submits nothing", status: ExitEscalated},
		{name: "deferred submits nothing", status: ExitDeferred},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := localRoundSubmitCheck{
				readNotes: func(string) (string, error) {
					t.Fatal("gate read the notes of a submit that submits nothing")
					return "", nil
				},
			}
			gate := localRoundGate(tc.status, check, &bytes.Buffer{})
			if err := gate("gt-mxz8kx", "polecat/capable/gt-mxz8kx+abc123"); err != nil {
				t.Fatalf("gate refused a submit it should skip: %v", err)
			}
		})
	}
}

// TestDoneLocalRoundGateNoticeWhenNoBeadResolves: with no bead resolved the gate
// has nothing to read, so the submit goes on, and the one notice names the
// branch so the skip is never silent.
func TestDoneLocalRoundGateNoticeWhenNoBeadResolves(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	check := localRoundSubmitCheck{
		readNotes: func(string) (string, error) {
			t.Fatal("gate read notes with no bead resolved")
			return "", nil
		},
	}
	gate := localRoundGate(ExitCompleted, check, &stderr)
	if err := gate("", "main"); err != nil {
		t.Fatalf("gate refused a submit with no work bead: %v", err)
	}
	const notice = "local-round gate skipped: branch main names no work bead"
	if n := strings.Count(stderr.String(), notice); n != 1 {
		t.Fatalf("notice printed %d times, want once; stderr = %q", n, stderr.String())
	}
}

// TestDoneLocalRoundGateRefusesUnreadableNotes: a resolved bead whose notes
// cannot be read refuses, with the error, rather than passing as clean.
func TestDoneLocalRoundGateRefusesUnreadableNotes(t *testing.T) {
	t.Parallel()
	check := localRoundSubmitCheck{
		readNotes:  func(string) (string, error) { return "", errors.New("bd show: connection refused") },
		branchHead: func() (string, error) { return localRoundTestMovedHead, nil },
	}
	gate := localRoundGate(ExitCompleted, check, &bytes.Buffer{})
	err := gate("gt-mxz8kx", "")
	if err == nil || !strings.Contains(err.Error(), "local-round gate cannot read the notes of gt-mxz8kx") {
		t.Fatalf("gate = %v, want a refusal that the notes cannot be read", err)
	}
}
