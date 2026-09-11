package mailgun

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filemill/internal/alert"
	"filemill/internal/app"
)

var _ alert.Mailer = (*Service)(nil)

// fakeReporter records every alert. The Service reports from the goroutine
// that calls it, so these tests need no locking.
type fakeReporter struct{ alerts []alert.Alert }

func (r *fakeReporter) Report(a alert.Alert) { r.alerts = append(r.alerts, a) }

// only returns the alerts in one category.
func (r *fakeReporter) only(category string) []alert.Alert {
	var out []alert.Alert
	for _, a := range r.alerts {
		if a.Category == category {
			out = append(out, a)
		}
	}
	return out
}

// fakeClock stands in for the Service's clock, so a grace period is crossed
// without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newAlertingFixture is a deliveryFixture whose Service reports to a
// fakeReporter and reads a fakeClock.
func newAlertingFixture(t *testing.T) (*deliveryFixture, *fakeReporter, *fakeClock) {
	t.Helper()
	f := newDeliveryFixture(t)
	rep := &fakeReporter{}
	clock := &fakeClock{t: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)}
	f.service.SetReporter(rep)
	f.service.now = clock.now
	return f, rep, clock
}

func tick(t *testing.T, f *deliveryFixture) {
	t.Helper()
	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}
}

// A failing reply is retried every second, and a Mailgun blip clears on its
// own, so it reports only once a submission has failed for five minutes, and
// then once, not on every tick after.
func TestDeliveryFailureReportsAfterFiveMinutes(t *testing.T) {
	f, rep, clock := newAlertingFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")
	f.mailgun.failTo["sender1@example.com"] = true

	tick(t, f)
	clock.advance(5*time.Minute - time.Second)
	tick(t, f)
	if len(rep.alerts) != 0 {
		t.Fatalf("reported inside the grace period: %+v", rep.alerts)
	}

	clock.advance(time.Second)
	tick(t, f)
	got := rep.only("delivery")
	if len(got) != 1 || len(rep.alerts) != 1 {
		t.Fatalf("after 5 minutes of failing, alerts = %+v; want one delivery", rep.alerts)
	}
	for _, want := range []string{"sender1@example.com", "simulated mailgun failure"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, got[0].Detail)
		}
	}

	clock.advance(time.Minute)
	tick(t, f)
	if len(rep.alerts) != 1 {
		t.Errorf("reported again in the same outage: %d alerts", len(rep.alerts))
	}
}

// A failure that clears inside the grace period is the blip the grace period
// exists for.
func TestDeliveryBlipStaysSilent(t *testing.T) {
	f, rep, clock := newAlertingFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")
	f.mailgun.failTo["sender1@example.com"] = true

	tick(t, f)
	clock.advance(4 * time.Minute)
	f.mailgun.failTo = map[string]bool{}
	tick(t, f)
	clock.advance(10 * time.Minute)
	tick(t, f)

	if !f.engine.delivered[1] {
		t.Fatal("the submission should have been delivered once Mailgun recovered")
	}
	if len(rep.alerts) != 0 {
		t.Errorf("a failure that cleared within the grace period reported: %+v", rep.alerts)
	}
}

func TestPublishFailureReportsAfterFiveMinutes(t *testing.T) {
	f, rep, clock := newAlertingFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "schedule.xlsx")
	f.publisher.err = fmt.Errorf("google token expired")

	tick(t, f)
	clock.advance(5 * time.Minute)
	tick(t, f)

	got := rep.only("publish")
	if len(got) != 1 || len(rep.alerts) != 1 {
		t.Fatalf("alerts = %+v; want one publish", rep.alerts)
	}
	if !strings.Contains(got[0].Detail, "google token expired") {
		t.Errorf("detail lacks the publish error:\n%s", got[0].Detail)
	}
}

// A reply that went out but can't be marked delivered means the database is
// failing, and every restart until it recovers sends another duplicate, so it
// skips the grace period.
func TestMarkFailureReportsAtOnce(t *testing.T) {
	f, rep, clock := newAlertingFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")
	f.engine.markErr = fmt.Errorf("disk I/O error")

	tick(t, f)
	if got := rep.only("delivery-mark"); len(got) != 1 || len(rep.alerts) != 1 {
		t.Fatalf("alerts = %+v; want one delivery-mark at the first failure", rep.alerts)
	}

	clock.advance(10 * time.Minute)
	tick(t, f)
	if len(rep.alerts) != 1 {
		t.Errorf("retrying the mark reported again: %d alerts", len(rep.alerts))
	}
}

