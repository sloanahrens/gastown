//go:build !windows

package testutil

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A pool starts a container only for a lease the free list cannot meet, so a
// package whose tests never overlap still starts one, and it starts no more
// than scratchContainers however many lessees arrive at once.
func TestScratchPoolStartsOnlyAsLeasesOverlap(t *testing.T) {
	t.Parallel()
	var starts int
	p := newScratchPool(func(context.Context) (*scratchInstance, error) {
		starts++
		return &scratchInstance{}, nil
	})

	held := make([]*scratchInstance, 0, scratchContainers)
	for i := 0; i < scratchContainers; i++ {
		inst, err := p.acquire()
		if err != nil || inst == nil {
			t.Fatalf("acquire %d = (%v, %v), want a container", i, inst, err)
		}
		held = append(held, inst)
		if want := i + 1; starts != want {
			t.Fatalf("acquire %d started %d containers, want %d", i, starts, want)
		}
	}
	if inst, err := p.acquire(); inst != nil || err != nil {
		t.Fatalf("acquire at the cap = (%v, %v), want (nil, nil) so the lessee waits", inst, err)
	}

	for _, inst := range held {
		p.release(inst)
	}
	if inst, err := p.acquire(); inst == nil || err != nil {
		t.Fatalf("acquire after release = (%v, %v), want a free container", inst, err)
	}
	if starts != scratchContainers {
		t.Fatalf("released containers were not reused: %d starts", starts)
	}
}

// A start that fails stops the pool: the lessee that tried gets the real
// error, and every later lessee gets it without another attempt or a wait for
// a container that will never come free.
func TestScratchPoolReportsAStartFailureToEveryLessee(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("no Docker for you")
	var starts int
	p := newScratchPool(func(context.Context) (*scratchInstance, error) {
		starts++
		return nil, wantErr
	})

	for i := 0; i < 3; i++ {
		inst, err := p.acquire()
		if inst != nil || !errors.Is(err, wantErr) {
			t.Fatalf("acquire %d = (%v, %v), want (nil, %v)", i, inst, err, wantErr)
		}
	}
	if starts != 1 {
		t.Fatalf("starts = %d, want 1: a stopped pool must not start again", starts)
	}
}

// A lease's reset drops exactly what the test added: the image's databases
// and anything the container started with stay.
func TestDatabasesAdded(t *testing.T) {
	t.Parallel()
	baseline := []string{"gt_test", "information_schema", "mysql"}
	now := []string{"gt_test", "hq", "information_schema", "mysql", "testrig"}
	if got := databasesAdded(baseline, now); !slices.Equal(got, []string{"hq", "testrig"}) {
		t.Fatalf("databasesAdded = %q, want [hq testrig]", got)
	}
	if got := databasesAdded(baseline, baseline); len(got) != 0 {
		t.Fatalf("databasesAdded on an untouched catalog = %q, want none", got)
	}
}
