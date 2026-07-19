package mailgun

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func signedFields(key string) (string, string, string) {
	timestamp, token := strconv.FormatInt(time.Now().Unix(), 10), "test-token"
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + token))
	return timestamp, token, hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSilentlyDropsMessageWithoutAttachments(t *testing.T) {
	s := &Service{sign: "key", max: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}}
	ts, token, sig := signedFields(s.sign)
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", bytes.NewBufferString("timestamp="+ts+"&token="+token+"&signature="+sig))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestWebhookSilentlyDropsUnknownRecipient(t *testing.T) {
	s := &Service{sign: "key", max: 1024, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}}
	ts, token, sig := signedFields(s.sign)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("timestamp", ts)
	_ = writer.WriteField("token", token)
	_ = writer.WriteField("signature", sig)
	_ = writer.WriteField("recipient", "unknown@mill.keywind.cc")
	part, _ := writer.CreateFormFile("attachment-1", "schedule.pdf")
	_, _ = part.Write([]byte("pdf"))
	_ = writer.Close()
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestWebhookRejectsForgedRequest(t *testing.T) {
	s := &Service{sign: "key", max: 1024, routes: map[string]string{}, allowed: map[string]bool{}}
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", bytes.NewBufferString("timestamp=1&token=x&signature=forged"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestWebhookRejectsOversizeAttachment(t *testing.T) {
	s := &Service{sign: "key", max: 2, routes: map[string]string{"workerlist@mill.keywind.cc": "workerlist"}, allowed: map[string]bool{}}
	ts, token, sig := signedFields(s.sign)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("timestamp", ts)
	_ = writer.WriteField("token", token)
	_ = writer.WriteField("signature", sig)
	_ = writer.WriteField("recipient", "workerlist@mill.keywind.cc")
	part, _ := writer.CreateFormFile("attachment-1", "schedule.pdf")
	_, _ = part.Write([]byte("too large"))
	_ = writer.Close()
	r := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.handle(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}
