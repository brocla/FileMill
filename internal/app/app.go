package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"filemill/internal/alert"
	"filemill/internal/config"
	"filemill/internal/contract"
	"filemill/internal/store"
	"github.com/google/uuid"
)

// jobTimeout bounds one transformer run. A var, not a const, so the timeout
// test can shrink it rather than wait 10 minutes.
var jobTimeout = 10 * time.Minute

// claimAlertAfter is how long job claims must keep failing before Run reports
// it: a blip clears itself, a stuck database halts all work. A var so Run's
// test need not wait a minute.
var claimAlertAfter = time.Minute

// outputTailSize is how much of a transformer's output an alert carries.
const outputTailSize = 2048

// panicMessage is a job's message after a panic. The sender reads it in the
// reply, so it says no more than that the fault was ours.
const panicMessage = "internal error while running the job"

// ErrRejected marks a Submit failure caused by unacceptable input — a wrong
// file type, a directory, or an unknown operation. The input itself is the
// problem, so retrying with the same input can never succeed. Callers that
// front a retrying transport (the Mailgun webhook) test for this with
// errors.Is to distinguish a permanent rejection from a transient failure.
var ErrRejected = errors.New("input rejected")

type App struct {
	root, data string
	cfg        config.Config
	store      *store.Store
	log        *log.Logger
	logFile    *os.File
	reporter   alert.Reporter
}
type OutputFile struct{ Name, Path string }

