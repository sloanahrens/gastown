package forgejoprobe

import (
	"context"
	"io"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// tailBytes is how much of a job's log the probe prints: the end of the log,
// where the failure is.
const tailBytes = 16384

// jobFailure is the status Forgejo gives a job that failed.
const jobFailure = "failure"

// PrintTail writes the tail of the failing job's log to out. It finds the push
// run on sha and reads the job named jobName, or the first failed job when no
// job has that name. Every read failure is logged through logf and returns, so
// the verdict the caller already has stands.
func PrintTail(ctx context.Context, st Statuses, out io.Writer, logf func(string, ...any), owner, repo, sha, jobName string) {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	runs, err := st.ListRuns(callCtx, owner, repo, forgejo.RunFilter{Event: []string{"push"}, HeadSHA: sha, Limit: 10})
	cancel()
	if err != nil {
		logf("no run to read for %s (%v)", sha, err)
		return
	}
	run, ok := newestRun(runs, sha)
	if !ok {
		logf("no workflow run tested %s", sha)
		return
	}

	callCtx, cancel = context.WithTimeout(ctx, callTimeout)
	jobs, err := st.ListRunJobs(callCtx, owner, repo, run.ID)
	cancel()
	if err != nil {
		logf("run %d: jobs could not be read (%v)", run.ID, err)
		return
	}
	job, ok := pickJob(jobs, jobName)
	if !ok {
		logf("run %d has no job '%s' and no failed job", run.ID, jobName)
		return
	}

	callCtx, cancel = context.WithTimeout(ctx, callTimeout)
	log, err := st.JobLogsTail(callCtx, owner, repo, job.ID, tailBytes)
	cancel()
	if err != nil {
		logf("the job log could not be read (%v)", err)
		return
	}
	// The server is asked for the tail, but one that ignores the range sends
	// the whole log, so the probe keeps the tail it prints here too.
	if len(log) > tailBytes {
		log = log[len(log)-tailBytes:]
	}
	logf("log tail of the log of run %d's job '%s':", run.ID, job.Name)
	if _, err := io.WriteString(out, log+"\n"); err != nil {
		logf("the log tail could not be written (%v)", err)
	}
}

// newestRun returns the run on sha with the greatest id, or false when none is.
func newestRun(list *forgejo.RunList, sha string) (forgejo.ActionRun, bool) {
	if list == nil {
		return forgejo.ActionRun{}, false
	}
	var best forgejo.ActionRun
	found := false
	for _, r := range list.Runs {
		if r.CommitSHA != sha {
			continue
		}
		if !found || r.ID > best.ID {
			best, found = r, true
		}
	}
	return best, found
}

// pickJob returns the job named name, else the first failed job.
func pickJob(jobs []forgejo.ActionRunJob, name string) (forgejo.ActionRunJob, bool) {
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	for _, j := range jobs {
		if j.Status == jobFailure {
			return j, true
		}
	}
	return forgejo.ActionRunJob{}, false
}
