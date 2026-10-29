package mailgun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"filemill/internal/alert"
	"filemill/internal/app"
)

// requestOverhead is the slack, on top of the per-attachment limit, allowed for
// a request body's form fields and multipart framing.
const requestOverhead = 1 << 20 // 1 MiB

// handle is the webhook entry point. It delegates the decision to receive and
// then maps the single (status, reason) result onto the HTTP response: reasons
// are logged (useful for a public endpoint), and 4xx/5xx additionally send an
// error body. Mailgun itself only distinguishes 406 ("don't retry") from every
// other non-200 code ("retry").
func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	// Unconditional arrival log: any request that reaches this handler is
	// recorded before any check, so "did the POST arrive?" is never ambiguous.
	s.log.Printf("webhook received: method=%s content-type=%q length=%d", r.Method, r.Header.Get("Content-Type"), r.ContentLength)
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBytes+requestOverhead)
	status, reason := s.receive(r)
	if reason != "" {
		s.log.Printf("webhook status=%d %s", status, reason)
	}
	if status >= 400 {
		http.Error(w, reason, status)
		return
	}
	w.WriteHeader(status)
}

// receive validates and processes one webhook, returning the HTTP status to
// send and a short reason (empty when there is nothing worth logging). The
// taxonomy is deliberate:
//
//	200 - accepted, or "received, nothing to do" (Mailgun should stop)
//	401 - bad, missing, or stale signature
//	405 - not a POST
//	406 - can never succeed; do not retry (malformed body, oversize)
//	500 - our fault or the network's; please retry (cut-off upload, storage,
//	      filesystem, submit failures)
//
// Mailgun retries every non-200 response except 406, on a fixed schedule over
// 8 hours, so a permanent refusal must be 406 - a 400 is retried just like a
// 500.
//
// A rejected attachment (wrong file type) is the sender's fault and can never
// succeed on retry, so it maps to 200, not 500 — returning 500 made Mailgun
// retry an unprocessable message for hours. Every outcome past the signature
// check names the (now verified) sender, so failures are attributable.
func (s *Service) receive(r *http.Request) (status int, reason string) {
	if r.Method != http.MethodPost {
		return http.StatusMethodNotAllowed, "method not allowed"
	}
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body
	if status, reason, failed := bodyFailure(parseForm(r), body, r.ContentLength); failed {
		return status, reason
	}
	if !authenticSignature(s.signKey, r.FormValue("timestamp"), r.FormValue("token"), r.FormValue("signature")) {
		return http.StatusUnauthorized, "invalid signature"
	}

	// The request is now trusted, so the sender field is verified and named in
	// every subsequent outcome. The remaining "nothing to do" outcomes return
	// 200 so Mailgun does not retry a request there is no point reprocessing.
	sender := r.FormValue("sender")
	recipient := r.FormValue("recipient")
	files := attachments(r)
	if len(files) == 0 {
		// A message that says it carried attachments but posted none inline is
		// not an empty email — it is the signature of a Mailgun route using
		// store(notify=) rather than forward(url), which posts metadata and
		// leaves the bytes behind a storage URL. Every real submission would
		// vanish here, looking exactly like ordinary empty mail, so name it.
		if declared := declaredAttachments(r); declared > 0 {
			reason := fmt.Sprintf("WARNING: message declares %d attachment(s) but none arrived inline — is the Mailgun route using store(notify=) instead of forward(url)? (to %s from %s)", declared, recipient, sender)
			s.report(alert.Alert{
				Category: "route-config",
				Summary:  "Mailgun route is not forwarding attachments",
				Detail:   reason + "\n\nEvery submission to this address is lost until its Mailgun route uses forward(url).\n",
			})
			return http.StatusOK, reason
		}
		// Routine, but still recorded: with nothing logged at all, a message
		// that arrived and one that never did looked identical.
		return http.StatusOK, fmt.Sprintf("no attachments (to %s from %s)", recipient, sender)
	}
	operation, ok := s.operationFor(recipient)
	if !ok {
		return http.StatusOK, fmt.Sprintf("unrouted recipient %s (from %s)", recipient, sender)
	}
	if !s.senderAllowed(sender) {
		return http.StatusOK, "sender not allowed " + sender
	}

	// One email is one unit of work. Two processable attachments are ambiguous —
	// there is one reply, and with link delivery one Drive upload, so a message
	// carrying several is dropped rather than guessed at. The sender is told
	// nothing; the log is the record.
	files = s.processable(operation, files)
	switch len(files) {
	case 0:
		return http.StatusOK, fmt.Sprintf("no attachment %s can process (to %s from %s)", operation, recipient, sender)
	case 1:
	default:
		return http.StatusOK, fmt.Sprintf("ignoring message with %d processable attachments; one per email (to %s from %s)", len(files), recipient, sender)
	}

	if !s.withinSizeLimit(files) {
		return http.StatusNotAcceptable, fmt.Sprintf("attachment exceeds size limit (to %s from %s)", recipient, sender)
	}

	if err := s.intake(r, operation, files); err != nil {
		if errors.Is(err, app.ErrRejected) {
			// Permanent: the sender's input is unprocessable. Drop it (200) so
			// Mailgun stops retrying, and record who sent it and why.
			return http.StatusOK, fmt.Sprintf("attachment rejected from %s: %v", sender, err)
		}
		s.report(alert.Alert{
			Category: "intake",
			Summary:  "webhook intake failing",
			Detail: fmt.Sprintf("Recipient: %s\nSender: %s\nOperation: %s\n\nError: %v\n\nThe webhook returned 500, so Mailgun will retry this message for several hours.\n",
				recipient, sender, operation, err),
		})
		return http.StatusInternalServerError, fmt.Sprintf("intake failed from %s: %v", sender, err)
	}
	return http.StatusOK, fmt.Sprintf("accepted: sender=%q recipient=%q operation=%s %s", sender, r.FormValue("recipient"), operation, s.arrival(r))
}

