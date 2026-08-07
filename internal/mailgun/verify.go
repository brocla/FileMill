package mailgun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// This file is the trust boundary. notify.keywind.cc is a public URL, so every
// inbound request is treated as hostile until it clears these checks:
//
//	authenticSignature - proves the POST came from Mailgun and is fresh
//	operationFor       - the recipient address is one we serve
//	senderAllowed      - the envelope sender is permitted to submit
//	processable        - exactly the attachments the transformer can handle
//	withinSizeLimit    - no attachment exceeds the configured cap
//
// Everything an attacker must defeat lives here and nowhere else.

const (
	// maxSignatureAge rejects replays: signatures older than this are refused
	// even when the HMAC is valid.
	maxSignatureAge = 15 * time.Minute
	// maxSignatureSkew rejects timestamps implausibly far in the future.
	maxSignatureSkew = 5 * time.Minute
)

// authenticSignature reports whether timestamp+token were signed with the
// webhook signing key and the timestamp falls inside the freshness window.
// Mailgun signs every notify POST as HMAC-SHA256 over timestamp+token.
func authenticSignature(signKey, timestamp, token, signature string) bool {
	if signKey == "" {
		return false
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	when := time.Unix(unix, 0)
	if time.Since(when) > maxSignatureAge || time.Until(when) > maxSignatureSkew {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signKey))
	mac.Write([]byte(timestamp + token))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// operationFor returns the transformer operation routed to a recipient address,
// or ok=false when the address is not served.
func (s *Service) operationFor(recipient string) (operation string, ok bool) {
	operation, ok = s.routes[strings.ToLower(recipient)]
	return operation, ok
}

// senderAllowed reports whether an envelope sender may submit work. An empty
// allowlist permits everyone.
func (s *Service) senderAllowed(sender string) bool {
	if len(s.allowed) == 0 {
		return true
	}
	return s.allowed[strings.ToLower(sender)]
}

// processable narrows a message's attachment parts to the ones the routed
// transformer actually accepts.
//
// Mail carries parts we never asked for — an inline signature logo arrives as
// just another attachment part. Counting those would make an ordinary one-PDF
// submission look like several, so they are filtered out before the message is
// judged, not counted against it.
func (s *Service) processable(operation string, files []*multipart.FileHeader) []*multipart.FileHeader {
	var keep []*multipart.FileHeader
	for _, fh := range files {
		if s.engine.Accepts(operation, filepath.Base(filepath.FromSlash(fh.Filename))) {
			keep = append(keep, fh)
		}
	}
	return keep
}

// withinSizeLimit reports whether every attachment is within the per-attachment
// byte limit. The check uses the multipart header size so an oversize request
// is refused before any job workspace is created.
func (s *Service) withinSizeLimit(files []*multipart.FileHeader) bool {
	for _, fh := range files {
		if fh.Size > s.maxBytes {
			return false
		}
	}
	return true
}
