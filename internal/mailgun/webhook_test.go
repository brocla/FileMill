package mailgun

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"filemill/internal/app"
)

// signedFields returns a fresh timestamp, token, and matching HMAC signature for
// the given signing key.
func signedFields(key string) (timestamp, token, signature string) {
	timestamp, token = strconv.FormatInt(time.Now().Unix(), 10), "test-token"
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + token))
	return timestamp, token, hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSilentlyDropsMessageWithoutAttachments(t *testing.T) {
	s := &Service{signKey: "key", maxBytes: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: discardLogger()}
	ts, token, sig := signedFields(s.signKey)
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", bytes.NewBufferString("timestamp="+ts+"&token="+token+"&signature="+sig))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestWebhookSilentlyDropsUnknownRecipient(t *testing.T) {
	s := &Service{signKey: "key", maxBytes: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: discardLogger()}
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "unknown@mill.keywind.cc"},
		map[string][]byte{"attachment-1": []byte("pdf")})
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestWebhookRejectsForgedRequest(t *testing.T) {
	s := &Service{signKey: "key", maxBytes: 1024, routes: map[string]string{}, allowed: map[string]bool{}, log: discardLogger()}
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", bytes.NewBufferString("timestamp=1&token=x&signature=forged"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestWebhookRejectsOversizeAttachment(t *testing.T) {
	s := &Service{engine: newFakeEngine(), signKey: "key", maxBytes: 2, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: discardLogger()}
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc"},
		map[string][]byte{"attachment-1": []byte("too large")})
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// TestWebhookAcceptsAndDropsRejectedAttachment covers a permanent input
// rejection (e.g. a .html attachment the transformer does not accept). Retrying
// can never succeed, so the handler must return 200 to stop Mailgun retrying —
// not 500, which triggered a multi-hour retry storm.
func TestWebhookAcceptsAndDropsRejectedAttachment(t *testing.T) {
	engine := newFakeEngine()
	engine.submitErr = fmt.Errorf(`workerlist does not accept ".html": %w`, app.ErrRejected)
	logger, buf := captureLogger()
	s := &Service{engine: engine, signKey: "key", maxBytes: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: logger}
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "sender@example.com"},
		map[string][]byte{"attachment-1": []byte("<html>")})
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (permanent rejection must not be retried)", w.Code)
	}
	if !strings.Contains(buf.String(), "sender@example.com") {
		t.Errorf("rejection reason must name the sender; got %q", buf.String())
	}
}

// TestWebhookRetriesGenuineIntakeFailure guards the other side: a real,
// transient failure (storage, filesystem) must still return 500 so Mailgun
// retries. The sender is recorded either way.
func TestWebhookRetriesGenuineIntakeFailure(t *testing.T) {
	engine := newFakeEngine()
	engine.submitErr = fmt.Errorf("disk full")
	logger, buf := captureLogger()
	s := &Service{engine: engine, signKey: "key", maxBytes: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: logger}
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "sender@example.com"},
		map[string][]byte{"attachment-1": []byte("pdf")})
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 (transient failure must be retryable)", w.Code)
	}
	if !strings.Contains(buf.String(), "sender@example.com") {
		t.Errorf("failure reason must name the sender; got %q", buf.String())
	}
}

// TestWebhookLogsSenderOnAccept covers the happy path: a successfully accepted
// submission logs an "accepted" line naming the verified sender, so successful
// mail is attributable in the log and not only in the database.
func TestWebhookLogsSenderOnAccept(t *testing.T) {
	engine := newFakeEngine()
	logger, buf := captureLogger()
	s := &Service{engine: engine, signKey: "key", maxBytes: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: logger}
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "sender@example.com"},
		map[string][]byte{"attachment-1": []byte("pdf")})
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !strings.Contains(buf.String(), "accepted:") || !strings.Contains(buf.String(), "sender@example.com") {
		t.Errorf("accept line must name the sender; got %q", buf.String())
	}
}

// A message that says it carried attachments but posted none inline is the
// signature of a Mailgun route using store(notify=) instead of forward(url):
// the bytes stay in Mailgun's storage and only metadata arrives. Every real
// submission would silently vanish, and the resulting request looks exactly
// like an ordinary no-attachment email — so it has to be called out by name.
func TestWebhookWarnsWhenAttachmentsAreDeclaredButNotPosted(t *testing.T) {
	logger, buf := captureLogger()
	s := &Service{engine: newFakeEngine(), signKey: "key", maxBytes: 1024,
		routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: logger}
	ts, token, sig := signedFields(s.signKey)
	body := "timestamp=" + ts + "&token=" + token + "&signature=" + sig +
		"&recipient=workerlist%40mill.keywind.cc&sender=someone%40example.com&attachment-count=2"
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (retrying cannot fix a route misconfiguration)", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "store(notify") {
		t.Errorf("the warning must name the likely cause; got %q", out)
	}
	if !strings.Contains(out, "workerlist@mill.keywind.cc") || !strings.Contains(out, "someone@example.com") {
		t.Errorf("the warning must name the recipient and sender; got %q", out)
	}
}

// Mailgun's stored-message payload lists attachments as JSON rather than
// sending an attachment-count, so that shape must trip the same warning.
func TestWebhookWarnsOnStoredAttachmentsJSON(t *testing.T) {
	logger, buf := captureLogger()
	s := &Service{engine: newFakeEngine(), signKey: "key", maxBytes: 1024,
		routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: logger}
	ts, token, sig := signedFields(s.signKey)
	attachments := url.QueryEscape(`[{"url":"https://storage.mailgun.net/v3/domains/x/messages/y/attachments/0","name":"schedule.pdf"}]`)
	body := "timestamp=" + ts + "&token=" + token + "&signature=" + sig +
		"&recipient=workerlist%40mill.keywind.cc&sender=someone%40example.com&attachments=" + attachments
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)

	if !strings.Contains(buf.String(), "store(notify") {
		t.Errorf("a stored-attachments payload must trip the warning; got %q", buf.String())
	}
}

