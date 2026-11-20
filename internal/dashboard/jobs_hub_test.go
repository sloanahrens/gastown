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
