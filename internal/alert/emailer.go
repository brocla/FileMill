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
//
// A failing Ledger does not silence alerts: a sick database is the likeliest
// cause of the failures worth alerting on (worker-claim, intake). The Emailer
// keeps an in-memory copy of the throttle state, in step with the Ledger while
// it works, and after the first Ledger error it throttles from that copy for
// the rest of the process. It never goes back: a Ledger whose writes failed
// would read back too few sends, and the caps would leak.
type Emailer struct {
	mailer Mailer
	ledger Ledger
	to     string
	cfg    Config
	now    func() time.Time
	log    *log.Logger
	queue  chan Alert

	// mem and ledgerErr are touched only by the goroutine running handle.
	mem       *memLedger
	ledgerErr error // the first Ledger error; non-nil means throttling from mem
}

func NewEmailer(m Mailer, l Ledger, to string, cfg Config, now func() time.Time, log *log.Logger) *Emailer {
	return &Emailer{
		mailer: m, ledger: l, to: to, cfg: cfg.withDefaults(), now: now, log: log,
		queue: make(chan Alert, queueSize),
		mem:   newMemLedger(),
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
// caps: a send that timed out may have reached Mailgun and been charged.
func (e *Emailer) handle(ctx context.Context, a Alert) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Printf("alert %s: panic while sending: %v; dropped: %s", a.Category, r, a.Summary)
		}
	}()

	now := e.now()
	sentAt, suppressed, sends := e.readState(a.Category, now.Add(-day))
	if !sentAt.IsZero() && now.Sub(sentAt) < e.cfg.Cooldown {
		e.suppress(a, "cooldown")
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

	e.mem.recordSent(a.Category, now)
	if err := e.ledger.RecordAlertSent(a.Category, now); err != nil {
		e.ledgerFailed(err)
	}
	// Composed after recording, so an email whose own record failed says so.
	subject, text := e.compose(a, suppressed, capReopens)
	if err := e.mailer.SendAlert(ctx, e.to, subject, text); err != nil {
		e.log.Printf("alert %s: send failed: %v; dropped: %s", a.Category, err, a.Summary)
		return
	}
	e.log.Printf("alert %s: sent: %s", a.Category, a.Summary)
}

// readState returns category's last send and suppressed count, and every send
// after since, from the Ledger while it works and from mem after it fails.
func (e *Emailer) readState(category string, since time.Time) (time.Time, int, []time.Time) {
	if e.ledgerErr == nil {
		sentAt, suppressed, err := e.ledger.LastAlert(category)
		if err == nil {
			var sends []time.Time
			if sends, err = e.ledger.AlertSendsSince(since); err == nil {
				e.mem.sync(category, sentAt, suppressed, sends)
				return sentAt, suppressed, sends
			}
		}
		e.ledgerFailed(err)
	}
	return e.mem.read(category, since)
}

// ledgerFailed switches the throttle to mem for the rest of the process. Only
// the first error is logged; later writes are still attempted, so the next
// process inherits as much history as the Ledger will take.
func (e *Emailer) ledgerFailed(err error) {
	if e.ledgerErr != nil {
		return
	}
	e.ledgerErr = err
	e.log.Printf("alert: throttle ledger failed: %v; throttling in memory until restart", err)
}

// suppress logs a held-back alert and counts it against its category.
func (e *Emailer) suppress(a Alert, reason string) {
	e.log.Printf("alert %s: suppressed (%s): %s", a.Category, reason, a.Summary)
	e.mem.recordSuppressed(a.Category)
	if err := e.ledger.RecordAlertSuppressed(a.Category); err != nil {
		e.ledgerFailed(err)
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
	if e.ledgerErr != nil {
		fmt.Fprintf(&b, "\nThe alert throttle's database state failed (%v), so it is throttling in memory until the worker restarts.\n", e.ledgerErr)
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

// memLedger is the Emailer's in-memory copy of the throttle state. While the
// Ledger works, every read syncs it; afterwards it is the throttle. Its send
// history comes from the last good read plus every send since, so the global
// caps carry over a Ledger failure. A category the Ledger was never read for
// starts with no cooldown, but the caps still bound it.
type memLedger struct {
	lastSent   map[string]time.Time
	suppressed map[string]int
	sends      []time.Time
}

func newMemLedger() *memLedger {
	return &memLedger{lastSent: map[string]time.Time{}, suppressed: map[string]int{}}
}

func (m *memLedger) sync(category string, sentAt time.Time, suppressed int, sends []time.Time) {
	m.lastSent[category] = sentAt
	m.suppressed[category] = suppressed
	m.sends = slices.Clone(sends)
}

func (m *memLedger) read(category string, since time.Time) (time.Time, int, []time.Time) {
	var sends []time.Time
	for _, at := range m.sends {
		if at.After(since) {
			sends = append(sends, at)
		}
	}
	return m.lastSent[category], m.suppressed[category], sends
}

func (m *memLedger) recordSent(category string, at time.Time) {
	m.lastSent[category] = at
	m.suppressed[category] = 0
	m.sends = append(slices.DeleteFunc(m.sends, func(t time.Time) bool { return !t.After(at.Add(-day)) }), at)
}

func (m *memLedger) recordSuppressed(category string) { m.suppressed[category]++ }
