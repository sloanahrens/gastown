//go:build integration

package runnerroll

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// integrationBound is the longest a test of the real scripts may run; the context it
// bounds kills a hung sh.
const integrationBound = 30 * time.Second

// execRunner runs argv for real, with stdin, as the pod exec does, so the store's
// scripts run through /bin/sh here.
func execRunner(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.Output()
}

// integrationStore is the real store over execRunner, in a town root that
// t.TempDir makes 0700.
func integrationStore(t *testing.T) (HoldStore, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewPodStore(dir, execRunner)
	if err != nil {
		t.Fatalf("NewPodStore: %v", err)
	}
	return store, dir
}

// assertOnlyHold fails unless the town root holds landing.hold and nothing else.
func assertOnlyHold(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != landingHoldName {
		t.Fatalf("town root = %v (%v), want only %s", entries, err, landingHoldName)
	}
}

// TestIntegrationHoldCreateIsExclusive: two creates started together give one nil and
// one ErrExists, the file holds the winner's content, and no temp file is left.
func TestIntegrationHoldCreateIsExclusive(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), integrationBound)
	defer cancel()
	store, dir := integrationStore(t)
	contents := []string{"roll a\nowner=a token=t-a at=x", "roll b\nowner=b token=t-b at=x"}
	gate, ready := make(chan struct{}), make(chan struct{}, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-gate
			errs[i] = store.Create(ctx, contents[i])
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("a create goroutine never started within the bound")
		}
	}
	close(gate)
	wg.Wait() // a hung sh is killed by ctx, so Create returns and this ends

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case errors.Is(err, ErrExists):
		default:
			t.Fatalf("create %d = %v, want one nil and the other ErrExists", i, err)
		}
	}
	got, present, err := store.Read(ctx)
	if winner < 0 || err != nil || !present || got != contents[winner] {
		t.Fatalf("winner %d, file = (%q, %v, %v), errs %v; want a winner whose content the file holds", winner, got, present, err, errs)
	}
	assertOnlyHold(t, dir)
}

// TestIntegrationHoldReadClassifies: Read tells a missing, an own and another's file apart.
func TestIntegrationHoldReadClassifies(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), integrationBound)
	defer cancel()
	store, dir := integrationStore(t)
	if _, present, err := store.Read(ctx); err != nil || present {
		t.Fatalf("Read of a missing file = present %v, err %v; want absent", present, err)
	}
	h, err := Take(ctx, store, "roll-a", "roll 1/5", fixedNow)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	own, present, err := store.Read(ctx)
	if err != nil || !present || !carriesToken(own, h.Token) {
		t.Fatalf("Read of own hold = (%q, %v, %v), want it present with its token", own, present, err)
	}
	if err := h.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.Create(ctx, "other\nowner=b token=t-b at=x"); err != nil {
		t.Fatalf("Create of another's hold: %v", err)
	}
	other, present, err := store.Read(ctx)
	if err != nil || !present || carriesToken(other, h.Token) {
		t.Fatalf("Read of another's hold = (%q, %v, %v), want it present without this token", other, present, err)
	}
	assertOnlyHold(t, dir)
}
