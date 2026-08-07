package gsheets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLivePublishRoundTrip talks to the real Google Drive API with the
// operator's OAuth credentials: it publishes a small file, checks the link
// looks like a Sheet, and deletes it again. Nothing is left behind.
//
// It is gated like the mailgun end-to-end test because it needs real
// credentials and the network. Run with:
//
//	FILEMILL_GOOGLE_E2E=1 go test ./internal/gsheets -run Live -v
//
// This is what proves the parts unit tests deliberately fake: that the refresh
// token still works, that Drive accepts the multipart framing and converts the
// upload, and that fields=id,webViewLink returns the link in the same call.
func TestLivePublishRoundTrip(t *testing.T) {
	if os.Getenv("FILEMILL_GOOGLE_E2E") == "" {
		t.Skip("set FILEMILL_GOOGLE_E2E=1 to run the live Google Drive test")
	}
	clientID := os.Getenv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")
	refreshToken := os.Getenv("GOOGLE_OAUTH_REFRESH_TOKEN")
	if clientID == "" || clientSecret == "" || refreshToken == "" {
		t.Skip("GOOGLE_OAUTH_CLIENT_ID/_SECRET/_REFRESH_TOKEN must all be set")
	}

	// A minimal valid xlsx is not needed: Drive converts what it is given, and
	// this test is about the transport, not the spreadsheet.
	path := filepath.Join(t.TempDir(), "filemill-live-test.xlsx")
	if err := os.WriteFile(path, []byte("name,value\nlive test,1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	client := New(clientID, clientSecret, refreshToken)
	ctx := context.Background()

	fileID, link, err := client.Publish(ctx, path, "FileMill live test — safe to delete")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	t.Logf("published %s -> %s", fileID, link)

	// Always clean up, even if the assertions below fail.
	t.Cleanup(func() {
		if err := client.Delete(ctx, fileID); err != nil {
			t.Errorf("cleanup delete of %s failed — remove it by hand: %v", fileID, err)
		}
	})

	if !strings.Contains(link, "docs.google.com/spreadsheets") {
		t.Errorf("link = %q, want a Sheets URL — the conversion may not have happened", link)
	}

	// Deleting twice must be safe, which is what lets the retention sweep repeat.
	if err := client.Delete(ctx, fileID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := client.Delete(ctx, fileID); err != nil {
		t.Errorf("deleting an already-deleted file must succeed; got %v", err)
	}
}
