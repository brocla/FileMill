// Package alert emails the operator when FileMill fails systemically.
//
// It is a leaf package because both app and mailgun report, and mailgun
// imports app, so neither can own it. See ERROR-ALERTING-PLAN.md.
package alert

import (
	"context"
	"time"
)

// Alert is one systemic failure.
type Alert struct {
	Category string // throttle key, e.g. "job-systemic"
	Summary  string // one line; becomes the subject
	Detail   string // job id, operation, input name, error, truncated output
}

// Reporter records a systemic failure. Report must not block or panic.
type Reporter interface{ Report(Alert) }

// Nop is the default Reporter: alerting disabled.
type Nop struct{}

func (Nop) Report(Alert) {}

// Mailer sends one plain-text message with its subject verbatim.
// *mailgun.Service satisfies it.
type Mailer interface {
	SendAlert(ctx context.Context, to, subject, text string) error
}

// Ledger is the persisted throttle state. *store.Store satisfies it.
//
// It lives in the database, not in memory, because the supervisor restarts a
// crashing worker every ≤120s and an in-memory throttle would reset each time.
type Ledger interface {
	// LastAlert returns when category last sent and how many of its alerts
	// have been suppressed since. A zero sentAt means it has never sent; it
	// can still have a suppressed count, held back by a global cap.
	LastAlert(category string) (sentAt time.Time, suppressed int, err error)
	// RecordAlertSent records a send at at and resets category's suppressed
	// count.
	RecordAlertSent(category string, at time.Time) error
	// RecordAlertSuppressed increments category's suppressed count.
	RecordAlertSuppressed(category string) error
	// AlertSendsSince returns the time of every send after t, in any
	// category, oldest first. It must cover at least the last 24h.
	AlertSendsSince(t time.Time) ([]time.Time, error)
}
