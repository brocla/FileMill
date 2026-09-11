package alert

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// testCategory records a test send in the Ledger apart from real alerts, so it
// never holds a real category in its cooldown.
const testCategory = "alert-test"

// SendTest sends one test alert to to, straight through the Mailer. The
// throttle is bypassed: the operator asked for exactly this message and needs
// to see whether it arrives. The send is still recorded in the Ledger, since
// it spends one of the day's Mailgun sends like any alert. A Ledger failure
// doesn't stop the send; every error is returned.
func SendTest(ctx context.Context, m Mailer, l Ledger, to string, now time.Time) error {
	recordErr := l.RecordAlertSent(testCategory, now)
	if recordErr != nil {
		recordErr = fmt.Errorf("record the test send: %w", recordErr)
	}
	text := fmt.Sprintf("This is a test alert from `filemill alert-test`, sent %s.\n\n"+
		"It reached you, so operator alerts work: FileMill emails its systemic failures to %s, from its reply address. "+
		"If this landed in spam, mark it as not spam so real alerts don't.\n\n"+
		"It counts toward the daily alert cap like any alert.\n",
		now.Format("2006-01-02 15:04 MST"), to)
	sendErr := m.SendAlert(ctx, to, subjectPrefix+"test alert", text)
	if sendErr != nil {
		sendErr = fmt.Errorf("send the test alert: %w", sendErr)
	}
	return errors.Join(sendErr, recordErr)
}
