package app

import (
	"strings"
	"testing"
)

// A panic in one job sweep must not end the sweep loop, which would leave
// workspaces piling up past retention with nothing to say so.
func TestJobSweepTickSurvivesPanic(t *testing.T) {
	a, rep := newTestApp(t, fakeTransformer(t, "succeed")...)
	completedJob(t, a)
	expireEverything(t)
	saved := removeWorkspace
	removeWorkspace = func(string) error { panic("boom") }
	t.Cleanup(func() { removeWorkspace = saved })

	a.sweepJobsTick()

	alerts := rep.reports()
	if len(alerts) != 1 || alerts[0].Category != "panic" {
		t.Fatalf("alerts = %+v, want one panic", alerts)
	}
	for _, want := range []string{"boom", "goroutine"} {
		if !strings.Contains(alerts[0].Detail, want) {
			t.Errorf("panic detail lacks %q:\n%s", want, alerts[0].Detail)
		}
	}
}

// InterruptLeftoverJobs is what run calls at startup; it hands the store's
// count back for the restart alert.
func TestInterruptLeftoverJobsCountsRunningJobs(t *testing.T) {
	a, _ := newTestApp(t, fakeTransformer(t, "succeed")...)
	claimJob(t, a) // left running, as a crashed worker would leave it

	n, err := a.InterruptLeftoverJobs()
	if err != nil || n != 1 {
		t.Fatalf("InterruptLeftoverJobs = %d, %v; want 1", n, err)
	}
}
