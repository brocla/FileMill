package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"filemill/internal/config"
	"filemill/internal/store"
)

// Only failures that are FileMill's problem report. A transformer that turned
// its input down through the contract is the sender's problem, and they hear
// about it in the reply. Reporting changes nothing the sender sees: statuses
// and messages are as they were before alerting, except that a result
// contradicting its exit code now says so instead of claiming success.
func TestExecuteReportsOnlySystemicFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		command    []string      // nil: the fake transformer acting out mode
		mode       string        // the fake transformer's behavior
		setup      func(*App)    // runs after the job is claimed
		timeout    time.Duration // 0: the production timeout
		cancelled  bool          // the worker is shutting down
		wantStatus string
		wantMsg    string // prefix of the job's message
		wantReport bool
		wantDetail string // also in the alert's detail, besides the job's identity
	}{
		{name: "success", mode: "succeed",
			wantStatus: store.StatusSucceeded, wantMsg: "done"},
		{name: "clean success:false", mode: "reject",
			wantStatus: store.StatusFailed, wantMsg: "not a worker list"},
		{name: "success:false with nonzero exit", mode: "reject-exit",
			wantStatus: store.StatusFailed, wantMsg: "not a worker list"},
		{name: "crash without result", mode: "crash",
			wantStatus: store.StatusFailed, wantMsg: "transformer exited unsuccessfully",
			wantReport: true, wantDetail: "Traceback: boom"},
		{name: "missing result.json", mode: "no-result",
			wantStatus: store.StatusFailed, wantMsg: "result.json missing", wantReport: true},
		{name: "invalid contract version", mode: "bad-version",
			wantStatus: store.StatusFailed, wantMsg: `unsupported result contract version "0"`, wantReport: true},
		{name: "success:true with nonzero exit", mode: "succeed-exit",
			wantStatus: store.StatusFailed, wantMsg: "transformer reported success but then exited with an error",
			wantReport: true, wantDetail: `exit status 3; its result.json said "done"`},
		{name: "timeout", mode: "hang", timeout: time.Second,
			wantStatus: store.StatusFailed, wantMsg: "transformer timed out after 10 minutes", wantReport: true},
		{name: "transformer no longer configured", mode: "succeed",
			setup:      func(a *App) { a.cfg = config.Config{} },
			wantStatus: store.StatusFailed, wantMsg: "registered transformer no longer exists", wantReport: true},
		{name: "transformer cannot start", command: []string{"no-such-transformer.exe"},
			wantStatus: store.StatusFailed, wantMsg: "transformer exited unsuccessfully",
			wantReport: true, wantDetail: "no-such-transformer"},
		// Shutdown cancels the job's context, which kills the transformer (here
		// before it starts). That is FileMill stopping, not the transformer
		// failing, so it must not alert on every restart that lands mid-job.
		{name: "killed by shutdown", mode: "hang", cancelled: true,
			wantStatus: store.StatusFailed, wantMsg: "transformer exited unsuccessfully"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.command
			if command == nil {
				command = fakeTransformer(t, tc.mode)
			}
			a, rep := newTestApp(t, command...)
			j := claimJob(t, a)
			if tc.setup != nil {
				tc.setup(a)
			}
			if tc.timeout > 0 {
				saved := jobTimeout
				jobTimeout = tc.timeout
				t.Cleanup(func() { jobTimeout = saved })
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}

			a.execute(ctx, j)

			got, err := a.Job(j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus || !strings.HasPrefix(got.Message, tc.wantMsg) {
				t.Errorf("job = %s %q, want %s %q…", got.Status, got.Message, tc.wantStatus, tc.wantMsg)
			}

			alerts := rep.reports()
			if !tc.wantReport {
				if len(alerts) != 0 {
					t.Fatalf("a non-systemic failure reported: %+v", alerts)
				}
				return
			}
			if len(alerts) != 1 || alerts[0].Category != "job-systemic" {
				t.Fatalf("alerts = %+v, want one job-systemic", alerts)
			}
			for _, want := range []string{j.ID, "Operation: fake", "Input: input.txt", tc.wantDetail} {
				if !strings.Contains(alerts[0].Detail, want) {
					t.Errorf("detail lacks %q:\n%s", want, alerts[0].Detail)
				}
			}
			if strings.ContainsAny(alerts[0].Summary, "\r\n") {
				t.Errorf("summary is not one line: %q", alerts[0].Summary)
			}
		})
	}
}

