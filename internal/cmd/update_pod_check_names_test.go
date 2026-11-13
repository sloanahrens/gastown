package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// TestUpdatePodPairCheckNamesTheCheckThatFailed holds each pair check failure to
// the check the rollback record names: a daemon pod on another digest is
// running-digests, a dev tag on another digest is registry-tag, and a values
// file that no longer holds the pair names no check.
func TestUpdatePodPairCheckNamesTheCheckThatFailed(t *testing.T) {
	t.Parallel()
	otherDaemon := "registry.internal:5000/town/gastown-daemon@" + testDigestOf([]byte("another daemon image"))
	rows := []struct {
		name      string
		kube      podPairKubectl
		wantCheck string
		wantMsg   string
	}{
		{
			name:      "a daemon pod runs another digest",
			kube:      podPairKubectl{pods: []podPairPod{runningPod("gastown-daemon-new", otherDaemon)}},
			wantCheck: update.CheckRunningDigests,
			wantMsg:   "runs daemon digest",
		},
		{
			name:      "the dev tag names another digest",
			kube:      podPairKubectl{pods: podPairHealthy(), devTag: podPairDevTagJSON(true, testDigestOf([]byte("another polecat image")))},
			wantCheck: update.CheckRegistryTag,
			wantMsg:   "names digest",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path, _ := podPairValuesFile(t)
			kube := row.kube
			clock := &rollbackTestClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
			check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path, Clock: clock, Sleep: clock.sleep}
			err := check.Check(context.Background(), podPairPair)
			var pc update.PairCheckError
			if !errors.As(err, &pc) {
				t.Fatalf("Check error = %v, want a PairCheckError", err)
			}
			if pc.Check != row.wantCheck {
				t.Errorf("check = %q, want %q", pc.Check, row.wantCheck)
			}
			if err == nil || !strings.Contains(err.Error(), row.wantMsg) {
				t.Errorf("Check error = %v, want its message to name %q", err, row.wantMsg)
			}
		})
	}
}

// TestUpdatePodPairCheckRefusalNamesNoCheck holds a values file that no longer
// holds the pair to no check of its own: it is refused before any step, and the
// rollback names it as the pair check.
func TestUpdatePodPairCheckRefusalNamesNoCheck(t *testing.T) {
	t.Parallel()
	path, _ := podPairValuesFile(t)
	other := podPairPair
	other.Commit = "3333333333333333333333333333333333333333"
	kube := podPairKubectl{pods: podPairHealthy()}
	check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path}
	// The file holds podPairPair, so a check of another pair refuses it.
	err := check.Check(context.Background(), other)
	var pc update.PairCheckError
	if errors.As(err, &pc) {
		t.Fatalf("Check error = %v names check %q, want no named check", err, pc.Check)
	}
	if len(kube.calls) != 0 {
		t.Errorf("commands = %d, want none: the file refused the pair before any step", len(kube.calls))
	}
}
