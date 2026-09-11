package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filemill/internal/alert"
)

// jobRetentionPeriod is how long a completed job's workspace stays on disk
// before the sweep deletes it. By then the sender already has the result —
// attached, or (for sheets-link) uploaded to Drive — so the local copy is a
// backup nobody is waiting on.
//
// A var, not a const: mailgun's retention test ages records through a fake
// Engine, but App talks to a concrete *store.Store with no such seam, so its
// own test shrinks this instead of waiting 30 real days.
var jobRetentionPeriod = 30 * 24 * time.Hour

// removeWorkspace deletes one job's workspace. A var so the sweep's test can
// make a delete fail, which the filesystem won't do on request.
var removeWorkspace = os.RemoveAll

const (
	// jobSweepInterval mirrors mailgun's retention sweep: retention is measured
	// in days, so checking daily is ample.
	jobSweepInterval = 24 * time.Hour
	// jobFirstSweepDelay staggers the startup sweep just past the noisy first
	// moments of a restart, while still guaranteeing one runs.
	jobFirstSweepDelay = time.Minute
)

// SweepExpiredJobs runs the job-retention sweep until ctx is cancelled.
//
// This is the same pattern as mailgun.Service.SweepExpired, applied to local
// job workspaces instead of published Drive files: the first sweep runs
// shortly after startup rather than a full interval later, because the
// worker restarts on every config reload and crash, and a plain 24-hour
// ticker on a machine that restarts daily would never sweep anything at all.
func (a *App) SweepExpiredJobs(ctx context.Context) {
	timer := time.NewTimer(jobFirstSweepDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := a.sweepExpiredJobs(); err != nil {
			a.log.Printf("job sweep: %v", err)
		}
		timer.Reset(jobSweepInterval)
	}
}

// sweepExpiredJobs deletes the on-disk workspace of every completed job past
// the retention horizon. Only completed_at is checked (see
// store.ExpiredJobs), so a job still queued or running is never touched.
//
// A workspace that fails to delete is logged and left unmarked, so the next
// sweep retries it; one stuck directory must not strand the rest of the
// batch. The failures are reported together, once per sweep. RemoveAll is
// idempotent — a workspace already gone counts as deleted — so marking a job
// after a crash-interrupted sweep is never a problem.
//
// A sweep that deleted something says so; an idle one stays silent, so the
// line means something when it does appear — the same reasoning as
// mailgun.Service.sweepExpired.
func (a *App) sweepExpiredJobs() error {
	ids, err := a.store.ExpiredJobs(time.Now().UTC().Add(-jobRetentionPeriod))
	if err != nil {
		return err
	}
	deleted := 0
	var failed []string
	for _, id := range ids {
		if err := removeWorkspace(filepath.Join(a.data, "jobs", id)); err != nil {
			a.log.Printf("job sweep: delete %s: %v", id, err)
			failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		deleted++
		if err := a.store.MarkJobFilesDeleted(id); err != nil {
			a.log.Printf("job sweep: record deletion of %s: %v", id, err)
		}
	}
	days := int(jobRetentionPeriod.Hours() / 24)
	if deleted > 0 {
		a.log.Printf("job sweep: deleted %d job workspace(s) older than %d days", deleted, days)
	}
	if len(failed) > 0 {
		a.reporter.Report(alert.Alert{
			Category: "sweep-jobs",
			Summary:  fmt.Sprintf("job sweep could not delete %d workspace(s)", len(failed)),
			Detail: fmt.Sprintf("These completed jobs are past the %d-day retention period, but their workspaces under data/jobs could not be deleted. The next sweep retries them.\n\n%s\n",
				days, strings.Join(failed, "\n")),
		})
	}
	return nil
}