func Open(root string) (*App, error) {
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "jobs"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(data, "logs"), 0755); err != nil {
		return nil, err
	}
	cfg, err := config.Load(filepath.Join(root, "config", "transformers.yaml"))
	if err != nil {
		return nil, err
	}
	s, err := store.Open(filepath.Join(data, "filemill.db"))
	if err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(filepath.Join(data, "logs", "filemill.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &App{root: root, data: data, cfg: cfg, store: s, log: log.New(lf, "", log.LstdFlags|log.LUTC), logFile: lf, reporter: alert.Nop{}}, nil
}
func (a *App) Close() error { a.logFile.Close(); return a.store.Close() }

// SetReporter routes the worker's systemic failures to r; nil restores the
// no-op default. It is a setter, not an Open argument, because the real
// reporter needs the Mailgun service, which is built after the App. Call it
// before Run.
func (a *App) SetReporter(r alert.Reporter) {
	if r == nil {
		r = alert.Nop{}
	}
	a.reporter = r
}

// LogWriter exposes the application log sink so adapters (e.g. the Mailgun
// webhook) can write to the same filemill.log the worker uses.
func (a *App) LogWriter() io.Writer { return a.logFile }
func (a *App) Submit(operation, source string) (string, error) {
	t, ok := a.cfg.Find(operation)
	if !ok {
		return "", fmt.Errorf("%w: unknown operation %q", ErrRejected, operation)
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: input must be a file", ErrRejected)
	}
	if !t.Accepts(info.Name()) {
		return "", fmt.Errorf("%w: %s does not accept %q", ErrRejected, operation, filepath.Ext(info.Name()))
	}
	id := uuid.NewString()
	workspace := filepath.Join(a.data, "jobs", id)
	input := filepath.Join(workspace, "input")
	if err := os.MkdirAll(filepath.Join(workspace, "output"), 0755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(input, 0755); err != nil {
		return "", err
	}
	name := filepath.Base(source)
	if err := copyFile(source, filepath.Join(input, name)); err != nil {
		return "", err
	}
	// A transformer's configured options flow into every job it runs. Default a
	// nil map to an empty object so optionless transformers keep seeing "{}".
	options := t.Options
	if options == nil {
		options = map[string]any{}
	}
	j := contract.Job{ContractVersion: contract.Version, JobID: id, Operation: operation, InputFiles: []contract.InputFile{{Path: filepath.ToSlash(filepath.Join("input", name)), Name: name}}, OutputDirectory: "output", Options: options}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(workspace, "job.json"), b, 0644); err != nil {
		return "", err
	}
	if err := a.store.Create(store.Job{ID: id, Operation: operation, InputName: name, CreatedAt: time.Now()}); err != nil {
		return "", err
	}
	a.log.Printf("job=%s status=queued operation=%s", id, operation)
	return id, nil
}

// Accepts reports whether the named operation's transformer handles a file with
// this name. The email adapter asks before submitting, so it can tell an
// attachment that is real work from one that is incidental (a signature image).
func (a *App) Accepts(operation, filename string) bool {
	t, ok := a.cfg.Find(operation)
	return ok && t.Accepts(filename)
}

// OperationOptions returns the options configured for an operation, or nil if
// there is no such operation. The email adapter cross-checks a route's delivery
// mode against them at startup.
func (a *App) OperationOptions(operation string) map[string]any {
	t, ok := a.cfg.Find(operation)
	if !ok {
		return nil
	}
	return t.Options
}

// OperationLabel names the report an operation produces, for the reply that
// carries it back. It falls back to the operation name, so a transformer that
// configures no label still says something rather than nothing -- and an
// operation that no longer exists still names what was asked for.
func (a *App) OperationLabel(operation string) string {
	t, ok := a.cfg.Find(operation)
	if !ok || t.Label == "" {
		return operation
	}
	return t.Label
}
func (a *App) Job(id string) (store.Job, error) { return a.store.Get(id) }
func (a *App) Outputs(id string) ([]OutputFile, error) {
	r, err := readResult(filepath.Join(a.data, "jobs", id, "result.json"), filepath.Join(a.data, "jobs", id))
	if err != nil {
		return nil, err
	}
	out := make([]OutputFile, 0, len(r.OutputFiles))
	for _, f := range r.OutputFiles {
		out = append(out, OutputFile{Name: f.Name, Path: filepath.Join(a.data, "jobs", id, filepath.FromSlash(f.Path))})
	}
	return out, nil
}
func (a *App) BeginEmail(messageID, sender, recipient, subject string) (int64, bool, error) {
	return a.store.BeginEmail(messageID, sender, recipient, subject)
}
func (a *App) SetEmailExpected(id int64, count int) error    { return a.store.SetEmailExpected(id, count) }
func (a *App) EmailHasJob(id int64, index int) (bool, error) { return a.store.EmailHasJob(id, index) }
func (a *App) AddEmailJob(id int64, index int, jobID string) error {
	return a.store.AddEmailJob(id, index, jobID)
}
func (a *App) PendingEmails() ([]store.EmailSubmission, error) { return a.store.PendingEmails() }
func (a *App) MarkEmailDelivered(id int64) error               { return a.store.MarkEmailDelivered(id) }
func (a *App) PutDelivery(submissionID int64, outputIndex int, fileID, link string) error {
	return a.store.PutDelivery(submissionID, outputIndex, fileID, link)
}
func (a *App) Delivery(submissionID int64, outputIndex int) (store.Delivery, bool, error) {
	return a.store.Delivery(submissionID, outputIndex)
}
func (a *App) ExpiredDeliveries(cutoff time.Time) ([]store.Delivery, error) {
	return a.store.ExpiredDeliveries(cutoff)
}
func (a *App) MarkDeliveryDeleted(submissionID int64, outputIndex int) error {
	return a.store.MarkDeliveryDeleted(submissionID, outputIndex)
}

// The alert throttle's state lives in the job database, so a restarted worker
// inherits it. main hands the App to alert.NewEmailer as its Ledger.
func (a *App) LastAlert(category string) (time.Time, int, error) { return a.store.LastAlert(category) }
func (a *App) RecordAlertSent(category string, at time.Time) error {
	return a.store.RecordAlertSent(category, at)
}
func (a *App) RecordAlertSuppressed(category string) error {
	return a.store.RecordAlertSuppressed(category)
}
func (a *App) AlertSendsSince(t time.Time) ([]time.Time, error) { return a.store.AlertSendsSince(t) }
func (a *App) Run(ctx context.Context, once bool) error {
	a.log.Printf("worker started once=%t", once)
	var claims claimFailures
	for {
		j, err := a.store.Next()
		if err != nil {
			if once {
				return err
			}
			// A transient store error (e.g. SQLITE_BUSY under load) must not
			// take down the worker: log it, back off, and retry. One that
			// persists halts all work, so it is reported.
			a.log.Printf("worker claim error: %v", err)
			if claims.failed(time.Now()) {
				a.reporter.Report(alert.Alert{
					Category: "worker-claim",
					Summary:  "worker cannot claim jobs",
					Detail: fmt.Sprintf("Claiming the next job has failed continuously since %s, so no job can run.\n\nLatest error: %v\n",
						claims.since.Format(time.RFC3339), err),
				})
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		claims.succeeded()
		if j != nil {
			a.execute(ctx, *j)
			if once {
				return nil
			}
			continue
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			a.log.Printf("worker stopped")
			return nil
		case <-time.After(time.Second):
		}
	}
}

// claimFailures tracks an unbroken run of failed job claims, so Run reports
// one that has lasted claimAlertAfter, and only once per run.
type claimFailures struct {
	since    time.Time // the run's first failure; zero while claims work
	reported bool
}

// failed records a failed claim at now and reports whether to alert.
func (c *claimFailures) failed(now time.Time) bool {
	if c.since.IsZero() {
		c.since = now
	}
	if c.reported || now.Sub(c.since) < claimAlertAfter {
		return false
	}
	c.reported = true
	return true
}

func (c *claimFailures) succeeded() { *c = claimFailures{} }

// execute runs one job and records how it ended. A failure the transformer
// reported through the contract (a valid result.json with success:false) is
// the sender's problem and the reply tells them. Any other failure is
// FileMill's problem and is reported as well.
func (a *App) execute(parent context.Context, j store.Job) {
	// A panic fails this job, not the worker: execute returns normally and Run
	// moves on to the next job.
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("job=%s panic=%v", j.ID, r)
			a.finish(j.ID, store.StatusFailed, panicMessage)
			a.reportJob("panic", j, fmt.Sprintf("%s job panicked: %v", j.Operation, r), panicMessage,
				fmt.Sprintf("Panic: %v\n\n%s", r, debug.Stack()))
		}
	}()
	t, ok := a.cfg.Find(j.Operation)
	if !ok {
		a.failSystemic(j, "registered transformer no longer exists", nil, nil)
		return
	}
	workspace := filepath.Join(a.data, "jobs", j.ID)
	ctx, cancel := context.WithTimeout(parent, jobTimeout)
	defer cancel()
	args := append(append([]string{}, t.Command[1:]...), "job.json")
	cmd := exec.CommandContext(ctx, t.Command[0], args...)
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		a.log.Printf("job=%s transformer_output=%s", j.ID, strings.TrimSpace(string(output)))
	}
	if ctx.Err() == context.DeadlineExceeded {
		a.failSystemic(j, "transformer timed out after 10 minutes", nil, output)
		return
	}
	result, readErr := readResult(filepath.Join(workspace, "result.json"), workspace)
	if err != nil {
		msg := "transformer exited unsuccessfully"
		if readErr == nil && result.Message != "" {
			msg = result.Message
		}
		switch {
		case readErr == nil && !result.Success:
			// Turned down through the contract, whatever the exit code.
			a.finish(j.ID, store.StatusFailed, msg)
		case parent.Err() != nil:
			// The worker is shutting down and killed the transformer. That is
			// FileMill stopping, not the transformer failing.
			a.finish(j.ID, store.StatusFailed, msg)
		case readErr != nil:
			a.failSystemic(j, msg, fmt.Errorf("%w; %w", err, readErr), output)
		default:
			// The result claims success but the exit code contradicts it. The
			// transformer's own message would tell the sender their report is
			// ready in a reply with nothing attached, so say what happened
			// instead, and keep its message for the alert.
			a.failSystemic(j, "transformer reported success but then exited with an error",
				fmt.Errorf("the transformer ended with %w; its result.json said %q", err, result.Message), output)
		}
		return
	}
	if readErr != nil {
		a.failSystemic(j, readErr.Error(), nil, output)
		return
	}
	if !result.Success {
		a.finish(j.ID, store.StatusFailed, result.Message)
		return
	}
	a.finish(j.ID, store.StatusSucceeded, result.Message)
}
func (a *App) finish(id, status, message string) {
	if err := a.store.Complete(id, status, message); err != nil {
		a.log.Printf("job=%s completion_error=%v", id, err)
		return
	}
	a.log.Printf("job=%s status=%s message=%q", id, status, message)
}