// A panic fails the job it happened in and is reported with its stack, and
// execute returns normally, so Run carries on with the next job. A transformer
// with no command is a real way in: execute slices the command's arguments
// before it runs anything. config.Load rejects one, but the App's own guard
// shouldn't depend on that.
func TestExecuteRecoversFromPanic(t *testing.T) {
	a, rep := newTestApp(t, fakeTransformer(t, "succeed")...)
	j := claimJob(t, a)
	a.cfg = config.Config{Transformers: []config.Transformer{{Operation: "fake"}}}

	a.execute(context.Background(), j)

	got, err := a.Job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status after a panic = %s, want %s", got.Status, store.StatusFailed)
	}
	alerts := rep.reports()
	if len(alerts) != 1 || alerts[0].Category != "panic" {
		t.Fatalf("alerts = %+v, want one panic", alerts)
	}
	for _, want := range []string{j.ID, "slice bounds out of range", "goroutine"} {
		if !strings.Contains(alerts[0].Detail, want) {
			t.Errorf("panic detail lacks %q:\n%s", want, alerts[0].Detail)
		}
	}
}

// The traceback of a crash is at the end of its output, so that is the part
// an alert keeps.
func TestOutputTailKeepsTheEnd(t *testing.T) {
	if got := outputTail([]byte("  short output\n")); got != "short output" {
		t.Errorf("short output = %q, want it whole and trimmed", got)
	}

	long := strings.Repeat("a", 3*outputTailSize) + "END"
	got := outputTail([]byte(long))
	if !strings.HasSuffix(got, "END") || !strings.HasPrefix(got, "…") {
		t.Errorf("long output tail = %q…, want the marked end", got[:20])
	}
	if n := len(got) - len("…"); n != outputTailSize {
		t.Errorf("tail keeps %d bytes, want %d", n, outputTailSize)
	}

	// A cut through a multi-byte character must not leave half of it behind.
	accented := "é" + strings.Repeat("b", outputTailSize-1)
	if got := outputTail([]byte(accented)); !strings.HasPrefix(got, "…b") {
		t.Errorf("tail starts %q, want the broken character dropped", got[:8])
	}
}

// A claim error is reported once it has lasted a minute, once per unbroken
// run of failures. A blip that clears within the minute stays silent.
func TestClaimFailuresReportOnceAfterAMinute(t *testing.T) {
	var c claimFailures
	t0 := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	for _, step := range []struct {
		at   time.Duration
		want bool
	}{
		{0, false},
		{59 * time.Second, false},
		{time.Minute, true},
		{2 * time.Minute, false}, // already reported this run
	} {
		if got := c.failed(t0.Add(step.at)); got != step.want {
			t.Fatalf("failed at +%v = %t, want %t", step.at, got, step.want)
		}
	}

	c.succeeded()
	if c.failed(t0.Add(3 * time.Minute)) {
		t.Fatal("a new run of failures reported at its first failure")
	}
	if !c.failed(t0.Add(4 * time.Minute)) {
		t.Fatal("a new run of failures was not reported after a minute")
	}
}

// Run wires claimFailures to the reporter and keeps retrying. Closing the
// store makes every claim fail, as a stuck database would.
func TestRunReportsPersistentClaimFailure(t *testing.T) {
	a, rep := newTestApp(t, fakeTransformer(t, "succeed")...)
	rep.notify = make(chan struct{}, 1)
	saved := claimAlertAfter
	claimAlertAfter = 0
	t.Cleanup(func() { claimAlertAfter = saved })
	a.store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, false) }()
	select {
	case <-rep.notify:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not report failing claims")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v; claim failures must not stop the worker", err)
	}

	alerts := rep.reports()
	if len(alerts) != 1 || alerts[0].Category != "worker-claim" {
		t.Fatalf("alerts = %+v, want one worker-claim", alerts)
	}
	if !strings.Contains(alerts[0].Detail, "database is closed") {
		t.Errorf("detail should carry the claim error:\n%s", alerts[0].Detail)
	}
}
