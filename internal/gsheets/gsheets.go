// Package gsheets publishes a spreadsheet file to Google Drive as a native
// Google Sheet and shares it by link. It is the real implementation behind the
// mailgun adapter's Publisher interface.
//
// The Google API is reached with plain net/http rather than the official SDK.
// Three requests are needed — refresh a token, upload a file, create one
// permission — and the SDK would be by far the largest dependency in this
// repository to make them. This mirrors how the Mailgun client is written.
//
// Authentication is an OAuth user refresh token with the drive.file scope, so
// FileMill can only ever see files it created itself, never the rest of the
// operator's Drive.
package gsheets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// sheetMimeType is what an uploaded file is converted to: a native Google
	// Sheet rather than a stored .xlsx.
	sheetMimeType = "application/vnd.google-apps.spreadsheet"
	// xlsxMimeType describes the bytes being uploaded.
	xlsxMimeType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	// tokenMargin refreshes an access token slightly before it expires, so a
	// request is never sent with a token that dies in flight.
	tokenMargin = 60 * time.Second
	// requestTimeout bounds a single Google call, so an unresponsive endpoint
	// can't stall the delivery loop.
	requestTimeout = 60 * time.Second
)

// Client publishes files to one Google account's Drive.
type Client struct {
	clientID, clientSecret, refreshToken string

	// Endpoints, overridable in tests.
	tokenURL, apiBase, uploadBase string

	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New returns a Client authenticating with an OAuth user refresh token.
func New(clientID, clientSecret, refreshToken string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		tokenURL:     "https://oauth2.googleapis.com/token",
		apiBase:      "https://www.googleapis.com",
		uploadBase:   "https://www.googleapis.com/upload",
		http:         &http.Client{Timeout: requestTimeout},
	}
}

// Publish uploads the file at path as a Google Sheet named name, shares it with
// anyone who has the link, and returns the file id and that link.
//
// A file that uploads but cannot be shared is deleted before the error is
// returned: leaving it would strand an unshared, unrecorded copy of the
// sender's data in Drive that the retention sweep never learns about.
func (c *Client) Publish(ctx context.Context, path, name string) (string, string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", "", err
	}
	fileID, link, err := c.upload(ctx, token, path, name)
	if err != nil {
		return "", "", err
	}
	if err := c.share(ctx, token, fileID); err != nil {
		if cleanup := c.Delete(ctx, fileID); cleanup != nil {
			return "", "", fmt.Errorf("share %s: %w (and it could not be cleaned up, so file %s is orphaned in Drive: %v)", name, err, fileID, cleanup)
		}
		return "", "", fmt.Errorf("share %s: %w", name, err)
	}
	return fileID, link, nil
}

// Delete removes a published file. A file that is already gone counts as
// deleted, so the retention sweep can safely repeat itself.
func (c *Client) Delete(ctx context.Context, fileID string) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiBase+"/drive/v3/files/"+url.PathEscape(fileID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(resp, "delete "+fileID)
}

// accessToken returns a valid access token, refreshing it when the cached one
// is near expiry. The refresh token itself is long-lived and never changes
// here; only the short-lived access token is fetched.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires.Add(-tokenMargin)) {
		return c.token, nil
	}

	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"refresh_token": {c.refreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := responseError(resp, "refresh access token"); err != nil {
		// The likeliest unattended failure: a revoked or expired refresh token.
		// Say so plainly — the fix is re-running the consent flow, not a retry.
		return "", err
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("token response carried no access_token")
	}
	c.token = body.AccessToken
	c.expires = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return c.token, nil
}

// upload posts the file as a multipart/related request: a JSON metadata part
// asking Drive to convert it to a Sheet, then the bytes.
//
// This is Drive's simple multipart upload, which is documented for files of
// 5 MB or less. Transformer outputs are spreadsheets far below that; anything
// larger would need the resumable protocol instead.
func (c *Client) upload(ctx context.Context, token, path, name string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Drive wants RFC 2387 related parts, not form-data: the parts carry only a
	// Content-Type, so they are built by hand rather than with CreateFormFile.
	metadata, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return "", "", err
	}
	if err := json.NewEncoder(metadata).Encode(map[string]string{"name": name, "mimeType": sheetMimeType}); err != nil {
		return "", "", err
	}
	media, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {xlsxMimeType}})
	if err != nil {
		return "", "", err
	}
	if _, err := io.Copy(media, file); err != nil {
		return "", "", err
	}
	if err := writer.Close(); err != nil {
		return "", "", err
	}

	endpoint := c.uploadBase + "/drive/v3/files?uploadType=multipart&fields=id%2CwebViewLink"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/related; boundary="+writer.Boundary())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if err := responseError(resp, "upload "+filepath.Base(path)); err != nil {
		return "", "", err
	}
	var created struct {
		ID          string `json:"id"`
		WebViewLink string `json:"webViewLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", "", fmt.Errorf("decode upload response: %w", err)
	}
	if created.ID == "" || created.WebViewLink == "" {
		return "", "", fmt.Errorf("upload response was missing id or webViewLink")
	}
	return created.ID, created.WebViewLink, nil
}

// share grants anyone holding the link edit access. The output is a reformat of
// data the sender emailed in, and the link goes back only to them.
func (c *Client) share(ctx context.Context, token, fileID string) error {
	payload, err := json.Marshal(map[string]string{"type": "anyone", "role": "writer"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/drive/v3/files/"+url.PathEscape(fileID)+"/permissions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return responseError(resp, "create permission")
}

// responseError turns a non-2xx response into an error carrying a bounded slice
// of Google's body, which names the actual cause where a status code alone
// would not.
func responseError(resp *http.Response, what string) error {
	if resp.StatusCode < 300 {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("google: %s returned %s: %s", what, resp.Status, bytes.TrimSpace(detail))
}