// failSystemic fails a job the transformer didn't handle through the contract
// and reports it. cause adds what the job's message leaves out, such as the
// exit status.
func (a *App) failSystemic(j store.Job, message string, cause error, output []byte) {
	a.finish(j.ID, store.StatusFailed, message)
	var extra strings.Builder
	if cause != nil {
		fmt.Fprintf(&extra, "Cause: %v\n", cause)
	}
	if tail := outputTail(output); tail != "" {
		fmt.Fprintf(&extra, "\nTransformer output (last 2 KB):\n%s\n", tail)
	}
	a.reportJob("job-systemic", j, fmt.Sprintf("%s job failed: %s", j.Operation, message), message, extra.String())
}

// reportJob reports a failed job, identified the same way in every alert.
func (a *App) reportJob(category string, j store.Job, summary, message, extra string) {
	detail := fmt.Sprintf("Job: %s\nOperation: %s\nInput: %s\nMessage: %s\n", j.ID, j.Operation, j.InputName, message)
	if extra != "" {
		detail += "\n" + extra
	}
	a.reporter.Report(alert.Alert{Category: category, Summary: strings.Join(strings.Fields(summary), " "), Detail: detail})
}

// outputTail returns the end of a transformer's output, where a crash's
// traceback lands, capped at outputTailSize bytes.
func outputTail(output []byte) string {
	s := strings.TrimSpace(string(output))
	if len(s) <= outputTailSize {
		return s
	}
	// The cut may split a multi-byte character; drop what is left of it.
	return "…" + strings.ToValidUTF8(s[len(s)-outputTailSize:], "")
}

func readResult(path, workspace string) (contract.Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return contract.Result{}, fmt.Errorf("result.json missing: %w", err)
	}
	var r contract.Result
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("invalid result.json: %w", err)
	}
	if r.ContractVersion != contract.Version {
		return r, fmt.Errorf("unsupported result contract version %q", r.ContractVersion)
	}
	for _, f := range r.OutputFiles {
		clean := filepath.Clean(filepath.FromSlash(f.Path))
		if filepath.IsAbs(clean) || clean == "output" || !strings.HasPrefix(clean, "output"+string(os.PathSeparator)) {
			return r, fmt.Errorf("invalid output path %q", f.Path)
		}
		if _, err := os.Stat(filepath.Join(workspace, clean)); err != nil {
			return r, fmt.Errorf("declared output missing %q", f.Path)
		}
	}
	return r, nil
}
func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
