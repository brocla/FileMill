package mailgun

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
	s := &Service{signKey: "key", maxBytes: 2, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}, log: discardLogger()}
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

// signedMultipart builds a signed multipart webhook request with the given form
// fields and attachments (field name -> content).
func signedMultipart(t *testing.T, signKey string, fields map[string]string, files map[string][]byte) *http.Request {
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
		part, err := w.CreateFormFile(name, name)
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
