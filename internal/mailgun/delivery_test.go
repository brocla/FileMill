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
	failAfter int   // fail Publish once this many have succeeded; 0 = never
	deleteErr error // when set, Delete fails with it
}

func (p *fakePublisher) Publish(_ context.Context, path, name string) (string, string, error) {
	p.published = append(p.published, path)
	if p.err != nil {
		return "", "", p.err
	}
	if p.failAfter > 0 && p.nextID >= p.failAfter {
		return "", "", fmt.Errorf("drive quota exceeded")
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

// addSubmission registers a finished submission whose single job produced the
// named output files — usually one, but a job may declare several.
func (f *deliveryFixture) addSubmission(t *testing.T, id int64, recipient string, outputNames ...string) {
	t.Helper()
	f.addSubmissionFor(t, id, recipient, "workerlist_sheets", outputNames...)
}

// addSubmissionFor is addSubmission with the operation named, for the tests
// that turn on which report the reply is carrying.
func (f *deliveryFixture) addSubmissionFor(t *testing.T, id int64, recipient, operation string, outputNames ...string) {
	t.Helper()
	jobID := fmt.Sprintf("job-%d", id)
	dir := t.TempDir()
	for _, name := range outputNames {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("spreadsheet bytes"), 0644); err != nil {
			t.Fatal(err)
		}
		f.engine.outputs[jobID] = append(f.engine.outputs[jobID], app.OutputFile{Name: name, Path: path})
	}
	f.engine.pending = append(f.engine.pending, store.EmailSubmission{
		ID: id, Sender: fmt.Sprintf("sender%d@example.com", id), Recipient: recipient,
		Subject: "Schedule", MessageID: fmt.Sprintf("<msg-%d@example.com>", id), Expected: 1,
		Jobs: []store.EmailJob{{Index: 0, Job: store.Job{
			ID: jobID, Operation: operation, Status: store.StatusSucceeded,
			InputName: "schedule.pdf", Message: "ok",
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

// Idempotency is per output file, not per submission — which is why the record
// is keyed on both. A job declaring two outputs that fails partway must resume
// where it stopped: the file already in Drive is reused, only the missing one
// is uploaded.
func TestDeliverResumesPartiallyPublishedSubmission(t *testing.T) {
	f := newDeliveryFixture(t)
	f.addSubmission(t, 1, "iwk@mill.test", "morning.xlsx", "evening.xlsx")
	f.publisher.failAfter = 1 // the first output uploads, the second does not

	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("first tick returned an error instead of skipping: %v", err)
	}
	if len(f.mailgun.sent) != 0 {
		t.Fatalf("nothing may be sent until every output is published; got %+v", f.mailgun.sent)
	}

	// Drive recovers; the next tick retries the same submission.
	f.publisher.failAfter = 0
	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("retry tick: %v", err)
	}

	// Three Publish attempts total: morning (ok), evening (failed), evening
	// (ok). morning must not appear twice.
	morning := 0
	for _, path := range f.publisher.published {
		if strings.HasSuffix(path, "morning.xlsx") {
			morning++
		}
	}
	if morning != 1 {
		t.Errorf("the already-published output was uploaded %d times, want 1", morning)
	}
	if len(f.mailgun.sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(f.mailgun.sent))
	}
	text := f.mailgun.sent[0].text
	if !strings.Contains(text, "drive-file-1") || !strings.Contains(text, "drive-file-2") {
		t.Errorf("the reply must carry both links; got %q", text)
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

// --- naming the report in the reply --------------------------------------
//
// One source PDF feeds more than one report, and the outputs differ only by a
// filename prefix the recipient never sees under link delivery. Without the
// report's name in the reply, an iwk answer and a vwk answer read identically.

func labelledFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := newDeliveryFixture(t)
	f.engine.labels = map[string]string{
		"workerlist_sheets": "IWK booth worker list",
		"workerlist_excel":  "IWK booth worker list",
		"vworker":           "VWK worker schedule",
	}
	return f
}

func deliverOne(t *testing.T, f *deliveryFixture) string {
	t.Helper()
	if err := f.service.deliverPending(context.Background()); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}
	if len(f.mailgun.sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(f.mailgun.sent))
	}
	return f.mailgun.sent[0].text
}

func TestDeliverNamesTheWorkerlistReportInALinkReply(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "iwk@mill.test", "workerlist_sheets", "iwk_schedule.xlsx")

	text := deliverOne(t, f)

	if !strings.Contains(text, "Your IWK booth worker list is ready") {
		t.Errorf("reply must name the report; got %q", text)
	}
	if strings.Contains(text, "VWK worker schedule") {
		t.Errorf("reply must not name the other report; got %q", text)
	}
}

func TestDeliverNamesTheVworkerReportInALinkReply(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "iwk@mill.test", "vworker", "vwk_schedule.xlsx")

	text := deliverOne(t, f)

	if !strings.Contains(text, "Your VWK worker schedule is ready") {
		t.Errorf("reply must name the report; got %q", text)
	}
	if strings.Contains(text, "IWK booth worker list") {
		t.Errorf("reply must not name the other report; got %q", text)
	}
}

// The two workerlist operations are one report in two layouts, so they answer
// to the same name. A recipient cares which report they got, not which layout
// rendered it.
func TestDeliverGivesBothWorkerlistLayoutsTheSameName(t *testing.T) {
	for _, operation := range []string{"workerlist_sheets", "workerlist_excel"} {
		t.Run(operation, func(t *testing.T) {
			f := labelledFixture(t)
			f.addSubmissionFor(t, 1, "iwk@mill.test", operation, "iwk_schedule.xlsx")

			if text := deliverOne(t, f); !strings.Contains(text, "IWK booth worker list") {
				t.Errorf("reply must name the report; got %q", text)
			}
		})
	}
}

// Every job line names its report, not just the link sentence: one submission
// can carry several jobs, and only a per-line label tells them apart.
func TestDeliverNamesTheReportOnEachJobLine(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "iwk@mill.test", "vworker", "vwk_schedule.xlsx")

	if text := deliverOne(t, f); !strings.Contains(text, "schedule.pdf (VWK worker schedule): ok") {
		t.Errorf("job line must name the report; got %q", text)
	}
}

