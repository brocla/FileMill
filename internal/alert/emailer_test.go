package alert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var (
	_ Reporter = Nop{}
	_ Reporter = (*Emailer)(nil)
)

// capNotice marks the one email that says the daily cap was reached.
const capNotice = "daily alert cap reached"

// Repeats inside the cooldown send nothing, but are counted, and the next
// email for the category says how many were held back.
func TestCooldownSuppressesRepeatsAndCountsThem(t *testing.T) {
	h := newHarness(t)

	for i := range 5 {
		h.report("job-systemic", "transformer timed out")
		if i < 4 {
			h.clock.Advance(time.Minute)
		}
	}
	sent := h.mailer.sends()
	if len(sent) != 1 {
		t.Fatalf("5 reports inside the cooldown sent %d emails, want 1", len(sent))
	}
	if sent[0].to != testRecipient || sent[0].subject != "[FileMill] transformer timed out" {
		t.Fatalf("first email = to %q, subject %q", sent[0].to, sent[0].subject)
	}
	if strings.Contains(sent[0].text, "more since") {
		t.Errorf("first email claims suppressed alerts:\n%s", sent[0].text)
	}

	// The cooldown runs from the first send; 11 more minutes reaches it exactly.
	h.clock.Advance(11 * time.Minute)
	h.report("job-systemic", "transformer timed out")
	sent = h.mailer.sends()
	if len(sent) != 2 {
		t.Fatalf("a report after the cooldown sent %d emails in total, want 2", len(sent))
	}
	if !strings.Contains(sent[1].text, "4 more since the last alert") {
		t.Errorf("second email should carry the 4 suppressed reports:\n%s", sent[1].text)
	}

	// The send reset the count.
	h.clock.Advance(15 * time.Minute)
	h.report("job-systemic", "transformer timed out")
	if sent = h.mailer.sends(); strings.Contains(sent[2].text, "more since") {
		t.Errorf("third email repeats a count already reported:\n%s", sent[2].text)
	}
}

func TestCategoriesDoNotSuppressEachOther(t *testing.T) {
	h := newHarness(t)

	h.report("job-systemic", "transformer crashed")
	h.report("delivery", "reply send failing")
	h.report("restart", "restarted after exit code 1")

	if sent := h.mailer.sends(); len(sent) != 3 {
		t.Fatalf("3 categories at once sent %d emails, want 3", len(sent))
	}
}

// The hourly cap counts sends in every category together, and an alert it
// holds back is counted against its category like a cooldown suppression,
// even though that category has never sent.
func TestHourlyCapHoldsAcrossCategories(t *testing.T) {
	h := newHarness(t)

	for i := range 11 {
		h.report(fmt.Sprintf("cat-%02d", i), "failure")
	}
	if sent := h.mailer.sends(); len(sent) != 10 {
		t.Fatalf("11 categories in one hour sent %d emails, want 10", len(sent))
	}
	if !strings.Contains(h.logs.String(), "hourly cap") {
		t.Errorf("the capped alert should be logged; log:\n%s", h.logs)
	}

	h.clock.Advance(time.Hour)
	h.report("cat-10", "failure")
	sent := h.mailer.sends()
	if len(sent) != 11 {
		t.Fatalf("an hour later the cap should reopen; %d emails in total, want 11", len(sent))
	}
	if !strings.Contains(sent[10].text, "1 more since the last alert") {
		t.Errorf("the reopened category should carry its capped alert:\n%s", sent[10].text)
	}
}

