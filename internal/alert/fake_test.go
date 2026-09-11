package alert

import (
	"bytes"
	"context"
	"log"
	"slices"
	"sync"
	"testing"
	"time"
)

// sentAlert is one message the fake mailer was asked to send.
type sentAlert struct{ to, subject, text string }

// fakeMailer records every send. Tests read it through sends(), which copies
// under the lock, because Run calls SendAlert from its own goroutine.
type fakeMailer struct {
	mu        sync.Mutex
	sent      []sentAlert
	err       error         // when set, every send is recorded and then fails with it
	panicWith any           // when set, every send is recorded and then panics with it
	notify    chan struct{} // when set, receives one value per send
}

func (m *fakeMailer) SendAlert(ctx context.Context, to, subject, text string) error {
	m.mu.Lock()
	m.sent = append(m.sent, sentAlert{to, subject, text})
	err, panicWith := m.err, m.panicWith
	m.mu.Unlock()
	if m.notify != nil {
		m.notify <- struct{}{}
	}
	if panicWith != nil {
		panic(panicWith)
	}
	return err
}

func (m *fakeMailer) sends() []sentAlert {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

// fakeLedger is an in-memory Ledger with the store's semantics: a category can
// hold a suppressed count before it has ever sent, and a send resets it.
type fakeLedger struct {
	mu         sync.Mutex
	lastSent   map[string]time.Time
	suppressed map[string]int
	sent       []time.Time // every recorded send, oldest first
	err        error       // when set, every method fails with it
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{lastSent: map[string]time.Time{}, suppressed: map[string]int{}}
}

func (l *fakeLedger) LastAlert(category string) (time.Time, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return time.Time{}, 0, l.err
	}
	return l.lastSent[category], l.suppressed[category], nil
}

func (l *fakeLedger) RecordAlertSent(category string, at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.lastSent[category] = at
	l.suppressed[category] = 0
	l.sent = append(l.sent, at)
	return nil
}

func (l *fakeLedger) RecordAlertSuppressed(category string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.suppressed[category]++
	return nil
}

func (l *fakeLedger) AlertSendsSince(t time.Time) ([]time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	var out []time.Time
	for _, at := range l.sent {
		if at.After(t) {
			out = append(out, at)
		}
	}
	return out, nil
}

// fakeClock is the Emailer's injected clock. It only moves when a test
// advances it, so the throttle windows are crossed without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuffer is a log sink that is safe to read while Run may still write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const testRecipient = "support@example.com"

// harness bundles an Emailer with the fakes behind it. Its ledger, mailer and
// clock outlive the Emailer, so restart can model a new process over the same
// database.
type harness struct {
	e      *Emailer
	mailer *fakeMailer
	ledger *fakeLedger
	clock  *fakeClock
	logs   *syncBuffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{mailer: &fakeMailer{}, ledger: newFakeLedger(), clock: newFakeClock(), logs: &syncBuffer{}}
	h.restart()
	return h
}

// restart replaces the Emailer, as a worker restart would, keeping the
// ledger. Only persisted state survives it.
func (h *harness) restart() {
	h.e = NewEmailer(h.mailer, h.ledger, testRecipient, Config{}, h.clock.Now, log.New(h.logs, "", 0))
}

// report runs one alert through the throttle synchronously, the way Run does
// for each alert it takes off the queue.
func (h *harness) report(category, summary string) {
	h.e.handle(context.Background(), Alert{Category: category, Summary: summary, Detail: "detail for " + summary})
}
