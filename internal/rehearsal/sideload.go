package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// errCacheNotEmpty is returned when a run's GOCACHE directory already holds
// files: a cache an earlier run warmed would hide the cold cost rehearsal measures.
var errCacheNotEmpty = errors.New("GOCACHE directory is not empty")

// makeCache creates the GOCACHE directory for dir i under cacheRoot. An existing
// directory is reused only while it is empty.
func makeCache(cacheRoot string, i int) (string, error) {
	cache := filepath.Join(cacheRoot, fmt.Sprintf("gocache-%d", i))
	err := os.Mkdir(cache, 0o700)
	if err == nil {
		return cache, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return "", fmt.Errorf("read GOCACHE directory %s: %w", cache, err)
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("%s: %w", cache, errCacheNotEmpty)
	}
	return cache, nil
}

// SideLoad runs `make presubmit` in each dir at once, each with GOCACHE set to
// its own empty directory under cacheRoot. It returns one SideRun per dir, in dirs
// order. A cache directory that cannot be made, or that already holds files, stops
// the load before any run starts; a run error is returned after the others finish,
// with every SideRun.
func SideLoad(ctx context.Context, dirs []string, cacheRoot string, run func(ctx context.Context, dir string, env []string, argv []string) (int, error), now func() time.Time) ([]SideRun, error) {
	caches := make([]string, len(dirs))
	for i := range dirs {
		cache, err := makeCache(cacheRoot, i)
		if err != nil {
			return nil, fmt.Errorf("side load: make GOCACHE directory: %w", err)
		}
		caches[i] = cache
	}
	runs := make([]SideRun, len(dirs))
	errs := make([]error, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := append(os.Environ(), "GOCACHE="+caches[i])
			start := now()
			code, err := run(ctx, dir, env, []string{"make", "presubmit"})
			runs[i] = SideRun{Seconds: now().Sub(start).Seconds(), ExitCode: code}
			if err != nil {
				errs[i] = fmt.Errorf("side load %s: %w", dir, err)
			}
		}()
	}
	wg.Wait()
	return runs, errors.Join(errs...)
}