// An upload whose record failed leaves a world-editable file in Drive that the
// retention sweep will never delete. Each one is reported at once, naming the
// file, since only a person can clean it up.
func TestOrphanedDriveFileReportsAtOnce(t *testing.T) {
	f, rep, _ := newAlertingFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "schedule.xlsx")
	f.engine.putDeliveryErr = fmt.Errorf("database is locked")

	tick(t, f)
	tick(t, f) // the retry uploads again, orphaning a second file

	got := rep.only("publish-orphan")
	if len(got) != 2 {
		t.Fatalf("orphan alerts = %+v; want one per orphaned file", got)
	}
	for i, id := range []string{"drive-file-1", "drive-file-2"} {
		if !strings.Contains(got[i].Detail, id) {
			t.Errorf("orphan alert %d does not name %s:\n%s", i+1, id, got[i].Detail)
		}
	}
	if len(rep.only("publish")) != 0 {
		t.Error("the publish grace period still applies to the submission itself")
	}
}

// A 500 from intake is our fault. Mailgun retries it for hours, and the
// throttle absorbs the burst.
func TestIntakeFailureReports(t *testing.T) {
	engine := newFakeEngine()
	engine.submitErr = fmt.Errorf("disk full")
	s := routedService(engine)
	rep := &fakeReporter{}
	s.SetReporter(rep)

	r := signedMultipart(t, "k",
		map[string]string{"recipient": "workerlist@mill.example.com", "sender": "kevin@example.com", "Message-Id": "<orig-3@example.com>"},
		map[string][]byte{"attachment-1": []byte("pdf")})
	if status, _ := s.receive(r); status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}

	got := rep.only("intake")
	if len(got) != 1 || len(rep.alerts) != 1 {
		t.Fatalf("alerts = %+v; want one intake", rep.alerts)
	}
	for _, want := range []string{"disk full", "kevin@example.com"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, got[0].Detail)
		}
	}
}

// Webhook noise and the sender's own mistakes are not FileMill failures.
func TestRoutineWebhookOutcomesDoNotReport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*fakeEngine)
		request func(t *testing.T) *http.Request
	}{
		{"rejected attachment", func(e *fakeEngine) {
			e.submitErr = fmt.Errorf(`workerlist does not accept ".html": %w`, app.ErrRejected)
		}, func(t *testing.T) *http.Request {
			return signedMultipart(t, "k", map[string]string{"recipient": "workerlist@mill.example.com", "sender": "kevin@example.com"},
				map[string][]byte{"attachment-1": []byte("<html>")})
		}},
		{"unrouted recipient", nil, func(t *testing.T) *http.Request {
			return signedMultipart(t, "k", map[string]string{"recipient": "nobody@mill.example.com", "sender": "kevin@example.com"},
				map[string][]byte{"attachment-1": []byte("pdf")})
		}},
		{"forged signature", nil, func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", strings.NewReader("timestamp=1&token=x&signature=forged"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			if tc.prepare != nil {
				tc.prepare(engine)
			}
			s := routedService(engine)
			rep := &fakeReporter{}
			s.SetReporter(rep)

			s.receive(tc.request(t))

			if len(rep.alerts) != 0 {
				t.Errorf("reported: %+v", rep.alerts)
			}
		})
	}
}

// Attachments declared but not posted mean the Mailgun route stores messages
// instead of forwarding them, and every real submission is being lost.
func TestStoredRouteWarningReports(t *testing.T) {
	s := routedService(newFakeEngine())
	rep := &fakeReporter{}
	s.SetReporter(rep)
	ts, token, sig := signedFields(s.signKey)
	body := "timestamp=" + ts + "&token=" + token + "&signature=" + sig +
		"&recipient=workerlist%40mill.example.com&sender=someone%40example.com&attachment-count=2"
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	s.receive(r)

	got := rep.only("route-config")
	if len(got) != 1 || !strings.Contains(got[0].Detail, "store(notify") {
		t.Fatalf("alerts = %+v; want one route-config naming the likely cause", rep.alerts)
	}
}

