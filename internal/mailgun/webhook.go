package mailgun

import (
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
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

	// The request is now trusted. The remaining "nothing to do" outcomes return
	// 200 so Mailgun does not retry a request there is no point reprocessing.
	files := attachments(r)
	if len(files) == 0 {
		return http.StatusOK, "" // e.g. a no-attachment notification
	}
	operation, ok := s.operationFor(r.FormValue("recipient"))
	if !ok {
		return http.StatusOK, "unrouted recipient " + r.FormValue("recipient")
	}
	if !s.senderAllowed(r.FormValue("sender")) {
		return http.StatusOK, "sender not allowed " + r.FormValue("sender")
	}
	if !s.withinSizeLimit(files) {
		return http.StatusBadRequest, "attachment exceeds size limit"
	}

	if err := s.intake(r, operation, files); err != nil {
		return http.StatusInternalServerError, "intake failed: " + err.Error()
	}
	return http.StatusOK, ""
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
