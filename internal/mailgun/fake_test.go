package mailgun

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filemill/internal/app"
	"filemill/internal/store"
)

// discardLogger is a no-op logger for tests that don't assert on log output.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// newTestLogger writes to the given sink, for tests that assert on log output
// but keep their own buffer.
func newTestLogger(w io.Writer) *log.Logger { return log.New(w, "", 0) }

// captureLogger returns a logger and the buffer it writes to, so a test can
// assert on what the webhook recorded (e.g. that a reason names the sender).
func captureLogger() (*log.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return log.New(buf, "", 0), buf
}

// fakeEngine is an in-memory Engine for isolation tests: no filesystem, no
// SQLite, no transformer subprocess. It records the order of mutating calls so
// tests can assert on the intake commit sequence.
type fakeEngine struct {
	nextSID   int64
	subs      map[submissionKey]int64  // idempotency key + recipient -> submission id
	jobs      map[int64]map[int]string // sid -> attachment index -> job id
	expected  map[int64]int
	delivered map[int64]bool

	submitCount     int
	sources         []string // source paths passed to Submit, in order
	operations      []string // operations passed to Submit, in order
	failSubmitAfter int      // fail Submit once this many have succeeded; -1 = never
	submitErr       error    // when set, Submit fails immediately with this error

	// accepted lists the extensions the fake transformer accepts (".pdf").
	// nil accepts everything, which is what most tests want.
	accepted []string

	calls []string // ordered log of mutating calls

	pending    []store.EmailSubmission
	outputs    map[string][]app.OutputFile
	outputsErr map[string]error // job id -> error Outputs should return

	deliveries      map[deliveryKey]store.Delivery
	sweptDeliveries map[deliveryKey]bool
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		subs:            map[submissionKey]int64{},
		jobs:            map[int64]map[int]string{},
		expected:        map[int64]int{},
		delivered:       map[int64]bool{},
		failSubmitAfter: -1,
		outputs:         map[string][]app.OutputFile{},
		outputsErr:      map[string]error{},
		deliveries:      map[deliveryKey]store.Delivery{},
		sweptDeliveries: map[deliveryKey]bool{},
	}
}

func (f *fakeEngine) Accepts(operation, filename string) bool {
	if f.accepted == nil {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	for _, allowed := range f.accepted {
		if ext == strings.ToLower(allowed) {
			return true
		}
	}
	return false
}

func (f *fakeEngine) Submit(operation, source string) (string, error) {
	f.calls = append(f.calls, "Submit")
	f.sources = append(f.sources, source)
	f.operations = append(f.operations, operation)
	if f.submitErr != nil {
		return "", f.submitErr
	}
	if f.failSubmitAfter >= 0 && f.submitCount >= f.failSubmitAfter {
		return "", fmt.Errorf("submit failed")
	}
	f.submitCount++
	return fmt.Sprintf("job-%d", f.submitCount), nil
}

// submissionKey mirrors the store's uniqueness: a submission is one message to
// one recipient. Keyed on the message alone, as this fake once was, a fan-out
// to two addresses collapses into a single submission and the second recipient
// silently gets no job — the bug this pair exists to prevent.
type submissionKey struct{ messageID, recipient string }

func (f *fakeEngine) BeginEmail(messageID, sender, recipient, subject string) (int64, bool, error) {
	f.calls = append(f.calls, "BeginEmail")
	key := submissionKey{messageID, recipient}
	if sid, ok := f.subs[key]; ok {
		return sid, false, nil
	}
	f.nextSID++
	f.subs[key] = f.nextSID
	f.jobs[f.nextSID] = map[int]string{}
	return f.nextSID, true, nil
}

func (f *fakeEngine) SetEmailExpected(id int64, count int) error {
	f.calls = append(f.calls, "SetEmailExpected")
	f.expected[id] = count
	return nil
}

func (f *fakeEngine) EmailHasJob(id int64, index int) (bool, error) {
	_, ok := f.jobs[id][index]
	return ok, nil
}

func (f *fakeEngine) AddEmailJob(id int64, index int, jobID string) error {
	f.calls = append(f.calls, fmt.Sprintf("AddEmailJob(%d)", index))
	f.jobs[id][index] = jobID
	return nil
}

func (f *fakeEngine) PendingEmails() ([]store.EmailSubmission, error) { return f.pending, nil }

func (f *fakeEngine) MarkEmailDelivered(id int64) error {
	f.delivered[id] = true
	return nil
}

func (f *fakeEngine) Outputs(id string) ([]app.OutputFile, error) {
	if err := f.outputsErr[id]; err != nil {
		return nil, err
	}
	return f.outputs[id], nil
}

// deliveryKey identifies one published output in the fake's record table.
type deliveryKey struct {
	submissionID int64
	outputIndex  int
}

func (f *fakeEngine) PutDelivery(submissionID int64, outputIndex int, fileID, link string) error {
	f.calls = append(f.calls, fmt.Sprintf("PutDelivery(%d,%d)", submissionID, outputIndex))
	key := deliveryKey{submissionID, outputIndex}
	if _, exists := f.deliveries[key]; exists {
		return nil // INSERT OR IGNORE: never overwrite a link already mailed out
	}
	f.deliveries[key] = store.Delivery{
		SubmissionID: submissionID, OutputIndex: outputIndex,
		FileID: fileID, Link: link, CreatedAt: time.Now().UTC(),
	}
	return nil
}

func (f *fakeEngine) Delivery(submissionID int64, outputIndex int) (store.Delivery, bool, error) {
	d, ok := f.deliveries[deliveryKey{submissionID, outputIndex}]
	return d, ok, nil
}

func (f *fakeEngine) ExpiredDeliveries(cutoff time.Time) ([]store.Delivery, error) {
	var out []store.Delivery
	for key, d := range f.deliveries {
		if !f.sweptDeliveries[key] && d.CreatedAt.Before(cutoff) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmissionID < out[j].SubmissionID })
	return out, nil
}

func (f *fakeEngine) MarkDeliveryDeleted(submissionID int64, outputIndex int) error {
	f.sweptDeliveries[deliveryKey{submissionID, outputIndex}] = true
	return nil
}