// A published file that won't delete is world-editable data outliving the
// 30-day promise. The sweep reports once for the batch, naming every file.
func TestSweepReportsDriveDeleteFailures(t *testing.T) {
	f, rep, _ := newAlertingFixture(t)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	f.age(1, 0, "stuck-file", old)
	f.age(2, 0, "other-file", old)
	f.publisher.deleteErr = fmt.Errorf("drive unavailable")

	if err := f.service.sweepExpired(context.Background()); err != nil {
		t.Fatalf("sweepExpired: %v", err)
	}

	got := rep.only("sweep-drive")
	if len(got) != 1 {
		t.Fatalf("alerts = %+v; want one sweep-drive for the batch", rep.alerts)
	}
	for _, want := range []string{"stuck-file", "other-file", "drive unavailable"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, got[0].Detail)
		}
	}
}

func TestCleanSweepDoesNotReport(t *testing.T) {
	f, rep, _ := newAlertingFixture(t)
	f.age(1, 0, "old-file", time.Now().UTC().Add(-31*24*time.Hour))

	if err := f.service.sweepExpired(context.Background()); err != nil {
		t.Fatalf("sweepExpired: %v", err)
	}
	if len(rep.alerts) != 0 {
		t.Errorf("a clean sweep reported: %+v", rep.alerts)
	}
}

// The reply adds "Re:" itself, so send can carry an alert's subject verbatim.
func TestReplySubjectCarriesRe(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")

	deliverOne(t, f)

	if got := f.mailgun.sent[0].subject; got != "Re: Schedule" {
		t.Errorf("reply subject = %q, want %q", got, "Re: Schedule")
	}
}

// An alert is a plain message: its subject as given, from REPLY_FROM, with no
// threading headers and no attachments.
func TestSendAlertPostsAPlainMessage(t *testing.T) {
	var fields map[string][]string
	var files int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		fields, files = r.MultipartForm.Value, len(r.MultipartForm.File)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s := sendTestService(server.URL, &http.Client{Timeout: time.Second})

	if err := s.SendAlert(context.Background(), "support@example.com", "[FileMill] intake failing", "the detail"); err != nil {
		t.Fatalf("SendAlert: %v", err)
	}

	for field, want := range map[string]string{
		"to":      "support@example.com",
		"from":    "filemill@mill.test",
		"subject": "[FileMill] intake failing",
		"text":    "the detail",
	} {
		if got := fields[field]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %q", field, got, want)
		}
	}
	if _, ok := fields["h:In-Reply-To"]; ok {
		t.Error("an alert must not thread into a conversation")
	}
	if files != 0 {
		t.Errorf("an alert carried %d attachments, want none", files)
	}
}

func TestAlertSettings(t *testing.T) {
	to, cfg, err := alertSettings(fileConfig{})
	if err != nil || to != "" || cfg != (alert.Config{}) {
		t.Fatalf("no alert keys = %q, %+v, %v; want alerting off and the defaults", to, cfg, err)
	}

	to, cfg, err = alertSettings(fileConfig{
		AlertRecipient: " support@example.com ", AlertCooldownMinutes: 20, AlertMaxPerHour: 5, AlertMaxPerDay: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := alert.Config{Cooldown: 20 * time.Minute, MaxPerHour: 5, MaxPerDay: 12}
	if to != "support@example.com" || cfg != want {
		t.Errorf("settings = %q, %+v; want support@example.com, %+v", to, cfg, want)
	}

	if _, _, err := alertSettings(fileConfig{AlertRecipient: "support"}); err == nil {
		t.Error("an alert_recipient that is not an address must fail at load")
	}
}

// A Service built without SetReporter, as most tests build it, must not panic
// on a path that reports.
func TestServiceWithoutReporterDoesNotPanic(t *testing.T) {
	engine := newFakeEngine()
	engine.submitErr = errors.New("disk full")
	s := routedService(engine)
	r := signedMultipart(t, "k",
		map[string]string{"recipient": "workerlist@mill.example.com", "sender": "kevin@example.com"},
		map[string][]byte{"attachment-1": []byte("pdf")})

	if status, _ := s.receive(r); status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
}
