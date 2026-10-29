package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"filemill/internal/store"
)

// A completed job past the retention horizon has its workspace deleted; one
// inside the horizon is left alone. The sweep's clock is set 30 days past a
// boundary taken between the two completions, so the cutoff falls exactly
// there. A slow machine only widens the gaps on either side of it - an earlier
// version raced a 100ms horizon against the real clock and lost on slow CI.
func TestSweepExpiredJobsDeletesOldWorkspaceOnly(t *testing.T) {
	root := t.TempDir()
	writeTransformers(t, root, `transformers:
  - operation: copy_rename
    command: ["python.exe", "x.py"]
    extensions: [txt]
`)
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	src := filepath.Join(root, "input.txt")
	if err := os.WriteFile(src, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	oldID, err := a.Submit("copy_rename", src)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Complete(oldID, store.StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	// The pauses only make the three times distinct on Windows' coarse clock.
	time.Sleep(20 * time.Millisecond)
	boundary := time.Now()
	time.Sleep(20 * time.Millisecond)

	freshID, err := a.Submit("copy_rename", src)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Complete(freshID, store.StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return boundary.Add(jobRetentionPeriod) }

	if err := a.sweepExpiredJobs(); err != nil {
		t.Fatalf("sweepExpiredJobs: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "data", "jobs", oldID)); !os.IsNotExist(err) {
		t.Errorf("old job workspace still exists (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "jobs", freshID)); err != nil {
		t.Errorf("fresh job workspace was removed: %v", err)
	}

	// The row survives; only the files are gone. A second sweep must not
	// touch it again (nothing new expired) or error on the missing directory.
	job, err := a.Job(oldID)
	if err != nil {
		t.Fatalf("Job(oldID) after sweep: %v", err)
	}
	if job.Status != store.StatusSucceeded {
		t.Errorf("swept job status = %q, want %q", job.Status, store.StatusSucceeded)
	}
	if err := a.sweepExpiredJobs(); err != nil {
		t.Fatalf("second sweepExpiredJobs: %v", err)
	}
}

// A job whose workspace never existed to begin with — say a previous sweep
// crashed after RemoveAll but before marking the row — must not make the
// sweep error out. RemoveAll on an absent path is success, not failure.
func TestSweepExpiredJobsToleratesAlreadyMissingWorkspace(t *testing.T) {
	root := t.TempDir()
	writeTransformers(t, root, `transformers:
  - operation: copy_rename
    command: ["python.exe", "x.py"]
    extensions: [txt]
`)
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	src := filepath.Join(root, "input.txt")
	if err := os.WriteFile(src, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	id, err := a.Submit("copy_rename", src)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Complete(id, store.StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "data", "jobs", id)); err != nil {
		t.Fatal(err)
	}
	expireEverything(t, a)

	if err := a.sweepExpiredJobs(); err != nil {
		t.Fatalf("sweepExpiredJobs must tolerate an already-missing workspace: %v", err)
	}
}
