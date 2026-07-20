package mailgun

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"filemill/internal/app"
	"gopkg.in/yaml.v3"
)

const (
	// defaultMaxBytes caps a single attachment when email.yaml does not.
	defaultMaxBytes = 20 << 20 // 20 MiB
	// mailgunAPI is Mailgun's Send API host, used unless a Service overrides it.
	mailgunAPI = "https://api.mailgun.net"
)

// fileConfig mirrors config/email.yaml. Secrets live in the environment, not
// this file, so it is safe to keep in version control.
type fileConfig struct {
	Routes  map[string]string `yaml:"routes"`
	Allowed []string          `yaml:"allowed_senders"`
	Max     int64             `yaml:"max_attachment_bytes"`
}

// Load reads config/email.yaml and the Mailgun secrets from the environment.
// It returns (nil, nil) when Mailgun is not configured at all, so the worker
// can run without the email adapter, and an error only when the configuration
// is present but incomplete.
func Load(root string, engine *app.App, logger *log.Logger) (*Service, error) {
	raw, err := os.ReadFile(filepath.Join(root, "config", "email.yaml"))
	if err != nil {
		return nil, err
	}
	var cfg fileConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}

	s := &Service{
		engine:   engine,
		signKey:  os.Getenv("MAILGUN_WEBHOOK_SIGNING_KEY"),
		apiKey:   os.Getenv("MAILGUN_API_KEY"),
		domain:   os.Getenv("MAILGUN_DOMAIN"),
		from:     os.Getenv("REPLY_FROM"),
		routes:   make(map[string]string, len(cfg.Routes)),
		allowed:  make(map[string]bool, len(cfg.Allowed)),
		maxBytes: cfg.Max,
		sendBase: mailgunAPI,
		log:      logger,
	}
	if s.maxBytes == 0 {
		s.maxBytes = defaultMaxBytes
	}
	for address, operation := range cfg.Routes {
		s.routes[strings.ToLower(address)] = operation
	}
	for _, sender := range cfg.Allowed {
		s.allowed[strings.ToLower(sender)] = true
	}

	switch {
	case s.apiKey == "" && s.signKey == "" && s.domain == "" && s.from == "":
		return nil, nil // Mailgun not configured; the adapter stays disabled.
	case s.apiKey == "" || s.signKey == "" || s.domain == "" || s.from == "":
		return nil, fmt.Errorf("mailgun configuration is incomplete: set MAILGUN_API_KEY, MAILGUN_WEBHOOK_SIGNING_KEY, MAILGUN_DOMAIN, and REPLY_FROM")
	}
	return s, nil
}
