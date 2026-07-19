package mailgun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

func TestValidSignature(t *testing.T) {
	key, timestamp, token := "signing-key", strconv.FormatInt(time.Now().Unix(), 10), "token-value"
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + token))
	signature := hex.EncodeToString(mac.Sum(nil))
	if !valid(key, timestamp, token, signature) {
		t.Fatal("expected valid signature")
	}
	if valid(key, timestamp, token, "forged") {
		t.Fatal("accepted forged signature")
	}
}

func TestRejectsStaleSignature(t *testing.T) {
	key, timestamp, token := "signing-key", "1", "token-value"
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + token))
	if valid(key, timestamp, token, hex.EncodeToString(mac.Sum(nil))) {
		t.Fatal("accepted stale signature")
	}
}
