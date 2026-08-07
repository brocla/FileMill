package gsheets

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGoogle stands in for the token endpoint and the Drive API, recording what
// the client actually sent. Request construction is the part of a hand-rolled
// client that can be wrong, so it is what these tests pin down; nothing here
// reaches the network.
type fakeGoogle struct {
	server *httptest.Server

	tokenRequests   int
	uploadRequests  int
	uploadedName    string // "name" from the metadata part
	uploadedMime    string // "mimeType" from the metadata part
	uploadedBytes   string // the media part
	uploadedType    string // the request's Content-Type header
	uploadAuth      string // the request's Authorization header
	uploadQuery     string
	permissionBody  map[string]string
	permissionFor   string // file id taken from the permission URL path
	deletedFileIDs  []string
	tokenStatus     int // non-zero overrides the token response status
	uploadStatus    int
	shareStatus     int
	deleteStatus    int
	accessTokenBody string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{accessTokenBody: `{"access_token":"access-123","expires_in":3600}`}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		g.tokenRequests++
		if err := r.ParseForm(); err != nil {
			t.Errorf("token form: %v", err)
		}
		if got := r.PostFormValue("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", got)
		}
		if got := r.PostFormValue("refresh_token"); got != "refresh-token" {
			t.Errorf("refresh_token = %q", got)
		}
		if got := r.PostFormValue("client_id"); got != "client-id" {
			t.Errorf("client_id = %q", got)
		}
		if got := r.PostFormValue("client_secret"); got != "client-secret" {
			t.Errorf("client_secret = %q", got)
		}
		if g.tokenStatus != 0 {
			http.Error(w, `{"error":"invalid_grant"}`, g.tokenStatus)
			return
		}
		_, _ = io.WriteString(w, g.accessTokenBody)
	})

	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		g.uploadRequests++
		g.uploadAuth = r.Header.Get("Authorization")
		g.uploadedType = r.Header.Get("Content-Type")
		g.uploadQuery = r.URL.RawQuery
		if g.uploadStatus != 0 {
			http.Error(w, `{"error":{"message":"quota exceeded"}}`, g.uploadStatus)
			return
		}

		_, params, err := mime.ParseMediaType(g.uploadedType)
		if err != nil {
			t.Errorf("parse upload content type: %v", err)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		metadata, err := reader.NextPart()
		if err != nil {
			t.Errorf("metadata part: %v", err)
			return
		}
		var meta map[string]string
		if err := json.NewDecoder(metadata).Decode(&meta); err != nil {
			t.Errorf("decode metadata part: %v", err)
		}
		g.uploadedName, g.uploadedMime = meta["name"], meta["mimeType"]

		media, err := reader.NextPart()
		if err != nil {
			t.Errorf("media part: %v", err)
			return
		}
		body, _ := io.ReadAll(media)
		g.uploadedBytes = string(body)

		_, _ = io.WriteString(w, `{"id":"file-abc","webViewLink":"https://docs.google.com/spreadsheets/d/file-abc/edit"}`)
	})

	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		switch {
		case r.Method == http.MethodDelete:
			g.deletedFileIDs = append(g.deletedFileIDs, rest)
			if g.deleteStatus != 0 {
				http.Error(w, `{"error":{"message":"gone"}}`, g.deleteStatus)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(rest, "/permissions"):
			g.permissionFor = strings.TrimSuffix(rest, "/permissions")
			if g.shareStatus != 0 {
				http.Error(w, `{"error":{"message":"insufficient permissions"}}`, g.shareStatus)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&g.permissionBody); err != nil {
				t.Errorf("decode permission body: %v", err)
			}
			_, _ = io.WriteString(w, `{"id":"perm-1"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

// client returns a Client pointed at the fake.
func (g *fakeGoogle) client() *Client {
	c := New("client-id", "client-secret", "refresh-token")
	c.tokenURL = g.server.URL + "/token"
	c.apiBase = g.server.URL
	c.uploadBase = g.server.URL + "/upload"
	c.http = g.server.Client()
	return c
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole request shape in one pass: the conversion mimeType (without it
// Drive stores an .xlsx instead of making a Sheet), the multipart/related
// framing Drive requires, the bearer token, and the link-sharing permission.
func TestPublishUploadsConvertsAndShares(t *testing.T) {
	google := newFakeGoogle(t)
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	fileID, link, err := google.client().Publish(context.Background(), path, "schedule")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if fileID != "file-abc" {
		t.Errorf("fileID = %q, want file-abc", fileID)
	}
	if link != "https://docs.google.com/spreadsheets/d/file-abc/edit" {
		t.Errorf("link = %q", link)
	}
	if google.uploadedMime != sheetMimeType {
		t.Errorf("mimeType = %q, want %q — without it Drive stores a file instead of converting it", google.uploadedMime, sheetMimeType)
	}
	if google.uploadedName != "schedule" {
		t.Errorf("name = %q, want schedule", google.uploadedName)
	}
	if google.uploadedBytes != "xlsx bytes" {
		t.Errorf("uploaded body = %q", google.uploadedBytes)
	}
	if !strings.HasPrefix(google.uploadedType, "multipart/related; boundary=") {
		t.Errorf("Content-Type = %q, want multipart/related with a boundary", google.uploadedType)
	}
	if google.uploadAuth != "Bearer access-123" {
		t.Errorf("Authorization = %q", google.uploadAuth)
	}
	if !strings.Contains(google.uploadQuery, "uploadType=multipart") {
		t.Errorf("query = %q, want uploadType=multipart", google.uploadQuery)
	}
	if !strings.Contains(google.uploadQuery, "fields=id%2CwebViewLink") {
		t.Errorf("query = %q, want the link requested in one call", google.uploadQuery)
	}
	if google.permissionFor != "file-abc" {
		t.Errorf("permission created for %q, want file-abc", google.permissionFor)
	}
	if google.permissionBody["type"] != "anyone" || google.permissionBody["role"] != "writer" {
		t.Errorf("permission = %v, want anyone/writer", google.permissionBody)
	}
}

// A file that uploads but cannot be shared must not stay in Drive: it would be
// an unshared, unrecorded copy of the sender's data that the retention sweep
// never hears about.
func TestPublishDeletesTheFileWhenSharingFails(t *testing.T) {
	google := newFakeGoogle(t)
	google.shareStatus = http.StatusForbidden
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	_, _, err := google.client().Publish(context.Background(), path, "schedule")
	if err == nil {
		t.Fatal("expected a sharing error")
	}
	if !strings.Contains(err.Error(), "insufficient permissions") {
		t.Errorf("the error must carry Google's reason; got %v", err)
	}
	if len(google.deletedFileIDs) != 1 || google.deletedFileIDs[0] != "file-abc" {
		t.Errorf("the orphan must be deleted; deleted = %v", google.deletedFileIDs)
	}
}

// If the cleanup delete also fails there is a real orphan, and the error has to
// name it — it is the only way anyone will find it.
func TestPublishNamesTheOrphanWhenCleanupFails(t *testing.T) {
	google := newFakeGoogle(t)
	google.shareStatus = http.StatusForbidden
	google.deleteStatus = http.StatusInternalServerError
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	_, _, err := google.client().Publish(context.Background(), path, "schedule")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "file-abc") || !strings.Contains(err.Error(), "orphaned") {
		t.Errorf("the error must name the orphaned file id; got %v", err)
	}
}

// The retention sweep is idempotent: deleting a file that is already gone is
// success, not an error to retry forever.
func TestDeleteTreatsMissingFileAsSuccess(t *testing.T) {
	google := newFakeGoogle(t)
	google.deleteStatus = http.StatusNotFound

	if err := google.client().Delete(context.Background(), "file-abc"); err != nil {
		t.Fatalf("a 404 must count as deleted; got %v", err)
	}
}

func TestDeleteSurfacesRealFailures(t *testing.T) {
	google := newFakeGoogle(t)
	google.deleteStatus = http.StatusInternalServerError

	err := google.client().Delete(context.Background(), "file-abc")
	if err == nil {
		t.Fatal("a 500 must be reported")
	}
	if !strings.Contains(err.Error(), "file-abc") {
		t.Errorf("the error must name the file; got %v", err)
	}
}

// The access token is cached: the sweep can delete a batch of files without
// re-authenticating for each one.
func TestAccessTokenIsReusedWhileValid(t *testing.T) {
	google := newFakeGoogle(t)
	client := google.client()
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	for i := 0; i < 3; i++ {
		if _, _, err := client.Publish(context.Background(), path, "schedule"); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if google.tokenRequests != 1 {
		t.Errorf("refreshed the token %d times, want 1", google.tokenRequests)
	}
}

// A token that expires immediately must be refreshed rather than reused, and
// the safety margin means "expires in 30s" already counts as expired.
func TestAccessTokenRefreshesBeforeExpiry(t *testing.T) {
	google := newFakeGoogle(t)
	google.accessTokenBody = `{"access_token":"access-123","expires_in":30}`
	client := google.client()
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	for i := 0; i < 2; i++ {
		if _, _, err := client.Publish(context.Background(), path, "schedule"); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if google.tokenRequests != 2 {
		t.Errorf("refreshed the token %d times, want 2 (a token inside the margin is not reused)", google.tokenRequests)
	}
}

// A revoked refresh token is the likeliest unattended failure. It must surface
// as a clear error and must not upload anything.
func TestPublishFailsClearlyWhenTheRefreshTokenIsRejected(t *testing.T) {
	google := newFakeGoogle(t)
	google.tokenStatus = http.StatusBadRequest
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	_, _, err := google.client().Publish(context.Background(), path, "schedule")
	if err == nil {
		t.Fatal("expected a token error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the error must carry Google's reason; got %v", err)
	}
	if google.uploadRequests != 0 {
		t.Error("nothing may be uploaded without a token")
	}
}

func TestPublishSurfacesUploadFailure(t *testing.T) {
	google := newFakeGoogle(t)
	google.uploadStatus = http.StatusForbidden
	path := writeTempFile(t, "schedule.xlsx", "xlsx bytes")

	_, _, err := google.client().Publish(context.Background(), path, "schedule")
	if err == nil {
		t.Fatal("expected an upload error")
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("the error must carry Google's reason; got %v", err)
	}
}
