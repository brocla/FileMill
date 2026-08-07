package mailgun

import (
	"context"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filemill/internal/app"
	"filemill/internal/store"
)

// fakePublisher stands in for Google Drive. It records every Publish so a test
// can assert that a retry did not upload a second copy.
type fakePublisher struct {
	published []string // paths passed to Publish, in order
	deleted   []string // file ids passed to Delete, in order
	nextID    int
	err       error // when set, Publish fails with it
	deleteErr error // when set, Delete fails with it
}

func (p *fakePublisher) Publish(_ context.Context, path, name string) (string, string, error) {
	p.published = append(p.published, path)
	if p.err != nil {
		return "", "", p.err
	}
	p.nextID++
	id := fmt.Sprintf("drive-file-%d", p.nextID)
	return id, "https://docs.google.com/spreadsheets/d/" + id + "/edit", nil
}

func (p *fakePublisher) Delete(_ context.Context, fileID string) error {
	p.deleted = append(p.deleted, fileID)
	return p.deleteErr
}

// sentMessage is one reply captured from the fake Mailgun endpoint.
type sentMessage struct {
	to, text    string
	attachments []string
}

// fakeMailgun is a stand-in Send API that records replies. Addresses listed in
// failTo get a 500, so a test can make one submission fail while others succeed.
type fakeMailgun struct {
	sent   []sentMessage
	failTo map[string]bool
}

func (m *fakeMailgun) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("parse content type: %v", err)
			return
		}
		msg := sentMessage{}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			buf := make([]byte, 4096)
			n, _ := part.Read(buf)
			switch {
			case part.FileName() != "":
				msg.attachments = append(msg.attachments, part.FileName())
			case part.FormName() == "to":
				msg.to = string(buf[:n])
			case part.FormName() == "text":
				msg.text = string(buf[:n])
			}
		}
		if m.failTo[msg.to] {
			http.Error(w, "simulated mailgun failure", http.StatusInternalServerError)
			return
		}
		m.sent = append(m.sent, msg)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server
}

// deliveryFixture wires an engine, a publisher, and a fake Mailgun into a
// Service configured for one sheets-link route and one plain email route.
type deliveryFixture struct {
	service   *Service
	engine    *fakeEngine
	publisher *fakePublisher
	mailgun   *fakeMailgun
	logs      *strings.Builder
}

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	engine, publisher := newFakeEngine(), &fakePublisher{}
	mg := &fakeMailgun{failTo: map[string]bool{}}
	server := mg.start(t)
	logs := &strings.Builder{}

	return &deliveryFixture{
		service: &Service{
			engine:    engine,
			publisher: publisher,
			from:      "filemill@mill.test",
			domain:    "mill.test",
			apiKey:    "key",
			delivery:  map[string]string{"iwk@mill.test": modeSheetsLink},
			sendBase:  server.URL,
			client:    &http.Client{Timeout: 5 * time.Second},
			log:       newTestLogger(logs),
		},
		engine:    engine,
		publisher: publisher,
		mailgun:   mg,
		logs:      logs,
	}
}

// addSubmission registers a finished submission whose single job produced one
// output file, and returns nothing — tests read results off the fixture.
func (f *deliveryFixture) addSubmission(t *testing.T, id int64, recipient, outputName string) {
	t.Helper()
	jobID := fmt.Sprintf("job-%d", id)
	path := filepath.Join(t.TempDir(), outputName)
	if err := os.WriteFile(path, []byte("spreadsheet bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	f.engine.outputs[jobID] = []app.OutputFile{{Name: outputName, Path: path}}
	f.engine.pending = append(f.engine.pending, store.EmailSubmission{
		ID: id, Sender: fmt.Sprintf("sender%d@example.com", id), Recipient: recipient,
		Subject: "Schedule", MessageID: fmt.Sprintf("<msg-%d@example.com>", id), Expected: 1,
		Jobs: []store.EmailJob{{Index: 0, Job: store.Job{
			ID: jobID, Status: store.StatusSucceeded, InputName: "schedule.pdf", Message: "ok",
		}}},
	})
}

// A sheets-link route replies with a link, not a file: the output is uploaded
// exactly once and the reply carries no attachment.
func TestDeliverSheetsLinkRouteEmailsLinkNotAttachment(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "schedule.xlsx")

	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}

	if len(f.publisher.published) != 1 {
		t.Fatalf("published %d files, want 1", len(f.publisher.published))
	}
	if len(f.mailgun.sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(f.mailgun.sent))
	}
	sent := f.mailgun.sent[0]
	if len(sent.attachments) != 0 {
		t.Errorf("a link reply must carry no attachment; got %v", sent.attachments)
	}
	if !strings.Contains(sent.text, "https://docs.google.com/spreadsheets/d/drive-file-1/edit") {
		t.Errorf("reply must carry the link; got %q", sent.text)
	}
	if !f.engine.delivered[1] {
		t.Error("submission must be marked delivered")
	}
}

