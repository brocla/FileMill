package main

import (
	"fmt"
	"strconv"
	"strings"

	"filemill/internal/alert"
)

// The supervisor (scripts/Supervise-FileMill.ps1) sets these for each relaunch,
// so the new worker can report how the last one ended: a process can't report
// its own death.
const (
	previousExitEnv  = "FILEMILL_PREVIOUS_EXIT"  // the last exit code; unset on a first launch
	rapidRestartsEnv = "FILEMILL_RAPID_RESTARTS" // restarts in quick succession so far
)

// crashLoopAt matches the supervisor's $crashLoopAt: this many rapid restarts
// is a crash-loop.
const crashLoopAt = 4

// restartAlert builds the restart alert, if there is anything to report: a
// supervisor relaunch, or jobs the last worker left running, which means it
// didn't stop cleanly even when no supervisor was there to say so.
func restartAlert(previousExit, rapidRestarts string, interrupted int) (alert.Alert, bool) {
	if previousExit == "" && interrupted == 0 {
		return alert.Alert{}, false
	}
	var summary, detail strings.Builder
	if previousExit != "" {
		fmt.Fprintf(&summary, "worker restarted after exit code %s", previousExit)
		fmt.Fprintf(&detail, "The supervisor restarted the FileMill worker after exit code %s.\n", previousExit)
		if rapid, err := strconv.Atoi(rapidRestarts); err == nil && rapid >= crashLoopAt {
			fmt.Fprintf(&summary, " (crash-loop: %d rapid restarts)", rapid)
			fmt.Fprintf(&detail, "It has restarted %d times in quick succession, a crash-loop. The supervisor backs off up to 2 minutes between attempts; data\\logs\\supervisor.log and filemill.log have the details.\n", rapid)
		}
		if interrupted > 0 {
			fmt.Fprintf(&summary, ", %d job(s) interrupted", interrupted)
		}
	} else {
		fmt.Fprintf(&summary, "worker started after an unclean stop: %d job(s) interrupted", interrupted)
		detail.WriteString("The FileMill worker found jobs its predecessor left running, so the last worker did not stop cleanly: a crash, a kill, or a power loss.\n")
	}
	if interrupted > 0 {
		fmt.Fprintf(&detail, "\n%d job(s) were running when the previous worker stopped. They are marked interrupted and not retried; each sender's reply asks them to send the file again.\n", interrupted)
	}
	return alert.Alert{Category: "restart", Summary: summary.String(), Detail: detail.String()}, true
}
