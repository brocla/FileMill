package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filemill/internal/store"
)

// expireEverything makes the job sweep treat every job completed so far as
// past retention, with no waiting, by running the sweep's clock one retention
// period plus an hour ahead.
func expireEverything(t *testing.T, a *App) {
	t.Helper()
	a.now = func() time.Time { return time.Now().Add(jobRetentionPeriod + time.Hour) }
}

func completedJob(t *testing.T, a *App) store.Job {
	t.Helper()
	j := claimJob(t, a)
	if err := a.store.Complete(j.ID, store.StatusSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	return j
}

// A workspace that won't delete keeps the sender's files on disk past the
// retention promise. The sweep reports once for the batch, naming each job.
func TestSweepExpiredJobsReportsDeleteFailures(t *testing.T) {
	a, rep := newTestApp(t, fakeTransformer(t, "succeed")...)
	j := completedJob(t, a)
	expireEverything(t, a)
	saved := removeWorkspace
	removeWorkspace = func(string) error { return errors.New("access is denied") }
	t.Cleanup(func() { removeWorkspace = saved })

	if err := a.sweepExpiredJobs(); err != nil {
		t.Fatalf("sweepExpiredJobs: %v", err)
	}

	alerts := rep.reports()
	if len(alerts) != 1 || alerts[0].Category != "sweep-jobs" {
		t.Fatalf("alerts = %+v, want one sweep-jobs", alerts)
	}
	for _, want := range []string{j.ID, "access is denied"} {
		if !strings.Contains(alerts[0].Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, alerts[0].Detail)
		}
	}
}

func TestCleanJobSweepDoesNotReport(t *testing.T) {
	a, rep := newTestApp(t, fakeTransformer(t, "succeed")...)
	j := completedJob(t, a)
	expireEverything(t, a)

	if err := a.sweepExpiredJobs(); err != nil {
		t.Fatalf("sweepExpiredJobs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.data, "jobs", j.ID)); !os.IsNotExist(err) {
		t.Fatalf("the job was not swept (stat err = %v), so this test proves nothing", err)
	}
	if alerts := rep.reports(); len(alerts) != 0 {
		t.Errorf("a clean sweep reported: %+v", alerts)
	}
}
