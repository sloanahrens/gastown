package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// SideLoad runs `make presubmit` in each dir at once, each with GOCACHE set to a
// new empty directory under cacheRoot. It returns one SideRun per dir, in dirs
// order. A cache directory that cannot be made stops the load before any run
// starts; a run error is returned after the others finish, with every SideRun.
func SideLoad(ctx context.Context, dirs []string, cacheRoot string, run func(ctx context.Context, dir string, env []string, argv []string) (int, error), now func() time.Time) ([]SideRun, error) {
	caches := make([]string, len(dirs))
	for i := range dirs {
		cache, err := os.MkdirTemp(cacheRoot, "gocache-")
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
