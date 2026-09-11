package main

import (
	"strings"
	"testing"
)

// A process can't report its own death, so the restarted worker reports it,
// from what the supervisor passes it and the jobs its predecessor left
// running.
func TestRestartAlert(t *testing.T) {
	for _, tc := range []struct {
		name         string
		previousExit string // FILEMILL_PREVIOUS_EXIT; empty on a first launch
		rapid        string // FILEMILL_RAPID_RESTARTS
		interrupted  int
		wantReport   bool
		wantSummary  string
		wantDetail   []string
	}{
		{name: "first launch after a clean stop"},
		{name: "restart after a crash", previousExit: "2", rapid: "1", wantReport: true,
			wantSummary: "worker restarted after exit code 2", wantDetail: []string{"exit code 2"}},
		{name: "below the crash-loop threshold", previousExit: "2", rapid: "3", wantReport: true,
			wantSummary: "worker restarted after exit code 2"},
		{name: "crash-loop", previousExit: "2", rapid: "4", wantReport: true,
			wantSummary: "worker restarted after exit code 2 (crash-loop: 4 rapid restarts)", wantDetail: []string{"crash-loop"}},
		{name: "unreadable restart count", previousExit: "1", rapid: "lots", wantReport: true,
			wantSummary: "worker restarted after exit code 1"},
		// Jobs left running mean the last worker didn't stop cleanly, even
		// with no supervisor to say so.
		{name: "interrupted jobs without a supervisor", interrupted: 3, wantReport: true,
			wantSummary: "worker started after an unclean stop: 3 job(s) interrupted", wantDetail: []string{"3 job(s)"}},
		{name: "crash with interrupted jobs", previousExit: "-1073741819", rapid: "0", interrupted: 1, wantReport: true,
			wantSummary: "worker restarted after exit code -1073741819, 1 job(s) interrupted", wantDetail: []string{"1 job(s)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := restartAlert(tc.previousExit, tc.rapid, tc.interrupted)
			if ok != tc.wantReport {
				t.Fatalf("report = %t, want %t (alert %+v)", ok, tc.wantReport, a)
			}
			if !ok {
				return
			}
			if a.Category != "restart" || a.Summary != tc.wantSummary {
				t.Errorf("alert = %s %q, want restart %q", a.Category, a.Summary, tc.wantSummary)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(a.Detail, want) {
					t.Errorf("detail lacks %q:\n%s", want, a.Detail)
				}
			}
		})
	}
}