// A genuinely empty message is routine, so it must not raise the alarm — but it
// still says who it was from and to. Logging nothing at all made an ordinary
// test unverifiable: silence looked identical to the message never arriving.
func TestWebhookRecordsPlainEmptyMessageWithoutWarning(t *testing.T) {
	logger, buf := captureLogger()
	s := &Service{engine: newFakeEngine(), signKey: "key", maxBytes: 1024,
		routes: map[string]string{"excel@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: logger}
	ts, token, sig := signedFields(s.signKey)
	body := "timestamp=" + ts + "&token=" + token + "&signature=" + sig +
		"&recipient=excel%40mill.keywind.cc&sender=someone%40example.com"
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)

	out := buf.String()
	if strings.Contains(out, "store(notify") {
		t.Errorf("an ordinary empty message must not raise the route warning; got %q", out)
	}
	if !strings.Contains(out, "excel@mill.keywind.cc") {
		t.Errorf("the outcome must name the recipient so a test can be confirmed; got %q", out)
	}
}

// pdfRouteService returns a Service routing workerlist@ to an engine that only
// accepts .pdf — the shape the attachment-count rule is written against.
func pdfRouteService(engine *fakeEngine, logger *log.Logger) *Service {
	engine.accepted = []string{".pdf"}
	return &Service{
		engine:   engine,
		signKey:  "key",
		maxBytes: 1024,
		routes:   map[string]string{"workerlist@mill.keywind.cc": "workerlist"},
		allowed:  map[string]bool{},
		log:      logger,
	}
}

// One email carries one unit of work. Two processable attachments are ambiguous
// — which output would the reply be? — so the message is dropped with a log line
// and no reply, rather than silently producing two jobs.
func TestWebhookIgnoresEmailWithTwoAcceptableAttachments(t *testing.T) {
	engine := newFakeEngine()
	logger, buf := captureLogger()
	s := pdfRouteService(engine, logger)
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "sender@example.com"},
		map[string][]byte{"attachment-1": []byte("pdf one"), "attachment-2": []byte("pdf two")},
		map[string]string{"attachment-1": "first.pdf", "attachment-2": "second.pdf"})
	w := httptest.NewRecorder()
	s.handle(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (dropped, not retried)", w.Code)
	}
	if engine.submitCount != 0 {
		t.Errorf("no job may be submitted; got %d", engine.submitCount)
	}
	if !strings.Contains(buf.String(), "sender@example.com") {
		t.Errorf("the drop must name the sender; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "workerlist@mill.keywind.cc") {
		t.Errorf("the drop must name the recipient; got %q", buf.String())
	}
}

// An inline signature logo rides along as an extra attachment part. Counting
// every part would drop an ordinary one-PDF submission, so only attachments the
// route's transformer accepts are counted — the rest are ignored as noise.
func TestWebhookIgnoresUnprocessableAttachmentsWhenCounting(t *testing.T) {
	engine := newFakeEngine()
	s := pdfRouteService(engine, discardLogger())
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "sender@example.com"},
		map[string][]byte{"attachment-1": []byte("logo bytes"), "attachment-2": []byte("pdf")},
		map[string]string{"attachment-1": "signature-logo.png", "attachment-2": "schedule.pdf"})
	w := httptest.NewRecorder()
	s.handle(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if engine.submitCount != 1 {
		t.Fatalf("exactly the PDF must be submitted; got %d submissions", engine.submitCount)
	}
	if !strings.HasSuffix(engine.sources[0], "schedule.pdf") {
		t.Errorf("the PDF must be the submitted file; got %q", engine.sources[0])
	}
}

// Nothing the transformer can process means there is nothing to do: drop it
// (200, so Mailgun stops) without creating a job.
func TestWebhookDropsEmailWithNoAcceptableAttachment(t *testing.T) {
	engine := newFakeEngine()
	logger, buf := captureLogger()
	s := pdfRouteService(engine, logger)
	r := signedMultipart(t, s.signKey,
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "sender@example.com"},
		map[string][]byte{"attachment-1": []byte("logo bytes")},
		map[string]string{"attachment-1": "signature-logo.png"})
	w := httptest.NewRecorder()
	s.handle(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if engine.submitCount != 0 {
		t.Errorf("no job may be submitted; got %d", engine.submitCount)
	}
	// Two addresses can share one operation, so the operation alone does not
	// identify where the mail was sent.
	if !strings.Contains(buf.String(), "workerlist@mill.keywind.cc") {
		t.Errorf("the drop must name the recipient, not just the operation; got %q", buf.String())
	}
}

// signedMultipart builds a signed multipart webhook request with the given form
// fields and attachments (field name -> content). An optional filenames map
// overrides the part filename, which the attachment-count rule matches on.
func signedMultipart(t *testing.T, signKey string, fields map[string]string, files map[string][]byte, filenames ...map[string]string) *http.Request {
	t.Helper()
	ts, token, sig := signedFields(signKey)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{"timestamp": ts, "token": token, "signature": sig} {
		_ = w.WriteField(k, v)
	}
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for name, content := range files {
		filename := name
		if len(filenames) > 0 {
			if override, ok := filenames[0][name]; ok {
				filename = override
			}
		}
		part, err := w.CreateFormFile(name, filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}