// countingBody counts the bytes read from a request body and keeps the first
// read error, so a body that would not parse can be told apart: cut off in
// transit, or complete but malformed.
type countingBody struct {
	io.ReadCloser
	n   int64
	err error
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

// bodyFailure judges the request body after parsing, reporting failed when the
// request must not go on. The rest of the body is drained first: the parser
// stops at its first error, often long before the end, and it also takes a
// clean end-of-stream between parts as the end of the form - so a body cut
// short there parses without error, and only the byte count shows it is
// incomplete. A cut-off upload (Mailgun gives up after 10 seconds) is not the
// sender's fault and is retried (500); a body that is too large, or arrived
// whole and still would not parse, can never succeed and is refused (406).
func bodyFailure(parseErr error, body *countingBody, declared int64) (status int, reason string, failed bool) {
	_, _ = io.Copy(io.Discard, body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(parseErr, &tooLarge) || errors.As(body.err, &tooLarge):
		return http.StatusNotAcceptable, fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit), true
	case body.err != nil:
		return http.StatusInternalServerError, fmt.Sprintf("request body incomplete: got %d bytes: %v", body.n, body.err), true
	case declared > 0 && body.n < declared:
		return http.StatusInternalServerError, fmt.Sprintf("request body incomplete: got %d of %d bytes", body.n, declared), true
	case parseErr != nil:
		return http.StatusNotAcceptable, fmt.Sprintf("malformed request body: %v", parseErr), true
	}
	return 0, "", false
}

// arrival describes when an email was sent (its Date header), when Mailgun
// posted it (the signed timestamp), and the lag from sending to arrival here.
// Without these, a slow reply could not be placed: before the sender's email
// reached Mailgun, between Mailgun and here, or after.
func (s *Service) arrival(r *http.Request) string {
	posted := "unknown"
	if unix, err := strconv.ParseInt(r.FormValue("timestamp"), 10, 64); err == nil {
		posted = time.Unix(unix, 0).UTC().Format(time.RFC3339)
	}
	sent, ok := sentAt(r)
	if !ok {
		return "sent=unknown posted=" + posted
	}
	lag := s.clock().Sub(sent).Round(time.Second)
	return fmt.Sprintf("sent=%s posted=%s lag=%s", sent.UTC().Format(time.RFC3339), posted, lag)
}

// sentAt reads the email's Date header. Mailgun posts headers as top-level
// fields (Message-Id is read the same way), and also as the message-headers
// JSON list of [name, value] pairs, which is the fallback.
func sentAt(r *http.Request) (time.Time, bool) {
	raw := r.FormValue("Date")
	if raw == "" {
		var headers [][]string
		_ = json.Unmarshal([]byte(r.FormValue("message-headers")), &headers)
		for _, h := range headers {
			if len(h) == 2 && strings.EqualFold(h[0], "Date") {
				raw = h[1]
				break
			}
		}
	}
	sent, err := mail.ParseDate(raw)
	return sent, err == nil
}

// parseForm decodes the request body. Mailgun sends no-attachment notifications
// as application/x-www-form-urlencoded and attachment notifications as
// multipart/form-data, so both must be handled.
func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(defaultMaxBytes)
	}
	return r.ParseForm()
}

// declaredAttachments reports how many attachments Mailgun says the message
// carried, which is not the same as how many actually arrived. A forwarded
// message reports attachment-count; a stored one lists them as JSON instead.
// Either shape, paired with no inline parts, means the bytes were not sent.
func declaredAttachments(r *http.Request) int {
	if n, err := strconv.Atoi(r.FormValue("attachment-count")); err == nil && n > 0 {
		return n
	}
	raw := r.FormValue("attachments")
	if raw == "" {
		return 0
	}
	var stored []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return 0
	}
	return len(stored)
}

// attachments returns the attachment file parts (attachment-1, attachment-2,
// ...) in field-name order so their submission indices are stable.
func attachments(r *http.Request) []*multipart.FileHeader {
	if r.MultipartForm == nil {
		return nil
	}
	fields := make([]string, 0, len(r.MultipartForm.File))
	for field := range r.MultipartForm.File {
		if strings.HasPrefix(field, "attachment-") {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields) // orders attachment-1..attachment-9 deterministically
	var files []*multipart.FileHeader
	for _, field := range fields {
		files = append(files, r.MultipartForm.File[field]...)
	}
	return files
}
