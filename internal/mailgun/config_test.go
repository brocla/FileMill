package mailgun

import (
	"strings"
	"testing"
)

// A typo in a delivery mode must not be a silently inert route: the operator
// would see mail arriving as attachments and no reason why.
func TestParseDeliveryRejectsUnknownMode(t *testing.T) {
	_, err := parseDelivery(map[string]string{"iwk@mill.test": "sheets-linc"})
	if err == nil {
		t.Fatal("an unknown delivery mode must be rejected")
	}
	if !strings.Contains(err.Error(), "sheets-linc") || !strings.Contains(err.Error(), "iwk@mill.test") {
		t.Errorf("the error must name the bad value and its address; got %v", err)
	}
}

func TestParseDeliveryAcceptsKnownModesAndLowercasesAddresses(t *testing.T) {
	got, err := parseDelivery(map[string]string{
		"IWK@Mill.Test":   modeSheetsLink,
		"excel@mill.test": modeEmail,
	})
	if err != nil {
		t.Fatalf("parseDelivery: %v", err)
	}
	if got["iwk@mill.test"] != modeSheetsLink {
		t.Errorf("address lookup must be case-insensitive, like routes; got %v", got)
	}
	if got["excel@mill.test"] != modeEmail {
		t.Errorf("an explicit email mode must survive; got %v", got)
	}
}

// Link delivery converts an xlsx into a native Sheet, so the route's transformer
// should be producing the sheets layout. Drive will convert an Excel-layout file
// too — the result is just worse — so this is a quality coupling, not a hard
// error. One warning makes a future slip visible without failing the worker.
func TestDeliveryPairingWarnsOnceWhenLayoutIsWrong(t *testing.T) {
	logger, buf := captureLogger()
	warnDeliveryPairing(logger,
		map[string]string{"iwk@mill.test": "workerlist_excel"},
		map[string]string{"iwk@mill.test": modeSheetsLink},
		func(operation string) string { return "excel" })

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one warning line; got %q", out)
	}
	if !strings.Contains(out, "iwk@mill.test") || !strings.Contains(out, "workerlist_excel") {
		t.Errorf("the warning must name the address and its operation; got %q", out)
	}
}

// A correct pairing must say nothing at all. A warning that fires on healthy
// config is noise, and noise is how a real warning gets ignored.
func TestDeliveryPairingSilentWhenCorrect(t *testing.T) {
	logger, buf := captureLogger()
	warnDeliveryPairing(logger,
		map[string]string{"iwk@mill.test": "workerlist_sheets", "excel@mill.test": "workerlist_excel"},
		map[string]string{"iwk@mill.test": modeSheetsLink},
		func(operation string) string {
			if operation == "workerlist_sheets" {
				return "sheets"
			}
			return "excel"
		})

	if buf.String() != "" {
		t.Errorf("a correct pairing must log nothing; got %q", buf.String())
	}
}

// An email-mode route is not subject to the pairing rule at all.
func TestDeliveryPairingIgnoresEmailRoutes(t *testing.T) {
	logger, buf := captureLogger()
	warnDeliveryPairing(logger,
		map[string]string{"excel@mill.test": "workerlist_excel"},
		map[string]string{"excel@mill.test": modeEmail},
		func(operation string) string { return "excel" })

	if buf.String() != "" {
		t.Errorf("an email route must not be checked for layout; got %q", buf.String())
	}
}

// A delivery entry for an address with no route is a dead entry — most likely a
// typo in the address — and worth one warning.
func TestDeliveryPairingWarnsOnUnroutedAddress(t *testing.T) {
	logger, buf := captureLogger()
	warnDeliveryPairing(logger,
		map[string]string{"iwk@mill.test": "workerlist_sheets"},
		map[string]string{"iwq@mill.test": modeSheetsLink},
		func(operation string) string { return "sheets" })

	if !strings.Contains(buf.String(), "iwq@mill.test") {
		t.Errorf("an unrouted delivery address must be flagged; got %q", buf.String())
	}
}

// Credentials are only required when a route actually needs them, so the
// feature stays off until deliberately configured.
func TestGoogleCredentialsRequiredOnlyForSheetsRoutes(t *testing.T) {
	if err := requireGoogleCredentials(map[string]string{"iwk@mill.test": modeEmail}, credentials{}); err != nil {
		t.Errorf("no sheets-link route must not require credentials; got %v", err)
	}

	err := requireGoogleCredentials(map[string]string{"iwk@mill.test": modeSheetsLink}, credentials{clientID: "id"})
	if err == nil {
		t.Fatal("a sheets-link route without credentials must fail at load")
	}
	if !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_SECRET") || !strings.Contains(err.Error(), "GOOGLE_OAUTH_REFRESH_TOKEN") {
		t.Errorf("the error must name the missing variables; got %v", err)
	}
	if strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_ID") {
		t.Errorf("the error must not name a variable that is set; got %v", err)
	}

	full := credentials{clientID: "id", clientSecret: "secret", refreshToken: "token"}
	if err := requireGoogleCredentials(map[string]string{"iwk@mill.test": modeSheetsLink}, full); err != nil {
		t.Errorf("complete credentials must satisfy the check; got %v", err)
	}
}
