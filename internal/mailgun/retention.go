package mailgun

import (
	"context"
	"time"
)

const (
	// retentionPeriod is how long a published Sheet stays in Drive before the
	// sweep deletes it. The sender got their link; after this it is gone.
	retentionPeriod = 30 * 24 * time.Hour
	// sweepInterval is how often the sweep runs. Retention is measured in days,
	// so checking daily is ample.
	sweepInterval = 24 * time.Hour
	// firstSweepDelay staggers the startup sweep just past the noisy first
	// moments of a restart, while still guaranteeing one runs.
	firstSweepDelay = time.Minute
)

// SweepExpired runs the retention sweep until ctx is cancelled.
//
// The first sweep runs shortly after startup rather than a full interval later.
// The worker is restarted for every config reload and by the supervisor after a
// crash, so a plain 24-hour ticker on a machine that restarts daily would never
// sweep anything at all.
func (s *Service) SweepExpired(ctx context.Context) {
	timer := time.NewTimer(firstSweepDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := s.sweepExpired(ctx); err != nil {
			s.log.Printf("retention sweep: %v", err)
		}
		timer.Reset(sweepInterval)
	}
}

// sweepExpired deletes every published file past the retention horizon.
//
// A file that fails to delete is logged and left unmarked, so the next sweep
// tries again; one unreachable file must not strand the rest of the batch. The
// delete itself is idempotent — a file already gone counts as deleted — so a
// record marked after a crash-interrupted sweep is never a problem.
//
// A sweep that deleted something says so. Retention runs unattended, on a daily
// timer, and removes other people's data: if only failures were logged, a sweep
// that never ran would look exactly like a sweep with nothing to do. Silence is
// kept for the idle case, so the line means something when it does appear.
func (s *Service) sweepExpired(ctx context.Context) error {
	if s.publisher == nil {
		return nil // no link delivery configured, so nothing was ever published
	}
	expired, err := s.engine.ExpiredDeliveries(time.Now().UTC().Add(-retentionPeriod))
	if err != nil {
		return err
	}
	deleted := 0
	for _, record := range expired {
		if err := s.publisher.Delete(ctx, record.FileID); err != nil {
			s.log.Printf("retention sweep: delete %s (submission %d): %v", record.FileID, record.SubmissionID, err)
			continue
		}
		deleted++
		if err := s.engine.MarkDeliveryDeleted(record.SubmissionID, record.OutputIndex); err != nil {
			s.log.Printf("retention sweep: record deletion of %s: %v", record.FileID, err)
		}
	}
	if deleted > 0 {
		s.log.Printf("retention sweep: deleted %d published file(s) older than %d days", deleted, int(retentionPeriod.Hours()/24))
	}
	return nil
}