// An attachment reply gets the same treatment: it is the job lines that carry
// the name there, since there is no link sentence to hang it on.
func TestDeliverNamesTheReportInAnAttachmentReply(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "excel@mill.test", "workerlist_excel", "iwk_schedule.xlsx")

	text := deliverOne(t, f)

	if len(f.mailgun.sent[0].attachments) != 1 {
		t.Fatalf("an email route must attach the file; got %v", f.mailgun.sent[0].attachments)
	}
	if !strings.Contains(text, "(IWK booth worker list)") {
		t.Errorf("job line must name the report; got %q", text)
	}
}

// A transformer that configures no label still says something: the operation
// name is a worse name than a real one, but far better than a blank.
func TestDeliverFallsBackToTheOperationNameWhenUnlabelled(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "iwk@mill.test", "copy_rename", "iwk_schedule.xlsx")

	if text := deliverOne(t, f); !strings.Contains(text, "Your copy_rename is ready") {
		t.Errorf("reply must fall back to the operation name; got %q", text)
	}
}

// Several files, one report: the sentence pluralizes by what the label names
// rather than by bolting an "s" onto a report's name.
func TestDeliverPluralizesWhenOneReportProducedSeveralFiles(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "iwk@mill.test", "vworker", "vwk_one.xlsx", "vwk_two.xlsx")

	text := deliverOne(t, f)

	if !strings.Contains(text, "Your VWK worker schedule files are ready") {
		t.Errorf("reply must pluralize around the label; got %q", text)
	}
	if !strings.Contains(text, "view and edit them:") {
		t.Errorf("reply must agree in number; got %q", text)
	}
}

// A submission whose jobs produced different reports has no single name to
// give, so it says nothing rather than naming one of them and misleading.
func TestDeliverUsesGenericWordingForAMixedSubmission(t *testing.T) {
	f := labelledFixture(t)
	f.addSubmissionFor(t, 1, "iwk@mill.test", "vworker", "vwk_schedule.xlsx")

	// A second job on the same submission, from the other transformer.
	dir := t.TempDir()
	path := filepath.Join(dir, "iwk_schedule.xlsx")
	if err := os.WriteFile(path, []byte("spreadsheet bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	f.engine.outputs["job-1b"] = []app.OutputFile{{Name: "iwk_schedule.xlsx", Path: path}}
	f.engine.pending[0].Jobs = append(f.engine.pending[0].Jobs, store.EmailJob{
		Index: 1, Job: store.Job{
			ID: "job-1b", Operation: "workerlist_sheets", Status: store.StatusSucceeded,
			InputName: "roster.pdf", Message: "ok",
		},
	})

	text := deliverOne(t, f)

	if !strings.Contains(text, "Your spreadsheets are ready") {
		t.Errorf("a mixed submission must not name one report; got %q", text)
	}
	// Both are still named on their own job lines.
	for _, want := range []string{"(VWK worker schedule)", "(IWK booth worker list)"} {
		if !strings.Contains(text, want) {
			t.Errorf("job lines must name each report; %q missing from %q", want, text)
		}
	}
}
