package mailgun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filemill/internal/store"
)

const testSupportLine = "Questions, problems, or suggestions? Your feedback is welcome at support@example.com."

// supportFixture is a delivery fixture with a support address configured.
func supportFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := newDeliveryFixture(t)
	f.service.support = "support@example.com"
	return f
}

// An attachment reply ends with the support line, set off by a blank line.
func TestDeliverEndsAttachmentReplyWithSupportLine(t *testing.T) {
	f := supportFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")

	text := deliverOne(t, f)

	if !strings.HasSuffix(text, "\n\n"+testSupportLine) {
		t.Errorf("reply must end with the support line after a blank line; got %q", text)
	}
}

// In a link reply the support line comes after the links, so it closes the
// message rather than splitting the report from its link.
func TestDeliverPutsSupportLineAfterTheLinks(t *testing.T) {
	f := supportFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "schedule.xlsx")

	text := deliverOne(t, f)

	link := strings.Index(text, "https://docs.google.com/spreadsheets/d/drive-file-1/edit")
	support := strings.Index(text, testSupportLine)
	if link < 0 || support < 0 || support < link {
		t.Errorf("support line must follow the link; got %q", text)
	}
	if !strings.HasSuffix(text, testSupportLine) {
		t.Errorf("reply must end with the support line; got %q", text)
	}
}

// A failed job is where a sender most needs to know where to turn.
func TestDeliverIncludesSupportLineWhenTheJobFailed(t *testing.T) {
	f := supportFixture(t)
	f.addSubmission(t, 1, "excel@mill.test")
	f.engine.pending[0].Jobs[0].Job.Status = store.StatusFailed
	f.engine.pending[0].Jobs[0].Job.Message = "no worker table found in the PDF"

	text := deliverOne(t, f)

	if !strings.Contains(text, "no worker table found in the PDF") {
		t.Errorf("reply must still carry the failure message; got %q", text)
	}
	if !strings.HasSuffix(text, testSupportLine) {
		t.Errorf("a failure reply must end with the support line; got %q", text)
	}
}

// With no support address the reply is exactly what it was before the footer
// existed.
func TestDeliverOmitsSupportLineWhenUnconfigured(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")

	text := deliverOne(t, f)

	if text != "schedule.pdf (workerlist_sheets): ok" {
		t.Errorf("reply = %q, want the unadorned job line", text)
	}
}

// Alerts go to the operator, who is the support address; inviting them to
// write to themselves would be noise.
func TestSendAlertCarriesNoSupportLine(t *testing.T) {
	var text string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		text = r.FormValue("text")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s := sendTestService(server.URL, &http.Client{Timeout: time.Second})
	s.support = "support@example.com"

	if err := s.SendAlert(context.Background(), "ops@example.com", "[FileMill] test", "the detail"); err != nil {
		t.Fatalf("SendAlert: %v", err)
	}

	if text != "the detail" {
		t.Errorf("alert text = %q, want it unchanged", text)
	}
}

func TestWithSupportFooter(t *testing.T) {
	if got := withSupportFooter("body", ""); got != "body" {
		t.Errorf("no address: got %q, want the body unchanged", got)
	}
	if got, want := withSupportFooter("body", "support@example.com"), "body\n\n"+testSupportLine; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSupportAddress(t *testing.T) {
	if got, err := supportAddress(fileConfig{}); err != nil || got != "" {
		t.Errorf("no key = %q, %v; want the footer off", got, err)
	}
	if got, err := supportAddress(fileConfig{SupportAddress: " support@example.com "}); err != nil || got != "support@example.com" {
		t.Errorf("padded key = %q, %v; want it trimmed", got, err)
	}
	if _, err := supportAddress(fileConfig{SupportAddress: "support"}); err == nil {
		t.Error("a support_address that is not an address must fail at load")
	}
}
