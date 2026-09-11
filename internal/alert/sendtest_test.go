package alert

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var testSendTime = time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

// alert-test proves the channel works before anything relies on it: one
// message to the configured recipient, through the same Mailer and subject
// prefix real alerts use.
func TestSendTestSendsOneMessageAndCountsIt(t *testing.T) {
	m, l := &fakeMailer{}, newFakeLedger()

	if err := SendTest(context.Background(), m, l, testRecipient, testSendTime); err != nil {
		t.Fatalf("SendTest: %v", err)
	}

	sent := m.sends()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	if sent[0].to != testRecipient || sent[0].subject != "[FileMill] test alert" {
		t.Errorf("sent to %q with subject %q", sent[0].to, sent[0].subject)
	}
	if !strings.Contains(sent[0].text, testRecipient) {
		t.Errorf("the body should say where alerts go:\n%s", sent[0].text)
	}
	// It spends one of the day's Mailgun sends like any alert, so it counts
	// against the daily cap, which then stays exact.
	if len(l.sent) != 1 || !l.sent[0].Equal(testSendTime) {
		t.Errorf("recorded sends = %v, want the test send at %v", l.sent, testSendTime)
	}
}

func TestSendTestReturnsAFailedSend(t *testing.T) {
	m := &fakeMailer{err: errors.New("mailgun send returned 401 Unauthorized: Forbidden")}

	err := SendTest(context.Background(), m, newFakeLedger(), testRecipient, testSendTime)

	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("SendTest = %v, want the send's error", err)
	}
}

// A ledger that can't record the send must not stop the test: the operator
// asked for it, and the ledger's failure is worth hearing about too.
func TestSendTestStillSendsWhenTheLedgerFails(t *testing.T) {
	m, l := &fakeMailer{}, newFakeLedger()
	l.writeErr = errors.New("database is locked")

	err := SendTest(context.Background(), m, l, testRecipient, testSendTime)

	if n := len(m.sends()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("SendTest = %v, want the ledger's error", err)
	}
}
