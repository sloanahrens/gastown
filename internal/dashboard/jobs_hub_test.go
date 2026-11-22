package dashboard

import "testing"

// The hub reads the job list on its own cadence and publishes it as State.Jobs,
// with the jobs source marked read: the Maintenance jobs pane draws from it.
func TestHubJobsRead(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{
		Jobs: func() *Jobs {
			return &Jobs{Rows: []JobRow{{Name: "dolt_gc", State: "ok"}}, Worst: "ok"}
		},
	})
	if st := h.State(); st.Jobs != nil {
		t.Fatalf("jobs before any read: %+v", st.Jobs)
	}
	h.pollJobs()

	st := h.State()
	if st.Jobs == nil || len(st.Jobs.Rows) != 1 || st.Jobs.Rows[0].Name != "dolt_gc" {
		t.Fatalf("a good read left jobs %+v, want the one dolt_gc row", st.Jobs)
	}
	if r := st.Reads[SourceJobs]; r.Error || r.Stale {
		t.Errorf("a good jobs read is flagged: %+v", r)
	}
}

// A failed read (a nil list) keeps the last good list and marks the source
// failed and stale, as the Jev spend row beside it does, so the pane draws the
// old rows with a stale flag rather than an empty section.
func TestHubJobsReadFailureKeepsLast(t *testing.T) {
	t.Parallel()

	ok := true
	h := NewHub(Config{
		Jobs: func() *Jobs {
			if !ok {
				return nil
			}
			return &Jobs{Rows: []JobRow{{Name: "dolt_gc", State: "ok"}}, Worst: "ok"}
		},
	})
	h.pollJobs()
	ok = false
	h.pollJobs()

	st := h.State()
	if st.Jobs == nil || len(st.Jobs.Rows) != 1 {
		t.Fatalf("a failed read left jobs %+v, want the last good list", st.Jobs)
	}
	if r := st.Reads[SourceJobs]; !r.Error || !r.Stale {
		t.Errorf("a failed jobs read is not flagged error and stale: %+v", r)
	}
}

// The hub keeps the last good job list across a failed read and marks it, so
// the pane draws the old rows as stale rather than current; a good read clears
// the marks. A first read that fails leaves an empty marked list, so a wired
// reader that fails says so instead of leaving the pane blank (gt-16wpss).
func TestHubJobsKeepsLastGood(t *testing.T) {
	t.Parallel()

	good := &Jobs{Rows: []JobRow{{Name: "dolt_gc", State: "ok"}}, Worst: "ok"}
	var list *Jobs
	h := NewHub(Config{Jobs: func() *Jobs { return list }})

	h.pollJobs()
	st := h.State()
	if st.Jobs == nil || st.Jobs.Rows == nil || len(st.Jobs.Rows) != 0 {
		t.Fatalf("a first failed read left jobs %+v, want an empty list", st.Jobs)
	}
	if !st.Jobs.ReadFailed || !st.Jobs.Stale {
		t.Errorf("a first failed read is not marked failed and stale: %+v", st.Jobs)
	}

	list = good
	h.pollJobs()
	st = h.State()
	if st.Jobs == nil || len(st.Jobs.Rows) != 1 || st.Jobs.Rows[0].Name != "dolt_gc" {
		t.Fatalf("a good read left jobs %+v, want the one dolt_gc row", st.Jobs)
	}
	if st.Jobs.ReadFailed || st.Jobs.Stale {
		t.Errorf("a good read left jobs marked failed or stale: %+v", st.Jobs)
	}

	list = nil
	h.pollJobs()
	st = h.State()
	if st.Jobs == nil || len(st.Jobs.Rows) != 1 || st.Jobs.Rows[0].Name != "dolt_gc" {
		t.Fatalf("a failed read left jobs %+v, want the last good list", st.Jobs)
	}
	if !st.Jobs.ReadFailed || !st.Jobs.Stale {
		t.Errorf("a failed read is not marked failed and stale: %+v", st.Jobs)
	}
}