// The daily cap binds even when the hourly one would allow a send. The 20th
// email in a rolling 24h is the cap notice, and as the oldest send leaves the
// window one slot reopens, whose send is again the notice.
func TestDailyCapHoldsAcrossCategoriesAndHours(t *testing.T) {
	h := newHarness(t)
	start := h.clock.Now()

	// 20 categories, 10 minutes apart: 6 an hour, well under the hourly cap.
	for i := range 20 {
		h.report(fmt.Sprintf("cat-%02d", i), fmt.Sprintf("failure %d", i))
		h.clock.Advance(10 * time.Minute)
	}
	sent := h.mailer.sends()
	if len(sent) != 20 {
		t.Fatalf("20 alerts across 3+ hours sent %d emails, want 20", len(sent))
	}
	for i, s := range sent[:19] {
		if strings.Contains(s.subject, capNotice) {
			t.Fatalf("email %d is marked as the cap notice: %q", i+1, s.subject)
		}
	}
	notice := sent[19]
	if notice.subject != "[FileMill] failure 19 ("+capNotice+")" {
		t.Errorf("20th subject = %q, want the alert marked as the cap notice", notice.subject)
	}
	// The oldest send leaves the window 24h after it went out.
	until := start.Add(24 * time.Hour).Format("2006-01-02 15:04 MST")
	if !strings.Contains(notice.text, "logged only until "+until) {
		t.Errorf("notice should say alerts are logged until %s:\n%s", until, notice.text)
	}
	if !strings.Contains(notice.text, "detail for failure 19") {
		t.Errorf("the notice must still carry the alert it replaced:\n%s", notice.text)
	}

	h.report("cat-20", "failure 20")
	if n := len(h.mailer.sends()); n != 20 {
		t.Fatalf("the 21st alert in 24h was sent (%d emails); the daily cap must hold", n)
	}
	if !strings.Contains(h.logs.String(), "daily cap") {
		t.Errorf("the capped alert should be logged; log:\n%s", h.logs)
	}

	// Exactly 24h after the first send it has left the window: one slot.
	h.clock.Advance(start.Add(24 * time.Hour).Sub(h.clock.Now()))
	h.report("cat-21", "failure 21")
	h.report("cat-22", "failure 22")
	sent = h.mailer.sends()
	if len(sent) != 21 {
		t.Fatalf("with one slot reopened, %d emails in total, want 21", len(sent))
	}
	if !strings.Contains(sent[20].subject, capNotice) {
		t.Errorf("the send filling the reopened slot is the 20th in the window, so the notice: %q", sent[20].subject)
	}
}

// A crash-looping worker gets a new Emailer on every restart. Reading the cap
// from the ledger, not from memory, is what keeps it from spending the whole
// Mailgun budget.
func TestDailyCapSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	for i := range 20 {
		h.report(fmt.Sprintf("cat-%02d", i), "failure")
		h.clock.Advance(10 * time.Minute)
	}

	h.restart()
	h.report("restart", "restarted after exit code 2")

	if n := len(h.mailer.sends()); n != 20 {
		t.Fatalf("a restarted Emailer sent past the daily cap: %d emails, want 20", n)
	}
}

func TestCooldownSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.report("restart", "restarted after exit code 1")

	for range 3 {
		h.clock.Advance(2 * time.Minute)
		h.restart()
		h.report("restart", "restarted after exit code 1")
	}
	if n := len(h.mailer.sends()); n != 1 {
		t.Fatalf("restarts inside the cooldown sent %d emails, want 1", n)
	}

	h.clock.Advance(15 * time.Minute)
	h.restart()
	h.report("restart", "restarted after exit code 1")
	sent := h.mailer.sends()
	if len(sent) != 2 || !strings.Contains(sent[1].text, "3 more since the last alert") {
		t.Fatalf("after the cooldown, want a 2nd email carrying 3 suppressed restarts; got %d emails:\n%v", len(sent), sent)
	}
}

