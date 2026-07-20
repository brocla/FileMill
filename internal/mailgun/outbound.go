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
// finished, marking each delivered only after Mailgun accepts it so a transient
// send failure is retried on the next tick.
func (s *Service) deliverPending(ctx context.Context) error {
	subs, err := s.engine.PendingEmails()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		terminal := true
		for _, item := range sub.Jobs {
			if item.Job.Status == "queued" || item.Job.Status == "running" {
				terminal = false
				break
			}
		}
		if !terminal {
			continue
		}

		var lines []string
		var outputs []string
		for _, item := range sub.Jobs {
			lines = append(lines, fmt.Sprintf("%s: %s", item.Job.InputName, item.Job.Message))
			if item.Job.Status != "succeeded" {
				continue
			}
			files, err := s.engine.Outputs(item.Job.ID)
			if err != nil {
				return err
			}
			for _, f := range files {
				outputs = append(outputs, f.Path)
			}
		}

		if err := s.send(ctx, sub.Sender, sub.Subject, sub.MessageID, strings.Join(lines, "\n"), outputs); err != nil {
			return err
		}
		if err := s.engine.MarkEmailDelivered(sub.ID); err != nil {
			return err
		}
	}
	return nil
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
