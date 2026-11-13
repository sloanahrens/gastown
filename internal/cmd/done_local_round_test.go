package cmd

import (
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

// localRoundTestBlock is one round on localRoundTestRoundHead, short as the
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
	reply := land.FormatLocalRoundReply("f1", land.LocalRoundFixed, "bounded the loop")
	withReply := func(block string) string { return block + "\n" + reply }

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
			wantRefuse: []string{"f1", "internal/x/a.go:12", "unbounded poll loop", "gt review-preview reply"},
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
		{
			name:       "critical without a reply refuses",
			notes:      localRoundTestBlock(localRoundTestFinding("f3", "critical")),
			head:       localRoundTestMovedHead,
			wantRefuse: []string{"f3"},
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
			wantRefuse: []string{"could not read the notes"},
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

func TestDoneLocalRoundBeadResolution(t *testing.T) {
	t.Parallel()
	errBranch := errors.New("not a worktree")
	tests := []struct {
		name      string
		explicit  string
		branch    string
		branchErr error
		wantBead  string
		wantErr   []string
	}{
		{name: "explicit issue wins", explicit: "gt-abc", branch: "main", wantBead: "gt-abc"},
		{name: "branch names the bead", branch: "polecat/capable/gt-mxz8kx+mweio5eh", wantBead: "gt-mxz8kx"},
		{name: "branch names no bead refuses", branch: "main", wantErr: []string{"names no work bead", "--issue"}},
		{name: "unreadable branch refuses", branchErr: errBranch, wantErr: []string{"getting current branch"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readBead := ""
			c := localRoundSubmitCheck{
				readNotes:  func(bead string) (string, error) { readBead = bead; return "", nil },
				branchHead: func() (string, error) { return localRoundTestRoundHead, nil },
			}
			err := c.checkFor(tc.explicit, func() (string, error) { return tc.branch, tc.branchErr })
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("checkFor refused: %v", err)
				}
				if readBead != tc.wantBead {
					t.Fatalf("checkFor read notes of %q, want %q", readBead, tc.wantBead)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkFor allowed a submit it should refuse")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestDoneLocalRoundGateSkipsNonSubmits(t *testing.T) {
	t.Parallel()
	polecatEnv := func(string) string { return "" }
	crewEnv := func(key string) string {
		if key == "GT_ROLE" {
			return "crew"
		}
		return ""
	}
	tests := []struct {
		name   string
		status string
		getenv func(string) string
	}{
		{name: "escalated submits nothing", status: ExitEscalated, getenv: polecatEnv},
		{name: "deferred submits nothing", status: ExitDeferred, getenv: polecatEnv},
		{name: "crew submit is not a polecat round", status: ExitCompleted, getenv: crewEnv},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := gateDoneLocalRound(tc.status, t.TempDir(), "", tc.getenv); err != nil {
				t.Fatalf("gate refused a submit it should skip: %v", err)
			}
		})
	}
}

func TestDoneLocalRoundWritesNothingOnRefusal(t *testing.T) {
	t.Parallel()
	major := localRoundTestFinding("f1", "major")
	check := localRoundSubmitCheck{
		readNotes:  func(string) (string, error) { return localRoundTestBlock(major), nil },
		branchHead: func() (string, error) { return localRoundTestMovedHead, nil },
	}
	submitted := false
	err := submitAfterLocalRound(
		func() error { return check.check("gt-mxz8kx") },
		func() error {
			submitted = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("submit went on past an unanswered major")
	}
	if submitted {
		t.Fatal("a refused submit reached the step that writes the READY TO LAND block and label")
	}
}
