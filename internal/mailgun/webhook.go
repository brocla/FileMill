package mailgun

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"filemill/internal/app"
)

// requestOverhead is the slack, on top of the per-attachment limit, allowed for
// a request body's form fields and multipart framing.
const requestOverhead = 1 << 20 // 1 MiB

// handle is the webhook entry point. It delegates the decision to receive and
// then maps the single (status, reason) result onto the HTTP response: reasons
// are logged (useful for a public endpoint), and 4xx/5xx additionally send an
// error body so Mailgun can distinguish "don't retry" (4xx) from "retry" (5xx).
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
//	400 - the client's fault; do not retry (malformed body, oversize)
//	401 - bad, missing, or stale signature
//	405 - not a POST
//	500 - our fault; please retry (storage, filesystem, submit failures)
//
// A rejected attachment (wrong file type) is the sender's fault and can never
// succeed on retry, so it maps to 200, not 500 — returning 500 made Mailgun
// retry an unprocessable message for hours. Every outcome past the signature
// check names the (now verified) sender, so failures are attributable.
func (s *Service) receive(r *http.Request) (status int, reason string) {
	if r.Method != http.MethodPost {
		return http.StatusMethodNotAllowed, "method not allowed"
	}
	if err := parseForm(r); err != nil {
		return http.StatusBadRequest, "malformed request body"
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
			return http.StatusOK, fmt.Sprintf("WARNING: message declares %d attachment(s) but none arrived inline — is the Mailgun route using store(notify=) instead of forward(url)? (to %s from %s)", declared, recipient, sender)
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
		return http.StatusBadRequest, fmt.Sprintf("attachment exceeds size limit (to %s from %s)", recipient, sender)
	}

	if err := s.intake(r, operation, files); err != nil {
		if errors.Is(err, app.ErrRejected) {
			// Permanent: the sender's input is unprocessable. Drop it (200) so
			// Mailgun stops retrying, and record who sent it and why.
			return http.StatusOK, fmt.Sprintf("attachment rejected from %s: %v", sender, err)
		}
		return http.StatusInternalServerError, fmt.Sprintf("intake failed from %s: %v", sender, err)
	}
	return http.StatusOK, fmt.Sprintf("accepted: sender=%q recipient=%q operation=%s", sender, r.FormValue("recipient"), operation)
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
