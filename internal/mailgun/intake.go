package mailgun

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
)

// intake turns a verified email's attachments into jobs and records the
// submission group.
//
// It is atomic by construction rather than by transaction. expected_jobs is set
// only after every job mapping is durable, and the delivery loop ignores any
// submission whose expected_jobs is still zero, so a group is never delivered
// half-built. A failure partway simply leaves the group invisible; because the
// submission id is keyed on Message-Id, a Mailgun retry resumes the same group,
// and EmailHasJob skips attachments already submitted so no job is duplicated.
//
// The one residual window (chosen over full transactionality for simplicity) is
// a crash between Submit and AddEmailJob: a retry would re-submit that single
// attachment. It is small, self-limited to one attachment, and only produces a
// duplicate job, never a lost or mis-delivered one.
func (s *Service) intake(r *http.Request, operation string, files []*multipart.FileHeader) error {
	messageID := r.FormValue("Message-Id")
	idempotencyKey := messageID
	if idempotencyKey == "" {
		// No Message-Id: fall back to Mailgun's token so retries still coalesce.
		idempotencyKey = "mailgun:" + r.FormValue("token")
	}

	sid, _, err := s.engine.BeginEmail(idempotencyKey, r.FormValue("sender"), r.FormValue("recipient"), r.FormValue("subject"))
	if err != nil {
		return fmt.Errorf("begin submission: %w", err)
	}

	for i, fh := range files {
		submitted, err := s.engine.EmailHasJob(sid, i)
		if err != nil {
			return fmt.Errorf("check attachment %d: %w", i, err)
		}
		if submitted {
			continue // already handled on an earlier attempt
		}
		jobID, err := s.submitAttachment(operation, fh)
		if err != nil {
			return fmt.Errorf("attachment %d: %w", i, err)
		}
		if err := s.engine.AddEmailJob(sid, i, jobID); err != nil {
			return fmt.Errorf("record attachment %d: %w", i, err)
		}
	}

	// Commit point: the group becomes visible to the delivery loop only now.
	if err := s.engine.SetEmailExpected(sid, len(files)); err != nil {
		return fmt.Errorf("finalize submission: %w", err)
	}
	return nil
}

// submitAttachment saves one attachment to a temp workspace, submits it as a
// job, and cleans up the temp copy regardless of the outcome.
func (s *Service) submitAttachment(operation string, fh *multipart.FileHeader) (string, error) {
	path, err := saveAttachment(fh, s.maxBytes)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(filepath.Dir(path))
	return s.engine.Submit(operation, path)
}

// saveAttachment writes an inbound attachment to a fresh temp directory under
// its original basename, preserving the extension App.Submit matches on
// (Transformer.Accepts) — a bare random temp name would be rejected. The caller
// removes the returned file's parent directory.
func saveAttachment(fh *multipart.FileHeader, maxBytes int64) (string, error) {
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()

	name := filepath.Base(filepath.FromSlash(fh.Filename))
	if name == "." || name == string(os.PathSeparator) || name == "" {
		name = "attachment"
	}

	dir, err := os.MkdirTemp("", "filemill-mail-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	dst, err := os.Create(path)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	defer dst.Close()

	written, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if written > maxBytes {
		os.RemoveAll(dir)
		return "", fmt.Errorf("attachment exceeds %d byte limit", maxBytes)
	}
	return path, nil
}
