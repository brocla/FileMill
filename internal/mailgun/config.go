package mailgun

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filemill/internal/app"
	"filemill/internal/gsheets"
	"gopkg.in/yaml.v3"
)

const (
	// defaultMaxBytes caps a single attachment when email.yaml does not.
	defaultMaxBytes = 20 << 20 // 20 MiB
	// defaultSendTimeoutSeconds bounds an outbound Mailgun send when email.yaml
	// does not, so a hung connection can't stall the delivery loop forever.
	defaultSendTimeoutSeconds = 30
	// mailgunAPI is Mailgun's Send API host, used unless a Service overrides it.
	mailgunAPI = "https://api.mailgun.net"
)

// fileConfig mirrors config/email.yaml. Secrets live in the environment, not
// this file, so it is safe to keep in version control.
type fileConfig struct {
	Routes      map[string]string `yaml:"routes"`
	Delivery    map[string]string `yaml:"delivery"`
	Allowed     []string          `yaml:"allowed_senders"`
	Max         int64             `yaml:"max_attachment_bytes"`
	SendTimeout int               `yaml:"send_timeout_seconds"`
}

// credentials holds the Google OAuth secrets the sheets-link delivery mode
// needs. They live in the environment, never in email.yaml, matching the
// MAILGUN_* pattern.
type credentials struct{ clientID, clientSecret, refreshToken string }

func googleCredentials() credentials {
	return credentials{
		clientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		clientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		refreshToken: os.Getenv("GOOGLE_OAUTH_REFRESH_TOKEN"),
	}
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
	sendTimeout := cfg.SendTimeout
	if sendTimeout <= 0 {
		sendTimeout = defaultSendTimeoutSeconds
	}
	s.client = &http.Client{Timeout: time.Duration(sendTimeout) * time.Second}
	for address, operation := range cfg.Routes {
		s.routes[strings.ToLower(address)] = operation
	}
	for _, sender := range cfg.Allowed {
		s.allowed[strings.ToLower(sender)] = true
	}

	delivery, err := parseDelivery(cfg.Delivery)
	if err != nil {
		return nil, err
	}
	s.delivery = delivery

	switch {
	case s.apiKey == "" && s.signKey == "" && s.domain == "" && s.from == "":
		return nil, nil // Mailgun not configured; the adapter stays disabled.
	case s.apiKey == "" || s.signKey == "" || s.domain == "" || s.from == "":
		return nil, fmt.Errorf("mailgun configuration is incomplete: set MAILGUN_API_KEY, MAILGUN_WEBHOOK_SIGNING_KEY, MAILGUN_DOMAIN, and REPLY_FROM")
	}

	// Link delivery reaches an external service, so a route that asks for it and
	// cannot have it is a hard failure at startup rather than a surprise hours
	// later. There is deliberately no fallback to attachment delivery: sending
	// the data by a channel the operator did not configure is worse than not
	// sending it.
	creds := googleCredentials()
	if err := requireGoogleCredentials(s.delivery, creds); err != nil {
		return nil, err
	}
	if usesSheetsLink(s.delivery) {
		s.publisher = gsheets.New(creds.clientID, creds.clientSecret, creds.refreshToken)
	}
	warnDeliveryPairing(s.log, s.routes, s.delivery, func(operation string) string {
		layout, _ := engine.OperationOptions(operation)["layout"].(string)
		return layout
	})
	return s, nil
}

// parseDelivery normalizes the address -> delivery mode map and rejects any
// value that is not a known mode. An unrecognized mode is a typo, and a typo
// that silently left the route on attachment delivery would be invisible.
func parseDelivery(raw map[string]string) (map[string]string, error) {
	delivery := make(map[string]string, len(raw))
	for address, mode := range raw {
		switch mode {
		case modeEmail, modeSheetsLink:
			delivery[strings.ToLower(address)] = mode
		default:
			return nil, fmt.Errorf("unknown delivery mode %q for %s: use %q or %q", mode, address, modeEmail, modeSheetsLink)
		}
	}
	return delivery, nil
}

// usesSheetsLink reports whether any route asks for link delivery. Nothing
// Google-related is required, constructed, or contacted when none does.
func usesSheetsLink(delivery map[string]string) bool {
	for _, mode := range delivery {
		if mode == modeSheetsLink {
			return true
		}
	}
	return false
}

// requireGoogleCredentials fails when a route asks for link delivery but the
// OAuth secrets it needs are not in the environment. With no sheets-link route
// the credentials are not needed at all, which is what keeps the feature off
// until it is deliberately configured.
func requireGoogleCredentials(delivery map[string]string, creds credentials) error {
	if !usesSheetsLink(delivery) {
		return nil
	}
	var missing []string
	for _, pair := range []struct {
		name, value string
	}{
		{"GOOGLE_OAUTH_CLIENT_ID", creds.clientID},
		{"GOOGLE_OAUTH_CLIENT_SECRET", creds.clientSecret},
		{"GOOGLE_OAUTH_REFRESH_TOKEN", creds.refreshToken},
	} {
		if pair.value == "" {
			missing = append(missing, pair.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s delivery is configured but incomplete: set %s", modeSheetsLink, strings.Join(missing, ", "))
	}
	return nil
}

// warnDeliveryPairing reports delivery routes whose transformer is not producing
// the layout link delivery expects. Drive converts an Excel-layout workbook too,
// so this is a quality coupling rather than a hard error — but a silent slip
// would show up only as a worse-looking spreadsheet weeks later. It logs only
// when something is actually wrong: a warning on healthy config is noise.
func warnDeliveryPairing(logger *log.Logger, routes, delivery map[string]string, layoutOf func(operation string) string) {
	for address, mode := range delivery {
		if mode != modeSheetsLink {
			continue
		}
		operation, routed := routes[address]
		if !routed {
			logger.Printf("config warning: %s delivery is set for %s, which no route serves", mode, address)
			continue
		}
		if layout := layoutOf(operation); layout != "sheets" {
			logger.Printf("config warning: %s delivery is set for %s, but its operation %s produces layout %q, not \"sheets\"", mode, address, operation, layout)
		}
	}
}
