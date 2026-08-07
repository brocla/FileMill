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
	"strings"
	"time"

	"filemill/internal/config"
	"filemill/internal/contract"
	"filemill/internal/store"
	"github.com/google/uuid"
)

const timeout = 10 * time.Minute

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
	return &App{root: root, data: data, cfg: cfg, store: s, log: log.New(lf, "", log.LstdFlags|log.LUTC), logFile: lf}, nil
}
func (a *App) Close() error { a.logFile.Close(); return a.store.Close() }

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
func (a *App) Run(ctx context.Context, once bool) error {
	a.log.Printf("worker started once=%t", once)
	for {
		j, err := a.store.Next()
		if err != nil {
			if once {
				return err
			}
			// A transient store error (e.g. SQLITE_BUSY under load) must not
			// take down the worker: log it, back off, and retry.
			a.log.Printf("worker claim error: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
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
func (a *App) execute(parent context.Context, j store.Job) {
	t, ok := a.cfg.Find(j.Operation)
	if !ok {
		a.finish(j.ID, store.StatusFailed, "registered transformer no longer exists")
		return
	}
	workspace := filepath.Join(a.data, "jobs", j.ID)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	args := append(append([]string{}, t.Command[1:]...), "job.json")
	cmd := exec.CommandContext(ctx, t.Command[0], args...)
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		a.log.Printf("job=%s transformer_output=%s", j.ID, strings.TrimSpace(string(output)))
	}
	if ctx.Err() == context.DeadlineExceeded {
		a.finish(j.ID, store.StatusFailed, "transformer timed out after 10 minutes")
		return
	}
	result, readErr := readResult(filepath.Join(workspace, "result.json"), workspace)
	if err != nil {
		msg := "transformer exited unsuccessfully"
		if readErr == nil && result.Message != "" {
			msg = result.Message
		}
		a.finish(j.ID, store.StatusFailed, msg)
		return
	}
	if readErr != nil {
		a.finish(j.ID, store.StatusFailed, readErr.Error())
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
