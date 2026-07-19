package mailgun

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"filemill/internal/app"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type fileConfig struct {
	Routes  map[string]string `yaml:"routes"`
	Allowed []string          `yaml:"allowed_senders"`
	Max     int64             `yaml:"max_attachment_bytes"`
}
type Service struct {
	app                     *app.App
	key, sign, domain, from string
	routes                  map[string]string
	allowed                 map[string]bool
	max                     int64
	sendBase                string
	log                     *log.Logger
}

// mailgunAPI is Mailgun's Send API host. Overridable via Service.sendBase so
// integration tests can point outbound mail at a local fake endpoint.
const mailgunAPI = "https://api.mailgun.net"

func Load(root string, a *app.App, l *log.Logger) (*Service, error) {
	b, e := os.ReadFile(filepath.Join(root, "config", "email.yaml"))
	if e != nil {
		return nil, e
	}
	var f fileConfig
	if e = yaml.Unmarshal(b, &f); e != nil {
		return nil, e
	}
	s := &Service{app: a, key: os.Getenv("MAILGUN_API_KEY"), sign: os.Getenv("MAILGUN_WEBHOOK_SIGNING_KEY"), domain: os.Getenv("MAILGUN_DOMAIN"), from: os.Getenv("REPLY_FROM"), routes: map[string]string{}, allowed: map[string]bool{}, max: f.Max, sendBase: mailgunAPI, log: l}
	if s.max == 0 {
		s.max = 20 << 20
	}
	for k, v := range f.Routes {
		s.routes[strings.ToLower(k)] = v
	}
	for _, v := range f.Allowed {
		s.allowed[strings.ToLower(v)] = true
	}
	if s.key == "" && s.sign == "" && s.domain == "" && s.from == "" {
		return nil, nil
	}
	if s.key == "" || s.sign == "" || s.domain == "" || s.from == "" {
		return nil, fmt.Errorf("Mailgun environment is incomplete")
	}
	return s, nil
}
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.handle) }
func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.max+1<<20)
	var e error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		e = r.ParseMultipartForm(s.max)
	} else {
		e = r.ParseForm()
	}
	if e != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if !valid(s.sign, r.FormValue("timestamp"), r.FormValue("token"), r.FormValue("signature")) {
		http.Error(w, "signature", 401)
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) == 0 {
		w.WriteHeader(200)
		return
	}
	op, ok := s.routes[strings.ToLower(r.FormValue("recipient"))]
	if !ok {
		w.WriteHeader(200)
		return
	}
	sender := r.FormValue("sender")
	if len(s.allowed) > 0 && !s.allowed[strings.ToLower(sender)] {
		w.WriteHeader(200)
		return
	}
	// Collect and size-check every attachment before persisting anything.
	// Validating up front keeps a rejected request from leaving behind a
	// submission group whose expected_jobs count can never be satisfied,
	// which would block redelivery and be silently dropped on Mailgun retry.
	var files []*multipart.FileHeader
	for k, v := range r.MultipartForm.File {
		if strings.HasPrefix(k, "attachment-") {
			files = append(files, v...)
		}
	}
	if len(files) == 0 {
		w.WriteHeader(200)
		return
	}
	for _, f := range files {
		if f.Size > s.max {
			http.Error(w, "attachment too large", 400)
			return
		}
	}
	mid := r.FormValue("Message-Id")
	if mid == "" {
		mid = "mailgun:" + r.FormValue("token")
	}
	sid, created, e := s.app.BeginEmail(mid, sender, r.FormValue("recipient"), r.FormValue("subject"))
	if e != nil {
		http.Error(w, "storage", 500)
		return
	}
	if !created {
		w.WriteHeader(200)
		return
	}
	if e = s.app.SetEmailExpected(sid, len(files)); e != nil {
		http.Error(w, "storage", 500)
		return
	}
	for i, f := range files {
		p, e := save(f, s.max)
		if e != nil {
			http.Error(w, "attachment", 400)
			return
		}
		id, e := s.app.Submit(op, p)
		os.RemoveAll(filepath.Dir(p))
		if e != nil {
			http.Error(w, "submit", 500)
			return
		}
		if e = s.app.AddEmailJob(sid, i, id); e != nil {
			http.Error(w, "storage", 500)
			return
		}
	}
	w.WriteHeader(200)
}
func valid(k, t, token, sig string) bool {
	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil || time.Since(time.Unix(unix, 0)) > 15*time.Minute || time.Until(time.Unix(unix, 0)) > 5*time.Minute {
		return false
	}
	m := hmac.New(sha256.New, []byte(k))
	m.Write([]byte(t + token))
	return k != "" && hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(sig))
}
// save writes an inbound attachment to a fresh temp directory under its own
// basename, so the file keeps its original extension. App.Submit matches the
// transformer by extension (Transformer.Accepts) and transformers such as
// workerlist expect a real ".pdf", so a bare random temp name would be
// rejected. The caller removes the returned file's parent directory.
func save(h *multipart.FileHeader, max int64) (string, error) {
	f, e := h.Open()
	if e != nil {
		return "", e
	}
	defer f.Close()
	name := filepath.Base(filepath.FromSlash(h.Filename))
	if name == "." || name == string(os.PathSeparator) || name == "" {
		name = "attachment"
	}
	dir, e := os.MkdirTemp("", "filemill-mail-*")
	if e != nil {
		return "", e
	}
	path := filepath.Join(dir, name)
	x, e := os.Create(path)
	if e != nil {
		os.RemoveAll(dir)
		return "", e
	}
	defer x.Close()
	n, e := io.Copy(x, io.LimitReader(f, max+1))
	if e != nil {
		os.RemoveAll(dir)
		return "", e
	}
	if n > max {
		os.RemoveAll(dir)
		return "", fmt.Errorf("attachment exceeds %d byte limit", max)
	}
	return path, nil
}

