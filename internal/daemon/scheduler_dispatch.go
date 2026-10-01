package daemon

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/schedulerrun"
)

// schedulerDispatchTimeout bounds one scheduled-dispatch pass. It allows
// multi-bead dispatch with formula cooking and hook retries; a timeout that
// fires mid-dispatch leaves the context queued (the hook's retry and the
// label-swap retry in the sling path both cover the partial case), and the
// next heartbeat picks it up.
const schedulerDispatchTimeout = 5 * time.Minute

// SetSchedulerDeps installs the collaborators scheduled dispatch runs on: the
// sling, the polecat-capacity probe, and the cross-rig escalation. They live
// in package cmd, which is what starts this daemon, so it hands them over
// here rather than the daemon reaching back for them (gt-638go.9).
//
// A daemon with no deps installed does not dispatch: there is nothing to
// dispatch with.
func (d *Daemon) SetSchedulerDeps(deps schedulerrun.Deps) {
	d.schedulerDeps = deps
}

// dispatchScheduledWork runs one in-process scheduler dispatch pass. It
// replaces the `gt scheduler run` subprocess the heartbeat used to fork: the
// queue logic lives in internal/schedulerrun, which both this and the command
// call, so the two run identical code.
func (d *Daemon) dispatchScheduledWork(ctx context.Context) {
	if d.schedulerDeps.Sling == nil {
		return
	}
	writer := newSchedulerLogWriter(d.logger)
	report, err := schedulerrun.Run(ctx, schedulerrun.Options{
		TownRoot: d.config.TownRoot,
		Actor:    schedulerRunActor,
		Daemon:   true,
		Out:      writer,
		ErrOut:   writer,
	}, d.schedulerDeps)
	if err != nil {
		d.logger.Printf("Scheduler dispatch failed: %v", err)
		return
	}
	if report.Dispatched > 0 || report.Failed > 0 {
		d.logger.Printf("Scheduler dispatch: dispatched %d, failed %d, skipped %d (reason: %s)",
			report.Dispatched, report.Failed, report.Skipped, report.Reason)
	}
}

// schedulerRunActor is the identity scheduled dispatch runs under in the
// scheduler feed events.
const schedulerRunActor = "daemon"

// schedulerLogWriter turns the scheduler's operator narration into daemon log
// lines. In-process dispatch writes to a writer instead of to a child process
// whose stdout the daemon read, so each line is logged as it is written.
type schedulerLogWriter struct {
	logger *log.Logger
	mu     sync.Mutex
	buf    []byte
}

func newSchedulerLogWriter(logger *log.Logger) *schedulerLogWriter {
	return &schedulerLogWriter{logger: logger}
}

func (w *schedulerLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.logger.Printf("Scheduler dispatch: %s", line)
	}
	return len(p), nil
}
