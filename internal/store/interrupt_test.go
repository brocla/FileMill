package store

import (
	"strings"
	"testing"
	"time"
)

// Only the worker marks jobs a dead predecessor left running. Open must not:
// every CLI command opens the same database, and one run while the worker is
// mid-job would mark that live job interrupted, which the delivery loop takes
// as finished.
func TestOpenLeavesRunningJobsAlone(t *testing.T) {
	s, path := openTestStore(t)
	if err := s.Create(Job{ID: "live", Operation: "op", InputName: "a.pdf", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}

	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	got, err := other.Get("live")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Errorf("a second Open changed a running job to %q", got.Status)
	}
}

func TestInterruptRunningMarksAndCountsLeftovers(t *testing.T) {
	s, _ := openTestStore(t)
	t0 := time.Now().UTC()
	for i, id := range []string{"left", "done", "queued"} {
		if err := s.Create(Job{ID: id, Operation: "op", InputName: id + ".pdf", CreatedAt: t0.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	// Claims go oldest first: "left" stays running, "done" finishes, "queued"
	// is never claimed.
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Complete("done", StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptRunning()
	if err != nil || n != 1 {
		t.Fatalf("InterruptRunning = %d, %v; want 1", n, err)
	}
	for id, want := range map[string]string{"left": StatusInterrupted, "done": StatusSucceeded, "queued": StatusQueued} {
		got, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Errorf("%s: status %q, want %q", id, got.Status, want)
		}
	}
	// The reply is built from the message, so it has to tell the sender what
	// to do: the job is not retried.
	if got, _ := s.Get("left"); !strings.Contains(got.Message, "send the file again") {
		t.Errorf("interrupted job message = %q, want it to ask for the file again", got.Message)
	}
	// It is completed, so the job sweep will eventually clear its workspace.
	expired, err := s.ExpiredJobs(time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(expired, "left") {
		t.Errorf("an interrupted job must be sweepable; expired = %v", expired)
	}

	if n, err := s.InterruptRunning(); err != nil || n != 0 {
		t.Errorf("a second InterruptRunning = %d, %v; want 0", n, err)
	}
}

func contains(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}