// A failed send is logged and dropped. It still counts against the caps: a
// send that timed out may have been delivered and charged by Mailgun anyway.
func TestFailedSendIsLoggedNotRetried(t *testing.T) {
	h := newHarness(t)
	h.mailer.err = errors.New("mailgun: 500 Internal Server Error")

	h.report("delivery", "reply send failing")

	if n := len(h.mailer.sends()); n != 1 {
		t.Fatalf("send attempts = %d, want exactly 1", n)
	}
	if !strings.Contains(h.logs.String(), "500 Internal Server Error") {
		t.Errorf("the failure should be logged; log:\n%s", h.logs)
	}
	if n := len(h.e.queue); n != 0 {
		t.Errorf("a failed send was re-reported: %d alerts queued", n)
	}
	if n := len(h.ledger.sent); n != 1 {
		t.Errorf("recorded sends = %d, want 1 (the attempt counts)", n)
	}

	h.report("delivery", "reply send failing")
	if n := len(h.mailer.sends()); n != 1 {
		t.Fatalf("a failed alert was retried: %d attempts", n)
	}
}

// Without throttle state the caps can't be enforced, so the alert is dropped
// rather than sent blind.
func TestLedgerErrorDropsAlert(t *testing.T) {
	h := newHarness(t)
	h.ledger.err = errors.New("database is locked")

	h.report("worker-claim", "job claim failing")

	if n := len(h.mailer.sends()); n != 0 {
		t.Fatalf("sent %d emails without throttle state, want 0", n)
	}
	if !strings.Contains(h.logs.String(), "database is locked") {
		t.Errorf("the ledger error should be logged; log:\n%s", h.logs)
	}
}

func TestMailerPanicIsContained(t *testing.T) {
	h := newHarness(t)
	h.mailer.panicWith = "boom"

	h.report("publish", "sheets publish failing")

	if !strings.Contains(h.logs.String(), "boom") {
		t.Errorf("the panic should be logged; log:\n%s", h.logs)
	}
	h.mailer.panicWith = nil
	h.report("intake", "intake failing")
	if n := len(h.mailer.sends()); n != 2 {
		t.Fatalf("the Emailer should keep working after a panic; %d sends, want 2", n)
	}
}

func TestReportNeverBlocksWhenQueueIsFull(t *testing.T) {
	h := newHarness(t) // Run is never started, so nothing drains the queue
	extra := 5

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range cap(h.e.queue) + extra {
			h.e.Report(Alert{Category: "intake", Summary: fmt.Sprintf("intake failing %d", i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Report blocked on a full queue")
	}

	if n := len(h.e.queue); n != cap(h.e.queue) {
		t.Errorf("queue holds %d alerts, want %d", n, cap(h.e.queue))
	}
	if n := strings.Count(h.logs.String(), "queue full"); n != extra {
		t.Errorf("logged %d drops, want %d; log:\n%s", n, extra, h.logs)
	}
}

func TestRunSendsWhileRunning(t *testing.T) {
	h := newHarness(t)
	h.mailer.notify = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.e.Run(ctx)
	}()

	h.e.Report(Alert{Category: "panic", Summary: "job panicked"})
	select {
	case <-h.mailer.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not send a reported alert")
	}
	cancel()
	<-done
}

// An alert raised during shutdown still goes out: Run drains what is queued
// before it returns.
func TestRunDrainsQueueOnShutdown(t *testing.T) {
	h := newHarness(t)
	for _, category := range []string{"intake", "delivery", "sweep-jobs"} {
		h.e.Report(Alert{Category: category, Summary: category + " failing"})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.e.Run(ctx)

	if n := len(h.mailer.sends()); n != 3 {
		t.Fatalf("shutdown drain sent %d emails, want 3", n)
	}
}

// A summary is the subject line; one carrying an error's newlines must not
// break it.
func TestSubjectIsOneLine(t *testing.T) {
	h := newHarness(t)
	h.report("intake", "intake failed:\nopen data/inbox: access denied\r\n")

	subject := h.mailer.sends()[0].subject
	if subject != "[FileMill] intake failed: open data/inbox: access denied" {
		t.Errorf("subject = %q", subject)
	}
}

func TestNopReportDoesNothing(t *testing.T) {
	Nop{}.Report(Alert{Category: "panic", Summary: "ignored"})
}
