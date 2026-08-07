// Package mailgun is FileMill's email adapter and its only internet-facing
// component. It authenticates inbound Mailgun webhooks, turns their attachments
// into jobs on the existing job engine, and mails the results back once every
// job in a submission has finished. Nothing beneath this package knows that
// FileMill is being driven by email.
//
// The files divide the responsibilities:
//
//	service.go  - the Service type and the Engine dependency it drives
//	config.go   - loading email.yaml and the Mailgun secrets
//	verify.go   - the trust boundary: signature, freshness, routing, limits
//	webhook.go  - the HTTP handler and the outcome -> status-code mapping
//	intake.go   - saving attachments and the atomic submission into the engine
//	outbound.go - the delivery loop and the Mailgun Send API client
package mailgun

import (
	"log"
	"net/http"
	"time"

	"filemill/internal/app"
	"filemill/internal/store"
)

// Engine is the slice of the job application that the email adapter drives.
// *app.App satisfies it; tests supply a fake. Depending on an interface rather
// than the concrete app keeps this — the untrusted, network-facing layer —
// exercisable in isolation.
type Engine interface {
	Submit(operation, source string) (jobID string, err error)
	Accepts(operation, filename string) bool
	BeginEmail(messageID, sender, recipient, subject string) (id int64, created bool, err error)
	SetEmailExpected(id int64, count int) error
	EmailHasJob(id int64, index int) (bool, error)
	AddEmailJob(id int64, index int, jobID string) error
	PendingEmails() ([]store.EmailSubmission, error)
	MarkEmailDelivered(id int64) error
	Outputs(id string) ([]app.OutputFile, error)

	// Published Drive files, for the sheets-link delivery mode: recorded before
	// the reply is sent so a retry reuses the link instead of uploading again,
	// and swept once they pass the retention horizon.
	PutDelivery(submissionID int64, outputIndex int, fileID, link string) error
	Delivery(submissionID int64, outputIndex int) (store.Delivery, bool, error)
	ExpiredDeliveries(cutoff time.Time) ([]store.Delivery, error)
	MarkDeliveryDeleted(submissionID int64, outputIndex int) error
}

// Service holds the adapter's configuration and its dependency on the job
// engine. Construct it with Load.
type Service struct {
	engine Engine

	signKey string // Mailgun HTTP webhook signing key — verifies inbound POSTs
	apiKey  string // Mailgun private API key — authenticates outbound sends
	domain  string // Mailgun sending domain, e.g. "mill.keywind.cc"
	from    string // address replies are sent from

	routes   map[string]string // recipient address -> transformer operation
	allowed  map[string]bool   // envelope senders permitted to submit (empty = all)
	maxBytes int64             // per-attachment size limit

	sendBase string       // Mailgun Send API base URL; overridable in tests
	client   *http.Client // outbound HTTP client (carries the send timeout)
	log      *log.Logger
}

// Handler returns the webhook HTTP handler. It is mounted by cmd/filemill run
// only in continuous worker mode.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.handle) }