// The load-bearing case. Delivery is at-least-once, so a failed send is retried
// on the next tick — but the upload must not repeat, or the sender's data ends
// up in a second world-editable sheet nobody will clean up.
func TestDeliverRetryAfterSendFailureDoesNotRepublish(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "schedule.xlsx")
	f.mailgun.failTo["sender1@example.com"] = true

	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("first tick returned an error instead of skipping: %v", err)
	}
	if len(f.publisher.published) != 1 {
		t.Fatalf("first tick published %d files, want 1", len(f.publisher.published))
	}
	if f.engine.delivered[1] {
		t.Fatal("submission must not be marked delivered when the send failed")
	}

	// Mailgun recovers; the next tick retries the same submission.
	f.mailgun.failTo = map[string]bool{}
	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("retry tick: %v", err)
	}

	if len(f.publisher.published) != 1 {
		t.Fatalf("retry re-uploaded: published %d files, want 1", len(f.publisher.published))
	}
	if len(f.mailgun.sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(f.mailgun.sent))
	}
	if !strings.Contains(f.mailgun.sent[0].text, "drive-file-1") {
		t.Errorf("retry must reuse the stored link; got %q", f.mailgun.sent[0].text)
	}
	if !f.engine.delivered[1] {
		t.Error("submission must be marked delivered after the successful retry")
	}
}

// Routes without a delivery mode keep today's behavior exactly: the output file
// is attached and nothing is published.
func TestDeliverEmailRouteStillAttachesTheFile(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")

	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}

	if len(f.publisher.published) != 0 {
		t.Errorf("an email route must not publish; got %v", f.publisher.published)
	}
	if len(f.mailgun.sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(f.mailgun.sent))
	}
	if got := f.mailgun.sent[0].attachments; len(got) != 1 || got[0] != "schedule.xlsx" {
		t.Errorf("attachments = %v, want [schedule.xlsx]", got)
	}
}

// Head-of-line blocking: one submission that cannot be delivered must not stall
// every submission behind it. With Google in the path a per-submission failure
// can persist for hours, and it would otherwise silently halt all replies —
// including plain-attachment ones that have nothing to do with it.
func TestDeliverSkipsFailingSubmissionAndContinues(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "stuck.xlsx")
	f.addSubmission(t, 2, "excel@mill.test", "fine.xlsx")
	f.publisher.err = fmt.Errorf("google token expired")

	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("a failing submission must be skipped, not returned: %v", err)
	}

	if f.engine.delivered[1] {
		t.Error("the failing submission must not be marked delivered")
	}
	if !f.engine.delivered[2] {
		t.Error("the following submission must still be delivered")
	}
	if len(f.mailgun.sent) != 1 || f.mailgun.sent[0].to != "sender2@example.com" {
		t.Fatalf("only the good submission should have been sent; got %+v", f.mailgun.sent)
	}
	if !strings.Contains(f.logs.String(), "google token expired") {
		t.Errorf("the skipped failure must be logged; got %q", f.logs.String())
	}
}

// An engine failure reading a job's outputs is a per-submission problem too,
// not a reason to stop the queue.
func TestDeliverSkipsSubmissionWhoseOutputsCannotBeRead(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "broken.xlsx")
	f.addSubmission(t, 2, "excel@mill.test", "fine.xlsx")
	f.engine.outputsErr["job-1"] = fmt.Errorf("result.json missing")

	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("an unreadable submission must be skipped, not returned: %v", err)
	}
	if f.engine.delivered[1] {
		t.Error("the unreadable submission must not be marked delivered")
	}
	if !f.engine.delivered[2] {
		t.Error("the following submission must still be delivered")
	}
}
