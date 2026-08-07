package mailgun

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"

	"filemill/internal/app"
	"filemill/internal/store"
)

// discardLogger is a no-op logger for tests that don't assert on log output.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

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
	subs      map[string]int64         // idempotency key -> submission id
	jobs      map[int64]map[int]string // sid -> attachment index -> job id
	expected  map[int64]int
	delivered map[int64]bool

	submitCount     int
	sources         []string // source paths passed to Submit, in order
	failSubmitAfter int      // fail Submit once this many have succeeded; -1 = never
	submitErr       error    // when set, Submit fails immediately with this error

	// accepted lists the extensions the fake transformer accepts (".pdf").
	// nil accepts everything, which is what most tests want.
	accepted []string

	calls []string // ordered log of mutating calls

	pending []store.EmailSubmission
	outputs map[string][]app.OutputFile
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		subs:            map[string]int64{},
		jobs:            map[int64]map[int]string{},
		expected:        map[int64]int{},
		delivered:       map[int64]bool{},
		failSubmitAfter: -1,
		outputs:         map[string][]app.OutputFile{},
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
	if f.submitErr != nil {
		return "", f.submitErr
	}
	if f.failSubmitAfter >= 0 && f.submitCount >= f.failSubmitAfter {
		return "", fmt.Errorf("submit failed")
	}
	f.submitCount++
	return fmt.Sprintf("job-%d", f.submitCount), nil
}

func (f *fakeEngine) BeginEmail(messageID, sender, recipient, subject string) (int64, bool, error) {
	f.calls = append(f.calls, "BeginEmail")
	if sid, ok := f.subs[messageID]; ok {
		return sid, false, nil
	}
	f.nextSID++
	f.subs[messageID] = f.nextSID
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

func (f *fakeEngine) Outputs(id string) ([]app.OutputFile, error) { return f.outputs[id], nil }
