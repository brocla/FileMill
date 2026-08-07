package mailgun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filemill/internal/app"
	"filemill/internal/store"
)

// deliveryPollInterval is how often the delivery loop checks for submission
// groups whose jobs have all finished.
const deliveryPollInterval = time.Second

// Deliver runs the outbound loop until ctx is cancelled: once every job in a
// submission group is terminal, it mails one threaded reply carrying every
// successful output. The inbound handler returns as soon as jobs are queued;
// this loop owns the reply.
func (s *Service) Deliver(ctx context.Context) {
	ticker := time.NewTicker(deliveryPollInterval)
	defer ticker.Stop()
	for {
		if err := s.deliverPending(ctx); err != nil {
			s.log.Printf("delivery: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
// now a stuck submission is retried on every tick, and its log line is the
// signal that something needs attention.
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
		}
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
func (s *Service) deliver(ctx context.Context, sub store.EmailSubmission) error {
	var lines []string
	var outputs []app.OutputFile
	for _, item := range sub.Jobs {
		lines = append(lines, fmt.Sprintf("%s: %s", item.Job.InputName, item.Job.Message))
		if item.Job.Status != store.StatusSucceeded {
			continue
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
		text = withLinks(text, links)
	} else {
		for _, f := range outputs {
			attachments = append(attachments, f.Path)
		}
	}

	if err := s.send(ctx, sub.Sender, sub.Subject, threadingID(sub.MessageID), text, attachments); err != nil {
		return err
	}
	return s.engine.MarkEmailDelivered(sub.ID)
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
		return nil, fmt.Errorf("sheets-link delivery is configured for this route but no publisher is available")
	}
	links := make([]string, 0, len(outputs))
	for i, out := range outputs {
		record, published, err := s.engine.Delivery(submissionID, i)
		if err != nil {
			return nil, fmt.Errorf("look up published output %d: %w", i, err)
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
			return nil, fmt.Errorf("publish %s: %w", out.Name, err)
		}
		if err := s.engine.PutDelivery(submissionID, i, fileID, link); err != nil {
			// The upload succeeded but is now unrecorded, so a retry would
			// re-upload it. Name the orphan so it can be found by hand.
			return nil, fmt.Errorf("record published file %s (now orphaned in Drive): %w", fileID, err)
		}
		links = append(links, link)
	}
	return links, nil
}

// withLinks appends the published spreadsheet links to a reply body.
func withLinks(text string, links []string) string {
	if len(links) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n\nYour spreadsheet is ready. Anyone with the link can view and edit it:\n")
	for _, link := range links {
		b.WriteString("\n")
		b.WriteString(link)
	}
	return b.String()
}

// send posts one reply to Mailgun's Send API, threaded into the original
// conversation and carrying the given output files as attachments.
func (s *Service) send(ctx context.Context, to, subject, messageID, text string, outputs []string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	fields := map[string]string{
		"from":    s.from,
		"to":      to,
		"subject": replySubject(subject),
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