// Deliver is reserved for the durable outbound delivery loop. The inbound
// handler intentionally returns immediately after the jobs have been queued.
func (s *Service) Deliver(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.deliverOnce(); err != nil {
			s.log.Printf("delivery: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) deliverOnce() error {
	subs, err := s.app.PendingEmails()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		done := true
		var text []string
		var outputs []string
		for _, item := range sub.Jobs {
			if item.Job.Status == "queued" || item.Job.Status == "running" {
				done = false
				break
			}
			text = append(text, fmt.Sprintf("%s: %s", item.Job.InputName, item.Job.Message))
			if item.Job.Status == "succeeded" {
				files, e := s.app.Outputs(item.Job.ID)
				if e != nil {
					return e
				}
				for _, f := range files {
					outputs = append(outputs, f.Path)
				}
			}
		}
		if !done {
			continue
		}
		if err := s.send(sub.Sender, sub.Subject, sub.MessageID, strings.Join(text, "\n"), outputs); err != nil {
			return err
		}
		if err := s.app.MarkEmailDelivered(sub.ID); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) send(to, subject, messageID, text string, outputs []string) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fields := map[string]string{"from": s.from, "to": to, "subject": replySubject(subject), "text": text}
	if messageID != "" {
		fields["h:In-Reply-To"] = messageID
		fields["h:References"] = messageID
	}
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			return err
		}
	}
	for _, path := range outputs {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		part, err := w.CreateFormFile("attachment", filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, in)
		}
		in.Close()
		if err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	base := s.sendBase
	if base == "" {
		base = mailgunAPI
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v3/%s/messages", base, s.domain), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.SetBasicAuth("api", s.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Mailgun send returned %s", resp.Status)
	}
	return nil
}
func replySubject(subject string) string {
	if subject == "" {
		return "Re: FileMill result"
	}
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	return "Re: " + subject
}
