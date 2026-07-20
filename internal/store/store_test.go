package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

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
