package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchBinaryFiresOnceAfterReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt")
	if err := os.WriteFile(path, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fired := make(chan struct{}, 2)
	go watchBinary(ctx, path, 10*time.Millisecond, func() { fired <- struct{}{} })

	select {
	case <-fired:
		t.Fatal("fired with the binary unchanged")
	case <-time.After(60 * time.Millisecond):
	}

	// install replaces the file by rename, which changes the inode.
	next := path + ".new"
	if err := os.WriteFile(next, []byte("version two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-ctx.Done():
		t.Fatal("did not fire after the binary was replaced")
	}
	select {
	case <-fired:
		t.Fatal("fired twice")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestWatchBinaryIgnoresMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt")
	if err := os.WriteFile(path, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan struct{}, 1)
	go watchBinary(ctx, path, 10*time.Millisecond, func() { fired <- struct{}{} })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
		t.Fatal("a missing file counted as a change")
	case <-time.After(80 * time.Millisecond):
	}
}
