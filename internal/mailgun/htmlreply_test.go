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

const testFragment = `<table role="presentation"><tr><td style="color:#1a7f37;">All checks passed</td></tr></table>`

// htmlFixture is a delivery fixture with a support address and one finished
// submission to recipient whose job carries jobHTML.
func htmlFixture(t *testing.T, recipient, jobHTML string, outputNames ...string) *deliveryFixture {
	t.Helper()
	f := supportFixture(t)
	f.engine.labels = map[string]string{"check_schedule": "Schedule check"}
	f.addSubmissionFor(t, 1, recipient, "check_schedule", outputNames...)
	job := &f.engine.pending[0].Jobs[0].Job
	job.Message, job.MessageHTML = "All checks passed\nCONTINUOUS COVERAGE - OK", jobHTML
	return f
}

func deliverOneMessage(t *testing.T, f *deliveryFixture) sentMessage {
	t.Helper()
	deliverOne(t, f)
	return f.mailgun.sent[0]
}

// A reply whose jobs sent no HTML is exactly what it was before: no html
// field, so Mailgun sends plain text alone.
func TestDeliverSendsNoHTMLWhenNoJobHasIt(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")

	sent := deliverOneMessage(t, f)

	if sent.hasHTML {
		t.Errorf("no job had HTML, so no html field may be sent; got %q", sent.html)
	}
	if sent.text != "schedule.pdf (workerlist_sheets): ok" {
		t.Errorf("text = %q, want it unchanged", sent.text)
	}
}

// With HTML, the reply is a whole document holding the transformer's fragment
// as given, under a caption naming the file and report, and closed by the same
// support line the text carries. The text part still has the plain report.
func TestDeliverSendsTheJobsHTMLBesideTheText(t *testing.T) {
	f := htmlFixture(t, "excel@mill.test", testFragment)

	sent := deliverOneMessage(t, f)

	if !sent.hasHTML {
		t.Fatal("a job with HTML must produce an html field")
	}
	for _, want := range []string{
		"<!doctype html>",
		`<meta charset="utf-8">`,
		testFragment,
		"schedule.pdf (Schedule check)",
		"Questions, problems, or suggestions? Your feedback is welcome at",
		`<a href="mailto:support@example.com">support@example.com</a>`,
	} {
		if !strings.Contains(sent.html, want) {
			t.Errorf("html lacks %q:\n%s", want, sent.html)
		}
	}
	if caption, fragment := strings.Index(sent.html, "schedule.pdf (Schedule check)"), strings.Index(sent.html, testFragment); caption > fragment {
		t.Errorf("the caption must come before the fragment:\n%s", sent.html)
	}
	if !strings.Contains(sent.text, "schedule.pdf (Schedule check): All checks passed\nCONTINUOUS COVERAGE - OK") {
		t.Errorf("text must still carry the plain report; got %q", sent.text)
	}
	if strings.Contains(sent.text, "<table") {
		t.Errorf("text must not carry HTML; got %q", sent.text)
	}
}

// A failed job's reply stays plain text, even if its HTML somehow reached the
// store.
func TestDeliverSendsNoHTMLForAFailedJob(t *testing.T) {
	f := htmlFixture(t, "excel@mill.test", testFragment)
	f.engine.pending[0].Jobs[0].Job.Status = store.StatusFailed

	sent := deliverOneMessage(t, f)

	if sent.hasHTML {
		t.Errorf("a failed job must not produce an html field; got %q", sent.html)
	}
}

// Everything FileMill itself puts in the HTML is escaped; only the
// transformer's fragment goes in as given.
func TestDeliverEscapesWhatFileMillInsertsInHTML(t *testing.T) {
	f := htmlFixture(t, "excel@mill.test", testFragment)
	f.engine.pending[0].Jobs[0].Job.InputName = "a<b>&c.pdf"
	f.engine.labels["check_schedule"] = `Check "<i>"`
	f.service.support = `help<x>@example.com`

	sent := deliverOneMessage(t, f)

	for _, want := range []string{
		"a&lt;b&gt;&amp;c.pdf",
		"Check &#34;&lt;i&gt;&#34;",
		"help&lt;x&gt;@example.com",
		testFragment,
	} {
		if !strings.Contains(sent.html, want) {
			t.Errorf("html lacks %q:\n%s", want, sent.html)
		}
	}
	for _, raw := range []string{"a<b>&c.pdf", "<i>", "help<x>"} {
		if strings.Contains(sent.html, raw) {
			t.Errorf("html carries unescaped %q:\n%s", raw, sent.html)
		}
	}
}

// In a link reply the links are real anchors in the HTML and bare URLs in the
// text, under the same sentence in both.
func TestDeliverPutsLinksInBothPartsOfAnHTMLReply(t *testing.T) {
	f := htmlFixture(t, "iwk@mill.test", testFragment, "schedule.xlsx")
	const link = "https://docs.google.com/spreadsheets/d/drive-file-1/edit"

	sent := deliverOneMessage(t, f)

	if !strings.Contains(sent.html, `<a href="`+link+`">`+link+`</a>`) {
		t.Errorf("html must link the sheet:\n%s", sent.html)
	}
	if !strings.Contains(sent.text, "\n"+link) {
		t.Errorf("text must carry the bare link; got %q", sent.text)
	}
	const sentence = "Your Schedule check is ready. Anyone with the link can view and edit it:"
	if !strings.Contains(sent.html, sentence) || !strings.Contains(sent.text, sentence) {
		t.Errorf("both parts must carry the link sentence\nhtml: %s\ntext: %q", sent.html, sent.text)
	}
	if link, support := strings.Index(sent.html, link), strings.Index(sent.html, "Questions, problems"); support < link {
		t.Errorf("the support line must follow the links:\n%s", sent.html)
	}
}

// A message's line breaks survive into the HTML of a job without a fragment.
func TestPlainHTMLParagraphKeepsLineBreaks(t *testing.T) {
	got := plainParagraph("one <two>\nthree")
	if !strings.Contains(got, "one &lt;two&gt;<br>three") {
		t.Errorf("plainParagraph = %q, want escaped text with <br>", got)
	}
}

// Alerts stay plain text.
func TestSendAlertSendsNoHTML(t *testing.T) {
	var fields map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		fields = r.MultipartForm.Value
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s := sendTestService(server.URL, &http.Client{Timeout: time.Second})

	if err := s.SendAlert(context.Background(), "ops@example.com", "[FileMill] test", "the detail"); err != nil {
		t.Fatalf("SendAlert: %v", err)
	}

	if _, ok := fields["html"]; ok {
		t.Errorf("an alert must not carry an html field; got %q", fields["html"])
	}
}
