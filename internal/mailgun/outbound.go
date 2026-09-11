package mailgun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filemill/internal/alert"
	"filemill/internal/app"
	"filemill/internal/store"
)

// deliveryPollInterval is how often the delivery loop checks for submission
// groups whose jobs have all finished.
const deliveryPollInterval = time.Second

// deliveryAlertAfter is how long a submission must keep failing before it is
// reported. Delivery retries every second, and a Mailgun or Drive blip clears
// well inside this.
const deliveryAlertAfter = 5 * time.Minute

// Deliver runs the outbound loop until ctx is cancelled: once every job in a
// submission group is terminal, it mails one threaded reply carrying every
// successful output. The inbound handler returns as soon as jobs are queued;
// this loop owns the reply.
func (s *Service) Deliver(ctx context.Context) {
	ticker := time.NewTicker(deliveryPollInterval)
	defer ticker.Stop()
	for {
		s.deliverTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverTick runs one pass of the delivery loop. A panic is logged and
// reported rather than ending the loop, which would leave the worker running
// with no replies going out.
func (s *Service) deliverTick(ctx context.Context) {
	defer s.recoverLoop("delivery loop")
	if err := s.deliverPending(ctx); err != nil {
		s.log.Printf("delivery: %v", err)
	}
}

// deliverPending sends a reply for every pending submission whose jobs have all
// finished.
//
// A submission that cannot be delivered is logged and skipped, not returned.
// Returning would stall every submission behind it: a transient Mailgun blip
// clears on the next tick, but a sheets-link failure (expired token, quota, a
// Drive outage) can persist for hours and would otherwise silently halt all
// replies — including plain-attachment ones with nothing to do with it. Only a
// failure to read the queue itself aborts the tick.
//
// Per-submission failure counting and a dead-letter status are a follow-up; for
// now a stuck submission is retried on every tick. Its log line, and an alert
// once it has failed for deliveryAlertAfter, are the signal that something
// needs attention.
func (s *Service) deliverPending(ctx context.Context) error {
	subs, err := s.engine.PendingEmails()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !finished(sub) {
			continue
		}
		if err := s.deliver(ctx, sub); err != nil {
			s.log.Printf("delivery: submission %d (from %s): %v", sub.ID, sub.Sender, err)
			s.deliveryFailed(sub, err)
			continue
		}
		delete(s.failing, sub.ID)
	}
	return nil
}

// finished reports whether every job in a submission has reached a terminal
// status, so the submission can be replied to as a whole.
func finished(sub store.EmailSubmission) bool {
	for _, item := range sub.Jobs {
		if item.Job.Status == store.StatusQueued || item.Job.Status == store.StatusRunning {
			return false
		}
	}
	return true
}

// deliver replies to one finished submission. It is marked delivered only after
// Mailgun accepts the reply, so a transient send failure is retried on the next
// tick; under sheets-link delivery the upload is already recorded by then and
// is not repeated.
//
// Delivery is therefore at-least-once (#6), and two windows can still send a
// reply twice: a crash between Mailgun accepting the reply and the mark, and a
// send that times out on our side after Mailgun accepted it. Each costs one
// duplicate, sent after the restart or on the next tick. That is accepted: a
// duplicate reply is harmless, and closing the window would take either a
// "delivering" state, which only trades a rare duplicate for a rare lost
// reply, or a provider-side idempotency key.
//
// A mark that fails while the process runs is not accepted: the submission
// stays pending and would be re-sent on every tick. markDelivered remembers
// it, so later ticks retry the mark alone. That memory dies with the process,
// so each restart before the mark succeeds sends one more duplicate.
func (s *Service) deliver(ctx context.Context, sub store.EmailSubmission) error {
	if s.sentUnmarked[sub.ID] {
		return s.markDelivered(sub.ID)
	}
	var lines []string
	var outputs []app.OutputFile
	var labels []string
	for _, item := range sub.Jobs {
		label := s.engine.OperationLabel(item.Job.Operation)
		lines = append(lines, fmt.Sprintf("%s (%s): %s",
			item.Job.InputName, label, item.Job.Message))
		if item.Job.Status != store.StatusSucceeded {
			continue
		}
		if label != "" {
			labels = appendDistinct(labels, label)
		}
		files, err := s.engine.Outputs(item.Job.ID)
		if err != nil {
			return fmt.Errorf("read outputs of job %s: %w", item.Job.ID, err)
		}
		outputs = append(outputs, files...)
	}
	text := strings.Join(lines, "\n")

	// A link reply is the same send with no attachments: the attachment loop
	// simply has nothing to walk.
	var attachments []string
	if s.deliveryMode(sub.Recipient) == modeSheetsLink {
		links, err := s.publish(ctx, sub.ID, outputs)
		if err != nil {
			return err
		}
		text = withLinks(text, links, labels)
	} else {
		for _, f := range outputs {
			attachments = append(attachments, f.Path)
		}
	}

	if err := s.send(ctx, sub.Sender, replySubject(sub.Subject), threadingID(sub.MessageID), text, attachments); err != nil {
		return err
	}
	return s.markDelivered(sub.ID)
}

// markDelivered records a sent reply as delivered. On failure it remembers the
// submission, so deliver retries only the mark and never the send: re-sending
// on every 1-second tick would flood the sender with duplicates and spend the
// Mailgun Free plan's 100 sends a day in under two minutes.
func (s *Service) markDelivered(id int64) error {
	if err := s.engine.MarkEmailDelivered(id); err != nil {
		if s.sentUnmarked == nil {
			s.sentUnmarked = map[int64]bool{}
		}
		s.sentUnmarked[id] = true
		return failedAt("delivery-mark", fmt.Errorf("reply sent, but marking it delivered failed (retrying the mark only): %w", err))
	}
	delete(s.sentUnmarked, id)
	return nil
}

// deliveryError labels a delivery failure with the alert category it belongs
// to. An unlabelled error is a failing reply: "delivery".
type deliveryError struct {
	category string
	err      error
}

func (e *deliveryError) Error() string { return e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

func failedAt(category string, err error) error { return &deliveryError{category, err} }

func categoryOf(err error) string {
	var labelled *deliveryError
	if errors.As(err, &labelled) {
		return labelled.category
	}
	return "delivery"
}

// deliveryOutage is one submission's unbroken run of failed deliveries.
type deliveryOutage struct {
	since    time.Time
	reported bool
}

// deliveryFailed reports a failing submission once per outage: after
// deliveryAlertAfter, or at once for a failed mark, where every restart before
// it heals sends another duplicate. An orphaned Drive file is reported every
// time, since each names a different file that only a person can delete.
// deliverPending ends the outage when the submission is delivered.
func (s *Service) deliveryFailed(sub store.EmailSubmission, err error) {
	now := s.clock()
	category := categoryOf(err)
	if category == "publish-orphan" {
		s.report(alert.Alert{
			Category: category,
			Summary:  "published Drive file orphaned",
			Detail: submissionDetail(sub, err) +
				"\nThe file was uploaded to Google Drive, but its record was not saved, so the retention sweep will never delete it. Delete it by hand.\n",
		})
		category = "publish" // the submission itself still gets the grace period
	}

	if s.failing == nil {
		s.failing = map[int64]*deliveryOutage{}
	}
	outage := s.failing[sub.ID]
	if outage == nil {
		outage = &deliveryOutage{since: now}
		s.failing[sub.ID] = outage
	}
	grace := deliveryAlertAfter
	if category == "delivery-mark" {
		grace = 0
	}
	if outage.reported || now.Sub(outage.since) < grace {
		return
	}
	outage.reported = true
	s.report(alert.Alert{
		Category: category,
		Summary:  outageSummary(category),
		Detail:   fmt.Sprintf("%s\nFailing since %s, retried every second.\n", submissionDetail(sub, err), outage.since.Format(time.RFC3339)),
	})
}

func outageSummary(category string) string {
	minutes := int(deliveryAlertAfter.Minutes())
	switch category {
	case "delivery-mark":
		return "reply sent but not recorded as delivered"
	case "publish":
		return fmt.Sprintf("sheets-link publish failing for %d minutes", minutes)
	}
	return fmt.Sprintf("reply delivery failing for %d minutes", minutes)
}

func submissionDetail(sub store.EmailSubmission, err error) string {
	return fmt.Sprintf("Submission: %d\nFrom: %s\nTo: %s\nSubject: %s\n\nError: %v\n", sub.ID, sub.Sender, sub.Recipient, sub.Subject, err)
}

// deliveryMode returns how replies to a recipient address are delivered.
func (s *Service) deliveryMode(recipient string) string {
	if mode := s.delivery[strings.ToLower(recipient)]; mode != "" {
		return mode
	}
	return modeEmail
}

// publish uploads each output that has not been published yet and returns every
// link, in output order.
//
// Recording the upload happens immediately after it, before the fallible email
// step — the same commit-point shape intake uses. Email delivery is
// at-least-once, and a duplicate email is harmless; a duplicate upload is not,
// since it would leave a second world-editable copy of the sender's data that
// nothing will clean up. Net: at-most-once upload, at-least-once email.
//
// The residual window matches intake's: a crash between Publish returning and
// PutDelivery committing orphans one Drive file, which the retry then
// re-uploads. It is small, self-limited to one file, and the retention sweep
// never learns about the orphan — the accepted cost of not running a
// distributed transaction against Google.
func (s *Service) publish(ctx context.Context, submissionID int64, outputs []app.OutputFile) ([]string, error) {
	if s.publisher == nil {
		return nil, failedAt("publish", fmt.Errorf("sheets-link delivery is configured for this route but no publisher is available"))
	}
	links := make([]string, 0, len(outputs))
	for i, out := range outputs {
		record, published, err := s.engine.Delivery(submissionID, i)
		if err != nil {
			return nil, failedAt("publish", fmt.Errorf("look up published output %d: %w", i, err))
		}
		if published {
			links = append(links, record.Link)
			continue
		}
		// The Sheet is named after the output without its extension: Drive
		// shows a native Sheet, so a trailing ".xlsx" would be a lie.
		title := strings.TrimSuffix(out.Name, filepath.Ext(out.Name))
		fileID, link, err := s.publisher.Publish(ctx, out.Path, title)
		if err != nil {
			return nil, failedAt("publish", fmt.Errorf("publish %s: %w", out.Name, err))
		}
		if err := s.engine.PutDelivery(submissionID, i, fileID, link); err != nil {
			// The upload succeeded but is now unrecorded, so a retry would
			// re-upload it. Name the orphan so it can be found by hand.
			return nil, failedAt("publish-orphan", fmt.Errorf("record published file %s (now orphaned in Drive): %w", fileID, err))
		}
		links = append(links, link)
	}
	return links, nil
}

// appendDistinct adds value to values unless it is already there, preserving
// order. The lists are a handful of labels at most, so a linear scan beats
// carrying a set alongside them.
func appendDistinct(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// withLinks appends the published spreadsheet links to a reply body.
//
// `labels` names the reports that succeeded. When they are all the same one --
// the ordinary case, since a submission is routed to a single operation -- the
// reply names it, so a reader who asked for two different reports over the same
// file can tell which reply is which. A mixed submission falls back to the
// generic wording rather than picking one of them to name.
func withLinks(text string, links, labels []string) string {
	if len(links) == 0 {
		return text
	}
	// "spreadsheet" unless every succeeded job produced the same report, in
	// which case that report's own name is more use to the reader.
	what := "spreadsheet"
	if len(labels) == 1 {
		what = labels[0]
	}
	sentence := fmt.Sprintf(
		"Your %s is ready. Anyone with the link can view and edit it:", what)
	if len(links) > 1 {
		// A label is a report's name, not a countable noun, so it is pluralized
		// by what it names rather than by an "s" -- "your X files", not "your Xs".
		plural := "spreadsheets"
		if len(labels) == 1 {
			plural = what + " files"
		}
		sentence = fmt.Sprintf(
			"Your %s are ready. Anyone with the link can view and edit them:", plural)
	}

	var b strings.Builder
	b.WriteString(text)
	fmt.Fprintf(&b, "\n\n%s\n", sentence)
	for _, link := range links {
		b.WriteString("\n")
		b.WriteString(link)
	}
	return b.String()
}

// SendAlert sends one plain-text operator alert from REPLY_FROM, with its
// subject as given: no "Re:", no threading, no attachments. It satisfies
// alert.Mailer. It never reports its own failure: the alert channel is the
// broken thing, and reporting it would only loop.
func (s *Service) SendAlert(ctx context.Context, to, subject, text string) error {
	return s.send(ctx, to, subject, "", text, nil)
}

// send posts one message to Mailgun's Send API with its subject as given.
// With a messageID it is threaded into that conversation, and it carries the
// given output files as attachments.
func (s *Service) send(ctx context.Context, to, subject, messageID, text string, outputs []string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	fields := map[string]string{
		"from":    s.from,
		"to":      to,
		"subject": subject,
		"text":    text,
	}
	if messageID != "" {
		fields["h:In-Reply-To"] = messageID
		fields["h:References"] = messageID
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return err
		}
	}
	for _, path := range outputs {
		if err := attachFile(writer, path); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}

	base := s.sendBase
	if base == "" {
		base = mailgunAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v3/%s/messages", base, s.domain), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.SetBasicAuth("api", s.apiKey)

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Include a bounded slice of Mailgun's error body — it carries the
		// actual reason (bad key, unverified domain, etc.), which a bare
		// status code hides.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("mailgun send returned %s: %s", resp.Status, bytes.TrimSpace(detail))
	}
	return nil
}

// attachFile copies one output file into the outbound multipart body.
func attachFile(writer *multipart.Writer, path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	part, err := writer.CreateFormFile("attachment", filepath.Base(path))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, in)
	return err
}

// threadingID returns the value for the reply's In-Reply-To/References headers.
// Intake stores a synthetic "mailgun:<token>" idempotency key when the inbound
// mail carried no real Message-Id; that is not a valid message id, so threading
// on it would emit a malformed header. Return "" in that case (send then omits
// the headers) and the real Message-Id otherwise.
func threadingID(idempotencyKey string) string {
	if strings.HasPrefix(idempotencyKey, "mailgun:") {
		return ""
	}
	return idempotencyKey
}

// replySubject prefixes "Re:" unless the subject already carries it.
func replySubject(subject string) string {
	if subject == "" {
		return "Re: FileMill result"
	}
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	return "Re: " + subject
}
