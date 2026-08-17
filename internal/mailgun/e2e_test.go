package mailgun

import (
	"bytes"
	"context"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filemill/internal/app"
)

// Absolute paths to the real workerlist transformer and a real fixture PDF.
// The end-to-end test is environment-specific by design: it runs the genuine
// Python transformer, so it is gated behind FILEMILL_E2E and skips cleanly
// when the transformer or its fixture is not present on this machine. The
// repo path is machine-specific, so it comes from FILEMILL_WORKERLIST_REPO
// with a placeholder default that simply won't exist on most machines.
var workerlistRepo = func() string {
	if p := os.Getenv("FILEMILL_WORKERLIST_REPO"); p != "" {
		return p
	}
	return `C:\path\to\workerlist`
}()

var fixturePDF = filepath.Join(workerlistRepo, "tests", "fixtures", "sample_schedule.pdf")

// TestEndToEndThroughWorkerlist exercises the full inbound-to-outbound path:
// a signed Mailgun webhook carrying a PDF -> App.Submit -> the worker runs the
// real workerlist transformer -> deliverOnce assembles a threaded reply and
// sends it to a fake Mailgun endpoint with schedule.xlsx attached.
//
// Run with:  FILEMILL_E2E=1 go test ./internal/mailgun -run EndToEnd -v
func TestEndToEndThroughWorkerlist(t *testing.T) {
	if os.Getenv("FILEMILL_E2E") == "" {
		t.Skip("set FILEMILL_E2E=1 to run the live workerlist end-to-end test")
	}
	pythonExe := filepath.Join(workerlistRepo, ".venv", "Scripts", "python.exe")
	script := filepath.Join(workerlistRepo, "workerlist.py")
	for _, p := range []string{pythonExe, script, fixturePDF} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("workerlist environment not available: %v", p)
		}
	}

	// A self-contained FileMill root: config/transformers.yaml pointing the
	// "workerlist" operation at the real Python transformer, and a data dir
	// that App.Open creates underneath.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	transformers := "transformers:\n" +
		"  - operation: workerlist\n" +
		"    command:\n" +
		"      - '" + pythonExe + "'\n" +
		"      - '" + script + "'\n" +
		"    extensions:\n" +
		"      - pdf\n"
	if err := os.WriteFile(filepath.Join(root, "config", "transformers.yaml"), []byte(transformers), 0644); err != nil {
		t.Fatal(err)
	}

	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Fake Mailgun Send API: capture the reply the delivery loop builds.
	var gotAttachments []string
	var gotSubject, gotTo, gotInReplyTo string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("fake mailgun parse: %v", err)
			http.Error(w, "bad", 400)
			return
		}
		gotSubject = r.FormValue("subject")
		gotTo = r.FormValue("to")
		gotInReplyTo = r.FormValue("h:In-Reply-To")
		for _, fh := range r.MultipartForm.File["attachment"] {
			gotAttachments = append(gotAttachments, fh.Filename)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"<fake@mill.test>","message":"Queued"}`)
	}))
	defer fake.Close()

	s := &Service{
		engine:   a,
		signKey:  "test-signing-key",
		domain:   "mill.test",
		from:     "filemill@mill.test",
		routes:   map[string]string{"workerlist@mill.example.com": "workerlist"},
		allowed:  map[string]bool{},
		maxBytes: 20 << 20,
		sendBase: fake.URL,
		log:      log.New(io.Discard, "", 0),
	}

	// A signed inbound webhook carrying the fixture PDF as attachment-1.
	pdf, err := os.ReadFile(fixturePDF)
	if err != nil {
		t.Fatal(err)
	}
	ts, token, sig := signedFields(s.signKey)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"timestamp": ts, "token": token, "signature": sig,
		"recipient":  "workerlist@mill.example.com",
		"sender":     "kevin@example.com",
		"subject":    "Booth schedule",
		"Message-Id": "<orig-123@example.com>",
	} {
		_ = mw.WriteField(k, v)
	}
	part, _ := mw.CreateFormFile("attachment-1", "sample_schedule.pdf")
	if _, err := part.Write(pdf); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/mailgun-webhook", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook returned %d, want 200", rec.Code)
	}

	// Drive the worker: exactly one attachment -> one queued job.
	if err := a.Run(context.Background(), true); err != nil {
		t.Fatalf("worker run: %v", err)
	}

	// Deliver the reply now that the job is terminal.
	if err := s.deliverPending(context.Background()); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}

	// The reply must thread to the original and carry the produced spreadsheet.
	if gotTo != "kevin@example.com" {
		t.Errorf("reply To = %q, want kevin@example.com", gotTo)
	}
	if !strings.HasPrefix(gotSubject, "Re:") {
		t.Errorf("reply subject = %q, want Re: prefix", gotSubject)
	}
	if gotInReplyTo != "<orig-123@example.com>" {
		t.Errorf("In-Reply-To = %q, want threading header", gotInReplyTo)
	}
	if len(gotAttachments) != 1 || !strings.HasSuffix(gotAttachments[0], ".xlsx") {
		t.Fatalf("reply attachments = %v, want one .xlsx", gotAttachments)
	}
	t.Logf("end-to-end OK: reply to %s subject %q attachment %v", gotTo, gotSubject, gotAttachments)
}
