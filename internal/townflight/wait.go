package townflight

// Wait returns the labels of the listed commands still running and a channel
// closed once all of them have ended. Each running command is waited for in its
// own goroutine, so the wait covers every one at once. The caller owns the time
// bound. done is already closed when none is running. A listing or probe error
// is returned with a nil channel. A wait that fails does not confirm its command
// ended, so done stays open for the caller's bound to report it.
func Wait(townRoot string) (labels []string, done <-chan struct{}, err error) {
	return wait(townRoot, waitEnded)
}

// wait is Wait with the wait for one entry a test can fail.
func wait(townRoot string, waitOne func(string) error) ([]string, <-chan struct{}, error) {
	paths, err := listEntries(townRoot)
	if err != nil {
		return nil, nil, err
	}
	labels := make([]string, 0, len(paths))
	waiting := make([]string, 0, len(paths))
	for _, path := range paths {
		label, running, err := probe(path)
		if err != nil {
			return nil, nil, err
		}
		if running {
			labels = append(labels, label)
			waiting = append(waiting, path)
		}
	}
	done := make(chan struct{})
	if len(waiting) == 0 {
		close(done)
		return labels, done, nil
	}
	results := make(chan error, len(waiting))
	for _, path := range waiting {
		go func(path string) { results <- waitOne(path) }(path)
	}
	go func() {
		for i := 0; i < len(waiting); i++ {
			if err := <-results; err != nil {
				return // a failed wait never confirms its command ended
			}
		}
		close(done)
	}()
	return labels, done, nil
}
