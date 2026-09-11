package alert

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// Throttle defaults. The daily cap is the binding one: the Mailgun Free plan
// allows 100 sends a day, shared with replies, and rejects every send past
// that until the next day. The hourly cap alone would allow 240. 20 keeps
// alerts to a fifth of the budget.
const (
	DefaultCooldown   = 15 * time.Minute
	DefaultMaxPerHour = 10
	DefaultMaxPerDay  = 20
)

const (
	// day is the rolling window of the daily cap.
	day = 24 * time.Hour
	// queueSize bounds the alerts waiting for Run. A flood that fills it
	// would be throttled anyway, so Report drops rather than waits.
	queueSize = 64
	// drainTimeout bounds how long Run keeps sending after ctx is cancelled,
	// inside main's 10s shutdown window.
	drainTimeout  = 5 * time.Second
	subjectPrefix = "[FileMill] "
)

// Config tunes the throttle. A zero or negative field takes its default.
type Config struct {
	Cooldown   time.Duration // per category
	MaxPerHour int           // across categories
	MaxPerDay  int           // across categories, rolling 24h
}

func (c Config) withDefaults() Config {
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.MaxPerHour <= 0 {
		c.MaxPerHour = DefaultMaxPerHour
	}
	if c.MaxPerDay <= 0 {
		c.MaxPerDay = DefaultMaxPerDay
	}
	return c
}

// Emailer is the real Reporter: a throttle in front of a Mailer, draining a
// buffered channel on its own goroutine so Report never blocks a hot path.
//
// Every alert passes a per-category cooldown and two global caps, all read
// from the Ledger. A suppressed alert is logged and counted, and the next
// email in its category says how many were held back.
type Emailer struct {
	mailer Mailer
	ledger Ledger
	to     string
	cfg    Config
	now    func() time.Time
	log    *log.Logger
	queue  chan Alert
}

func NewEmailer(m Mailer, l Ledger, to string, cfg Config, now func() time.Time, log *log.Logger) *Emailer {
	return &Emailer{
		mailer: m, ledger: l, to: to, cfg: cfg.withDefaults(), now: now, log: log,
		queue: make(chan Alert, queueSize),
	}
}

// Report queues a for Run and returns at once. A full queue drops the alert.
func (e *Emailer) Report(a Alert) {
	select {
	case e.queue <- a:
	default:
		e.log.Printf("alert %s: queue full; dropped: %s", a.Category, a.Summary)
	}
}

// Run sends queued alerts until ctx is cancelled, then sends whatever is still
// queued, for up to drainTimeout, so an alert raised during shutdown still
// goes out. It returns when the drain is done.
func (e *Emailer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			e.drain(ctx)
			return
		case a := <-e.queue:
			e.handle(ctx, a)
		}
	}
}

func (e *Emailer) drain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	for {
		select {
		case a := <-e.queue:
			if ctx.Err() != nil {
				e.log.Printf("alert %s: shutdown drain timed out; dropped: %s", a.Category, a.Summary)
				continue
			}
			e.handle(ctx, a)
		default:
			return
		}
	}
}

// handle throttles one alert and sends it if it passes.
//
// The send is recorded before it is attempted, and a failed send is logged
// and dropped, never retried or re-reported. It still counts against the
// caps: a send that timed out may have reached Mailgun and been charged. For
// the same reason the alert is dropped when the Ledger can't be read or
// written: without it the caps can't be enforced.
func (e *Emailer) handle(ctx context.Context, a Alert) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Printf("alert %s: panic while sending: %v; dropped: %s", a.Category, r, a.Summary)
		}
	}()

	now := e.now()
	sentAt, suppressed, err := e.ledger.LastAlert(a.Category)
	if err != nil {
		e.log.Printf("alert %s: read throttle state: %v; dropped: %s", a.Category, err, a.Summary)
		return
	}
	if !sentAt.IsZero() && now.Sub(sentAt) < e.cfg.Cooldown {
		e.suppress(a, "cooldown")
		return
	}
	sends, err := e.ledger.AlertSendsSince(now.Add(-day))
	if err != nil {
		e.log.Printf("alert %s: read send history: %v; dropped: %s", a.Category, err, a.Summary)
		return
	}
	if sentAfter(sends, now.Add(-time.Hour)) >= e.cfg.MaxPerHour {
		e.suppress(a, "hourly cap")
		return
	}
	if len(sends) >= e.cfg.MaxPerDay {
		e.suppress(a, "daily cap")
		return
	}

	// The send that fills the daily cap says so. It counts within the cap, so
	// the cap stays exact, and the slot reopens when the oldest send in the
	// window, possibly this one, leaves it.
	capReopens := time.Time{}
	if len(sends) == e.cfg.MaxPerDay-1 {
		capReopens = slices.MinFunc(append(sends, now), time.Time.Compare).Add(day)
	}
	subject, text := e.compose(a, suppressed, capReopens)

	if err := e.ledger.RecordAlertSent(a.Category, now); err != nil {
		e.log.Printf("alert %s: record send: %v; dropped: %s", a.Category, err, a.Summary)
		return
	}
	if err := e.mailer.SendAlert(ctx, e.to, subject, text); err != nil {
		e.log.Printf("alert %s: send failed: %v; dropped: %s", a.Category, err, a.Summary)
		return
	}
	e.log.Printf("alert %s: sent: %s", a.Category, a.Summary)
}

// suppress logs a held-back alert and counts it against its category.
func (e *Emailer) suppress(a Alert, reason string) {
	e.log.Printf("alert %s: suppressed (%s): %s", a.Category, reason, a.Summary)
	if err := e.ledger.RecordAlertSuppressed(a.Category); err != nil {
		e.log.Printf("alert %s: record suppression: %v", a.Category, err)
	}
}

// compose builds the email. A non-zero capReopens marks it as the cap notice.
func (e *Emailer) compose(a Alert, suppressed int, capReopens time.Time) (subject, text string) {
	summary := oneLine(a.Summary)
	if summary == "" {
		summary = a.Category
	}
	subject = subjectPrefix + summary

	var b strings.Builder
	if !capReopens.IsZero() {
		subject += " (daily alert cap reached)"
		fmt.Fprintf(&b, "Daily alert cap reached: this is alert %d of %d in 24 hours. Further alerts are logged only until %s.\n\n",
			e.cfg.MaxPerDay, e.cfg.MaxPerDay, capReopens.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(&b, "%s\n\nCategory: %s\n", summary, a.Category)
	if a.Detail != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(a.Detail, "\n"))
	}
	if suppressed > 0 {
		fmt.Fprintf(&b, "\n%d more since the last alert in this category, suppressed by the throttle.\n", suppressed)
	}
	return subject, b.String()
}

// sentAfter counts the sends after t.
func sentAfter(sends []time.Time, t time.Time) int {
	n := 0
	for _, at := range sends {
		if at.After(t) {
			n++
		}
	}
	return n
}

// oneLine collapses whitespace, newlines included, so a summary carrying an
// error's text still makes a single subject line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
