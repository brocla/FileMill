package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// backdateDelivery ages a record so a retention test doesn't have to wait 30
// days. Test-only, hence its home here rather than in the store's API.
func (s *Store) backdateDelivery(submissionID int64, outputIndex int, when time.Time) error {
	_, err := s.db.Exec("UPDATE email_deliveries SET created_at=? WHERE submission_id=? AND output_index=?",
		when.UTC().Format(time.RFC3339Nano), submissionID, outputIndex)
	return err
}

// backdateJobCompletion ages a job's completed_at so a job-sweep test doesn't
// have to wait 30 days. Test-only, hence its home here rather than in the
// store's API.
func (s *Store) backdateJobCompletion(id string, when time.Time) error {
	_, err := s.db.Exec("UPDATE jobs SET completed_at=? WHERE id=?", when.UTC().Format(time.RFC3339Nano), id)
	return err
}

// A published Drive file is recorded before the reply is sent, so a retry after
// a failed send finds the record and reuses the link instead of uploading a
// second copy. The record is keyed per output file, because one job may declare
// several.
func TestDeliveryRecordIsPerOutputAndReadBack(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutDelivery(7, 0, "file-a", "https://docs.google.com/a"); err != nil {
		t.Fatalf("put first output: %v", err)
	}
	if err := s.PutDelivery(7, 1, "file-b", "https://docs.google.com/b"); err != nil {
		t.Fatalf("put second output: %v", err)
	}

	got, ok, err := s.Delivery(7, 1)
	if err != nil || !ok {
		t.Fatalf("Delivery(7,1) = ok %t, err %v; want a record", ok, err)
	}
	if got.FileID != "file-b" || got.Link != "https://docs.google.com/b" {
		t.Fatalf("read back %+v, want file-b/…/b", got)
	}
	if _, ok, err := s.Delivery(7, 2); err != nil || ok {
		t.Fatalf("Delivery(7,2) = ok %t, err %v; want no record", ok, err)
	}
	// A different submission must not see this one's records.
	if _, ok, err := s.Delivery(8, 0); err != nil || ok {
		t.Fatalf("Delivery(8,0) = ok %t, err %v; want no record", ok, err)
	}
}

// The retention sweep asks for records older than a cutoff and marks each one
// deleted, so a swept record is never offered twice.
func TestExpiredDeliveriesRespectCutoffAndDeletion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutDelivery(1, 0, "old-file", "https://docs.google.com/old"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDelivery(2, 0, "new-file", "https://docs.google.com/new"); err != nil {
		t.Fatal(err)
	}
	// Back-date the first record past the retention horizon.
	if err := s.backdateDelivery(1, 0, time.Now().UTC().Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	expired, err := s.ExpiredDeliveries(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].FileID != "old-file" {
		t.Fatalf("expired = %+v, want only old-file", expired)
	}

	if err := s.MarkDeliveryDeleted(expired[0].SubmissionID, expired[0].OutputIndex); err != nil {
		t.Fatal(err)
	}
	again, err := s.ExpiredDeliveries(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a deleted record must not be offered again; got %+v", again)
	}
}

// The job sweep's whole query lives here: a completed job past the cutoff is
// offered once, and never again once marked.
func TestExpiredJobsRespectCutoffAndDeletion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	if err := s.Create(Job{ID: "old-job", Operation: "copy_rename", InputName: "a.txt", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete("old-job", StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.backdateJobCompletion("old-job", now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(Job{ID: "new-job", Operation: "copy_rename", InputName: "b.txt", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete("new-job", StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-30 * 24 * time.Hour)
	expired, err := s.ExpiredJobs(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0] != "old-job" {
		t.Fatalf("expired = %v, want only old-job", expired)
	}

	if err := s.MarkJobFilesDeleted("old-job"); err != nil {
		t.Fatal(err)
	}
	again, err := s.ExpiredJobs(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a swept job must not be offered again; got %v", again)
	}

	// The row itself must survive being swept — only files_deleted_at changes.
	got, err := s.Get("old-job")
	if err != nil {
		t.Fatalf("Get after sweep: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Errorf("swept job status = %q, want %q", got.Status, StatusSucceeded)
	}
}

// A job that is still queued or running must never be swept, however old its
// created_at is — completed_at is the only clock the sweep reads.
func TestExpiredJobsExcludesQueuedAndRunning(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ancient := time.Now().UTC().Add(-365 * 24 * time.Hour)
	if err := s.Create(Job{ID: "stuck-job", Operation: "copy_rename", InputName: "a.txt", CreatedAt: ancient}); err != nil {
		t.Fatal(err)
	}

	expired, err := s.ExpiredJobs(time.Now().UTC().Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("a job with no completed_at must never be offered; got %v", expired)
	}
}

// Reproduces the production race: the webhook handler writing rows while the
// worker claims jobs, concurrently. Before the SetMaxOpenConns(1) + busy_timeout
// fix this surfaced SQLITE_BUSY and crashed the worker; now all access
// serializes cleanly. The email path also exercises the refactored
// PendingEmails, whose nested query would deadlock (hang) under a single
// connection if it held the outer result set open.
func TestConcurrentAccessDoesNotReturnBusy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	errs := make(chan error, 256)
	var wg sync.WaitGroup

	// Writers: create jobs, like the webhook handler's intake.
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := s.Create(Job{ID: fmt.Sprintf("job-%d", n), Operation: "op", InputName: "f.txt", CreatedAt: time.Now()}); err != nil {
				errs <- fmt.Errorf("create: %w", err)
			}
		}(i)
	}

	// Claimers: Next + Complete, like the worker loop, racing the writers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				j, err := s.Next()
				if err != nil {
					errs <- fmt.Errorf("next: %w", err)
					return
				}
				if j != nil {
					if err := s.Complete(j.ID, StatusSucceeded, "done"); err != nil {
						errs <- fmt.Errorf("complete: %w", err)
					}
				}
			}
		}()
	}

	// Email path + PendingEmails, exercising the refactored nested query.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sid, _, err := s.BeginEmail(fmt.Sprintf("<msg-%d>", n), "a@b", "r@c", "subj")
			if err != nil {
				errs <- fmt.Errorf("beginemail: %w", err)
				return
			}
			jobID := fmt.Sprintf("ejob-%d", n)
			if err := s.Create(Job{ID: jobID, Operation: "op", InputName: "f.pdf", CreatedAt: time.Now()}); err != nil {
				errs <- fmt.Errorf("email create: %w", err)
				return
			}
			if err := s.AddEmailJob(sid, 0, jobID); err != nil {
				errs <- fmt.Errorf("addemailjob: %w", err)
				return
			}
			if err := s.SetEmailExpected(sid, 1); err != nil {
				errs <- fmt.Errorf("setexpected: %w", err)
				return
			}
			if _, err := s.PendingEmails(); err != nil {
				errs <- fmt.Errorf("pending: %w", err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access error: %v", err)
	}
}
