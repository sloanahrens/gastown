//go:build integration

package polecat

import (
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This test needs a real git talking HTTP to a real (stalling) socket, so it
// lives in the integration tier; the unit tier pins the production bound in
// TestNewWorkSurvivalUsesTheRemoteQueryTimeout.
// An https origin that stalls must not hang the predicate: over http git
// forks git-remote-http, and the bounded remote calls kill that helper along
// with git, so the answer comes back "unknown" near the bound (gt-vm5g4).
func TestIntegrationSurvivingWorkStallingHTTPOriginIsUnknownAndBounded(t *testing.T) {
	for _, k := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no loopback listener: %v", err)
	}
	var (
		mu   sync.Mutex
		held []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // accept, then say nothing
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	const bound = time.Second
	stallURL := "http://" + ln.Addr().String() + "/repo.git"

	for _, tc := range []struct {
		name       string
		localWork  bool
		remoteHits int // stalled remote calls before the answer: ls-remote, then the base fetch
	}{
		{name: "no local candidate", remoteHits: 1},
		{name: "local candidate needs the base refresh", localWork: true, remoteHits: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A rig whose .repo.git has origin at the stalling URL, and, for
			// the local candidate, the branch with work in it already.
			tmp := t.TempDir()
			rigRoot := filepath.Join(tmp, "gastown")
			bare := filepath.Join(rigRoot, ".repo.git")
			runGit(t, tmp, "init", "--bare", "--initial-branch=main", bare)
			if tc.localWork {
				seed := filepath.Join(tmp, "seed")
				branch := "polecat/basalt/" + survivalIssue + "+mu5wzd6q"
				runGit(t, tmp, "init", "--initial-branch=main", seed)
				seedFile(t, filepath.Join(seed, "base.txt"), "base\n")
				runGit(t, seed, "add", "-A")
				runGit(t, seed, "commit", "-m", "base")
				runGit(t, seed, "checkout", "-q", "-b", branch)
				seedFile(t, filepath.Join(seed, "work.txt"), "work.txt\n")
				runGit(t, seed, "add", "-A")
				runGit(t, seed, "commit", "-m", "work on "+branch)
				runGit(t, seed, "push", "-q", bare, "main", branch)
			}
			runGit(t, bare, "remote", "add", "origin", stallURL)
			start := time.Now()
			w, err := newWorkSurvival(gitOpener{}, rigRoot, bound)
			if err != nil {
				t.Fatal(err)
			}
			got, err := w.ForIssue(survivalIssue)
			elapsed := time.Since(start)
			t.Logf("returned in %v", elapsed.Round(10*time.Millisecond))
			if err == nil {
				t.Fatalf("want unknown (error) from a stalling origin, got branch %q", got)
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("want a timeout in the unknown answer, got %v", err)
			}
			if limit := time.Duration(tc.remoteHits)*bound + 8*time.Second; elapsed > limit {
				t.Fatalf("predicate took %v, limit %v", elapsed, limit)
			}
		})
	}
}
